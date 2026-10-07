package hardtest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// Phase 7 of docs/test-plan-s2.md: the processor simulator and the hard tests of the first three "done when" items of
// S2 on Anvil. The rows run in their order; S2-T710 runs last and searches what the phase left. Every scenario ends
// with the one nonce sequence of its operator: next_nonce equals the count on chain, no PLANNED slot, no gap.

// authorizeArgs are the casctl arguments of a USD authorization.
func authorizeArgs(authID, cardRef, amount string, extra ...string) []string {
	return append([]string{"sim", "authorize", "--auth-id", authID, "--card-ref", cardRef, "--amount", amount, "--currency", "USD"}, extra...)
}

// returnsOpen counts the returns of the tenant that are not final.
func (w *world) returnsOpen(t *testing.T) int {
	t.Helper()
	return w.count(t, `SELECT count(*) FROM returns r JOIN authorizations a ON a.id = r.authorization_id
		WHERE a.tenant_id = $1 AND r.status NOT IN ('CONFIRMED', 'NOTHING_TO_RETURN')`, w.tenantID)
}

// S2-T701 — Req: §2.1.1 Processor simulator, PRD §4.2. The three operations through the casctl binary.
func TestT701_SimCommands(t *testing.T) {
	w := newWorld(t, worldOptions{})
	w.fundedCard(t, "card_A")
	w.start(t)

	auth := w.mustSim(t, authorizeArgs("auth-701", "card_A", "5.00", "--merchant-name", "Shop", "--merchant-mcc", "5411")...)
	if len(auth) != 1 || auth[0].Status != 200 || auth[0].field("decision") != "APPROVED" || len(auth[0].field("tx_hash")) != 66 {
		t.Fatalf("authorize: %+v, want one line, 200 APPROVED with a tx_hash", auth)
	}
	ret := w.mustSim(t, "sim", "return", "--auth-id", "auth-701", "--return-id", "rv-701", "--type", "REVERSAL", "--amount", "2")
	if len(ret) != 1 || ret[0].Status != 200 || ret[0].field("status") != "ACCEPTED" || ret[0].field("token_amount") != "2000000" {
		t.Errorf("return: %+v, want one line, 200 ACCEPTED of 2000000", ret)
	}
	get := w.mustSim(t, "sim", "get", "--auth-id", "auth-701")
	if len(get) != 1 || get[0].Status != 200 || get[0].field("auth_id") != "auth-701" || get[0].field("tx_hash") != auth[0].field("tx_hash") {
		t.Errorf("get: %+v", get)
	}

	// Answers of the API are printed with exit 0, a 4xx included.
	unknown := w.casctl(t, "sim", "get", "--auth-id", "auth-701-unknown")
	if l := unknown.lines(t); unknown.code != 0 || len(l) != 1 || l[0].Status != 404 {
		t.Errorf("get of an unknown auth_id: exit %d, %q", unknown.code, unknown.stdout)
	}
	invalid := w.casctl(t, authorizeArgs("auth-701-bad", "card_A", "-1")...)
	if l := invalid.lines(t); invalid.code != 0 || len(l) != 1 || l[0].Status != 422 {
		t.Errorf("invalid amount: exit %d, %q", invalid.code, invalid.stdout)
	}
	// A transport error is not zero.
	w.ca.stop(t)
	if down := w.casctl(t, "sim", "get", "--auth-id", "auth-701"); down.code == 0 || down.stdout != "" {
		t.Errorf("card-auth down: exit %d, stdout %q", down.code, down.stdout)
	}
	for _, args := range [][]string{{"--help"}, {"sim", "--help"}, {"sim", "authorize", "--help"}} {
		if r := w.casctl(t, args...); r.code != 0 || strings.Contains(r.stdout+r.stderr, w.pair.Password) {
			t.Errorf("%v: exit %d or the password printed", args, r.code)
		}
	}
	if n := w.trapHits.Load(); n != 0 {
		t.Errorf("casctl sim opened %d connections to CASCTL_DATABASE_URL", n)
	}

	w.start(t)
	w.settle(t, 30*time.Second, "the return confirmed", func() bool { return w.returnsOpen(t) == 0 })
	w.noNonceGap(t)
}

// S2-T702 — Req: Done-when 1, FR-3. One auth_id 100 times, 10 in parallel, through casctl.
func TestT702_ReplaySameProcess(t *testing.T) {
	w := newWorld(t, worldOptions{})
	w.fundedCard(t, "card_A")
	w.start(t)

	r := w.casctl(t, authorizeArgs("auth-702", "card_A", "3", "--repeat", "100", "--parallel", "10")...)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	out := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(out) != 100 {
		t.Fatalf("%d answers, want 100", len(out))
	}
	for i, l := range out {
		if l != out[0] {
			t.Fatalf("answer %d differs from the first:\n%s\n%s", i, l, out[0])
		}
	}
	if first := r.lines(t)[0]; first.Status != 200 || first.field("decision") != "APPROVED" {
		t.Errorf("answer %s, want 200 APPROVED", out[0])
	}
	if n := w.debited(t, "auth-702"); n != 1 {
		t.Errorf("%d Debited events, want 1", n)
	}
	w.noNonceGap(t)
}

// S2-T703 — Req: Done-when 1, FR-16, D-21, UC-3 row 11. Replay across a SIGKILL of card-auth.
func TestT703_ReplayAcrossRestart(t *testing.T) {
	t.Run("a: killed after the send, before the answer", func(t *testing.T) {
		w := newWorld(t, worldOptions{})
		w.fundedCard(t, "card_A")
		w.start(t)
		// Mining is held: the debit is sent, not mined, when card-auth is killed.
		w.chain.SetAutomine(t, false)
		args := authorizeArgs("auth-703a", "card_A", "4")
		first := w.casctlCmd(args...)
		out := &syncBuffer{}
		collect(out)
		first.Stdout, first.Stderr = out, out
		if err := first.Start(); err != nil {
			t.Fatal(err)
		}
		w.waitPending(t, 3*time.Second)
		w.ca.kill()
		if err := first.Wait(); err == nil {
			t.Errorf("the first casctl got an answer from a killed card-auth: %s", out.String())
		}
		if strings.Contains(out.String(), "APPROVED") {
			t.Errorf("an approval before the kill: %s", out.String())
		}

		w.start(t)
		w.chain.Mine(t)
		w.chain.SetAutomine(t, true)
		for i := range 2 {
			a := w.mustSim(t, args...)
			if len(a) != 1 || a[0].Status != 200 || a[0].field("decision") != "DECLINED" || a[0].field("decline_reason") != "TIMEOUT" {
				t.Errorf("repeat %d: %+v, want DECLINED / TIMEOUT", i+1, a)
			}
		}
		final := ""
		w.settle(t, 60*time.Second, "auth-703a final", func() bool {
			final = w.status(t, "auth-703a")
			return final == "LATE_DEBIT_REFUNDED" || final == "DECLINED"
		})
		n := w.debited(t, "auth-703a")
		switch {
		case final == "LATE_DEBIT_REFUNDED" && n == 1:
			if c := w.count(t, `SELECT count(*) FROM returns r JOIN authorizations a ON a.id = r.authorization_id
				WHERE a.tenant_id = $1 AND a.auth_id = 'auth-703a' AND r.type = 'LATE_DEBIT' AND r.status = 'CONFIRMED'`,
				w.tenantID); c != 1 {
				t.Errorf("%d confirmed LATE_DEBIT returns, want 1", c)
			}
		case final == "DECLINED" && n == 0:
			t.Log("the debit expired unmined: DECLINED / TIMEOUT")
		default:
			t.Errorf("final %s with %d Debited events", final, n)
		}
		t.Logf("auth-703a ended %s with %d Debited event(s)", final, n)
		w.noNonceGap(t)
	})

	t.Run("b: killed after APPROVED", func(t *testing.T) {
		w := newWorld(t, worldOptions{})
		w.fundedCard(t, "card_A")
		w.start(t)
		args := authorizeArgs("auth-703b", "card_A", "4")
		first := w.casctl(t, args...)
		if l := first.lines(t); first.code != 0 || len(l) != 1 || l[0].field("decision") != "APPROVED" {
			t.Fatalf("first: exit %d, %q", first.code, first.stdout)
		}
		w.ca.kill()
		w.start(t)
		again := w.casctl(t, args...)
		if again.code != 0 || again.stdout != first.stdout {
			t.Errorf("the repeat after the restart:\n%s\nwant the stored answer:\n%s", again.stdout, first.stdout)
		}
		if n := w.debited(t, "auth-703b"); n != 1 {
			t.Errorf("%d Debited events, want 1", n)
		}
		w.noNonceGap(t)
	})
}

// S2-T704 — Req: Done-when 1, FR-10. 10 cards, 5 auth_ids per card, every request sent twice at once, all at once.
func TestT704_Concurrency(t *testing.T) {
	w := newWorld(t, worldOptions{})
	cards := make([]string, 10)
	for i := range cards {
		cards[i] = "card_" + string(rune('A'+i))
		w.fundedCard(t, cards[i])
	}
	w.start(t)

	type run struct {
		authID, card string
		r            cliResult
	}
	var runs []*run
	for _, card := range cards {
		for k := range 5 {
			runs = append(runs, &run{authID: "auth-704-" + card + "-" + string(rune('0'+k)), card: card})
		}
	}
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.r = w.casctl(t, authorizeArgs(r.authID, r.card, "1", "--repeat", "2", "--parallel", "2")...)
		}()
	}
	wg.Wait()

	for _, r := range runs {
		lines := r.r.lines(t)
		if r.r.code != 0 || len(lines) != 2 {
			t.Errorf("%s: exit %d, %d answers: %s", r.authID, r.r.code, len(lines), r.r.stderr)
			continue
		}
		for _, l := range lines {
			if l.Status == 409 {
				t.Errorf("%s: 409", r.authID)
			}
			if l.field("decision") != "APPROVED" {
				t.Errorf("%s: %s", r.authID, l.Body)
			}
		}
		if n := w.debited(t, r.authID); n != 1 {
			t.Errorf("%s: %d Debited events, want 1", r.authID, n)
		}
	}

	// Per card the debits are in sequence: the decision of one is stored before the next is submitted.
	for _, card := range cards {
		rows, err := w.owner.Query(ctx, `SELECT
				(SELECT e.id FROM authorization_events e WHERE e.authorization_id = a.id AND e.to_status = 'DEBIT_SUBMITTED'),
				(SELECT e.id FROM authorization_events e WHERE e.authorization_id = a.id AND e.from_status = 'DEBIT_SUBMITTED')
			FROM authorizations a JOIN cards c ON c.id = a.card_id
			WHERE a.tenant_id = $1 AND c.card_ref = $2 ORDER BY 1`, w.tenantID, card)
		if err != nil {
			t.Fatal(err)
		}
		var prevDecided int64 = -1
		n := 0
		for rows.Next() {
			var submitted, decided int64
			if err := rows.Scan(&submitted, &decided); err != nil {
				t.Fatal(err)
			}
			if submitted < prevDecided {
				t.Errorf("%s: a debit was submitted (event %d) before the previous one was decided (event %d)", card, submitted, prevDecided)
			}
			prevDecided = decided
			n++
		}
		rows.Close()
		if n != 5 {
			t.Errorf("%s: %d authorizations, want 5", card, n)
		}
	}
	op := w.chain.Operator.Bytes()
	if total, distinct := w.count(t, `SELECT count(*) FROM operator_txs WHERE operator_address = $1`, op),
		w.count(t, `SELECT count(DISTINCT nonce) FROM operator_txs WHERE operator_address = $1`, op); total != 50 || distinct != 50 {
		t.Errorf("%d operator transactions with %d distinct nonces, want 50 and 50", total, distinct)
	}
	w.noNonceGap(t)
}

// S2-T705 — Req: Done-when 2, FR-2. Mining is delayed by 800 ms after the debit reaches the pool; in each of 20 runs
// the answer comes after the block with the debit was mined, which is the earliest time a receipt exists.
func TestT705_ApproveOnlyAfterTheSignal(t *testing.T) {
	w := newWorld(t, worldOptions{})
	w.fundedCard(t, "card_A")
	w.start(t)
	w.chain.SetAutomine(t, false)

	for i := range 20 {
		authID := "auth-705-" + string(rune('a'+i))
		type result struct {
			body map[string]any
			at   time.Time
		}
		done := make(chan result, 1)
		go func() {
			b, at := w.authorizeHTTP(t, authID, "card_A", "1")
			done <- result{b, at}
		}()
		seen := w.waitPending(t, 2*time.Second)
		time.Sleep(time.Until(seen.Add(800 * time.Millisecond)))
		mineStart := time.Now()
		w.chain.Mine(t)
		mined := time.Now()
		r := <-done
		t.Logf("run %2d: debit in the pool %s, block mined %s–%s, answer %s (%s after the mining began)", i+1,
			seen.Format("15:04:05.000"), mineStart.Format("15:04:05.000"), mined.Format("15:04:05.000"),
			r.at.Format("15:04:05.000"), r.at.Sub(mineStart).Round(time.Millisecond))
		if r.body["decision"] != "APPROVED" {
			t.Errorf("run %d: %v, want APPROVED", i+1, r.body)
		}
		if !r.at.After(mineStart) {
			t.Errorf("run %d: the answer %s came before the block was mined %s", i+1, r.at, mineStart)
		}
	}
	w.chain.SetAutomine(t, true)
	w.noNonceGap(t)
}

// S2-T706 — Req: Done-when 3, FR-17. Mining is delayed by 3 s: the deadline answers DECLINED / TIMEOUT, the debit
// lands, and the LATE_DEBIT return confirms without any action; the test only mines blocks.
func TestT706_LateDebitRefunded(t *testing.T) {
	w := newWorld(t, worldOptions{})
	wallet := w.fundedCard(t, "card_A")
	w.start(t)
	before := w.balance(t, wallet)
	w.chain.SetAutomine(t, false)

	type result struct {
		body map[string]any
		at   time.Time
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		b, at := w.authorizeHTTP(t, "auth-706", "card_A", "6")
		done <- result{b, at}
	}()
	seen := w.waitPending(t, 2*time.Second)
	r := <-done
	if r.body["decision"] != "DECLINED" || r.body["decline_reason"] != "TIMEOUT" || r.body["status"] != "TIMED_OUT" {
		t.Errorf("answer %v, want DECLINED / TIMEOUT, status TIMED_OUT", r.body)
	}
	if d := r.at.Sub(start); d > 3*time.Second {
		t.Errorf("answered after %s", d)
	}
	time.Sleep(time.Until(seen.Add(3 * time.Second)))
	w.chain.Mine(t)
	w.chain.SetAutomine(t, true)
	if n := w.debited(t, "auth-706"); n != 1 {
		t.Fatalf("%d Debited events after the late mining, want 1\n%s", n, w.ca.logs.String())
	}
	w.settle(t, 60*time.Second, "LATE_DEBIT_REFUNDED", func() bool { return w.status(t, "auth-706") == "LATE_DEBIT_REFUNDED" })
	if c := w.count(t, `SELECT count(*) FROM returns r JOIN authorizations a ON a.id = r.authorization_id
		WHERE a.tenant_id = $1 AND a.auth_id = 'auth-706' AND r.type = 'LATE_DEBIT' AND r.status = 'CONFIRMED'`, w.tenantID); c != 1 {
		t.Errorf("%d confirmed LATE_DEBIT returns, want 1", c)
	}
	if after := w.balance(t, wallet); after.Cmp(before) != 0 {
		t.Errorf("the wallet holds %s, %s before: not refunded in full", after, before)
	}
	if !strings.Contains(w.metrics(t), "late_debits_total 1") {
		t.Error("late_debits_total is not 1")
	}
	w.noNonceGap(t)
}

// cast runs `cast send` from an unlocked or impersonated account of Anvil.
func (w *world) cast(t *testing.T, from, to common.Address, sig string, args ...string) {
	t.Helper()
	path, err := exec.LookPath("cast")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("cast is not on PATH")
		}
		t.Skip("cast is not on PATH")
	}
	cmd := exec.Command(path, append([]string{"send", "--rpc-url", w.chain.RPCURL, "--unlocked", "--from", from.Hex(), to.Hex(), sig}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cast send %s: %v\n%s", sig, err, out)
	}
}

// S2-T707 — Req: PRD §4.2, the S1 scenario. A fresh deployment; the wallet mints and approves, ADMIN sets the limit
// and the treasury approves the refund, all with cast; then authorize 25.40, return 10.00 and finality.
func TestT707_FullCycle(t *testing.T) {
	w := newWorld(t, worldOptions{noRefundAllowance: true})
	c := w.chain
	wallet := c.NewWallet(t)
	w.cast(t, wallet, c.Token, "mint(address,uint256)", wallet.Hex(), "100000000")
	w.cast(t, wallet, c.Token, "approve(address,uint256)", c.Controller.Hex(), "100000000")
	w.cast(t, c.Admin, c.Controller, "setDailyLimit(address,uint256)", wallet.Hex(), "50000000")
	w.cast(t, c.Treasury, c.Token, "approve(address,uint256)", c.Controller.Hex(), "10000000")
	w.addCard(t, "card_A", wallet)
	w.start(t)

	auth := w.mustSim(t, authorizeArgs("auth-707", "card_A", "25.40")...)
	if len(auth) != 1 || auth[0].field("decision") != "APPROVED" || auth[0].field("token_amount") != "25400000" {
		t.Fatalf("authorize: %+v", auth)
	}
	ret := w.mustSim(t, "sim", "return", "--auth-id", "auth-707", "--return-id", "rv-707", "--type", "REFUND", "--amount", "10.00")
	if len(ret) != 1 || ret[0].Status != 200 || ret[0].field("token_amount") != "10000000" {
		t.Fatalf("return: %+v", ret)
	}
	op := c.Operator.Bytes()
	w.settle(t, 60*time.Second, "every row final", func() bool {
		return w.status(t, "auth-707") == "DEBIT_CONFIRMED" && w.returnsOpen(t) == 0 &&
			w.count(t, `SELECT count(*) FROM operator_txs WHERE operator_address = $1 AND status <> 'CONFIRMED'`, op) == 0
	})
	for addr, want := range map[common.Address]string{wallet: "84600000", c.Treasury: "15400000", c.Controller: "0"} {
		if got := w.balance(t, addr).String(); got != want {
			t.Errorf("balance of %s: %s, want %s", addr.Hex(), got, want)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM operator_txs WHERE operator_address = $1 AND status = 'CONFIRMED'`, op); n != 2 {
		t.Errorf("%d CONFIRMED operator transactions, want 2: the debit and the refund", n)
	}
	w.noNonceGap(t)
}

// cardAuthMetrics are the ten metrics of card-auth in SRS — Card Spend §2.5.1.
var cardAuthMetrics = []string{
	"auth_decision_seconds", "auth_decisions_total", "late_debits_total", "debits_lost_total", "returns_not_confirmed",
	"operator_tx_pending_seconds", "operator_gas_balance", "chain_listener_connected", "inclusion_signals_total",
	"treasury_refund_capacity",
}

// S2-T708 — Req: §2.5.1. After an approval, a return and its finality, every metric of card-auth is on /metrics.
func TestT708_AllMetrics(t *testing.T) {
	w := newWorld(t, worldOptions{})
	w.fundedCard(t, "card_A")
	w.start(t)
	w.mustSim(t, authorizeArgs("auth-708", "card_A", "2")...)
	w.mustSim(t, "sim", "return", "--auth-id", "auth-708", "--return-id", "rv-708", "--type", "REVERSAL")
	w.settle(t, 30*time.Second, "the return confirmed", func() bool { return w.returnsOpen(t) == 0 })
	time.Sleep(500 * time.Millisecond) // one tracker cycle updates the gauges
	m := w.metrics(t)
	for _, name := range cardAuthMetrics {
		if !strings.Contains(m, "\n# TYPE "+name+" ") {
			t.Errorf("/metrics has no %s", name)
		}
	}
	for _, sample := range []string{`auth_decisions_total{decision="APPROVED",reason=""} 1`, "auth_decision_seconds_count 1",
		`inclusion_signals_total{source="polling"} 1`, "returns_not_confirmed 0", "chain_listener_connected 0"} {
		if !strings.Contains(m, sample) {
			t.Errorf("/metrics has no %s", sample)
		}
	}
	w.noNonceGap(t)
}

// S2-T710 — Req: §3.2 Security, handoff §4. Last row of the phase: it adds one more scenario, then searches the
// whole database, every log of card-auth and every output of casctl of the phase for the operator keys, the Basic
// passwords and the database URLs. Only the SHA-256 of each password is in api_credentials.
func TestT710_Secrets(t *testing.T) {
	w := newWorld(t, worldOptions{})
	w.fundedCard(t, "card_A")
	w.start(t)
	w.mustSim(t, authorizeArgs("auth-710", "card_A", "1")...)
	w.mustSim(t, "sim", "return", "--auth-id", "auth-710", "--return-id", "rv-710", "--type", "REFUND")
	w.mustSim(t, "sim", "get", "--auth-id", "auth-710")
	w.settle(t, 30*time.Second, "the return confirmed", func() bool { return w.returnsOpen(t) == 0 })
	w.ca.stop(t)
	w.noNonceGap(t)

	collected.mu.Lock()
	secrets := append([]string(nil), collected.secrets...)
	outputs := append([]*syncBuffer(nil), collected.outputs...)
	pairs := append(collected.pairs[:0:0], collected.pairs...)
	collected.mu.Unlock()
	for _, u := range []string{testdb.URL(t), testdb.ServerURL(t), testdb.CardAuthURL(t)} {
		secrets = append(secrets, u)
		if parsed, err := url.Parse(u); err == nil {
			if p, ok := parsed.User.Password(); ok {
				secrets = append(secrets, p)
			}
		}
	}

	texts := map[string]string{"database": dbText(t, w.owner)}
	for i, b := range outputs {
		texts[fmt.Sprintf("output %03d", i)] = b.String()
	}
	for _, name := range sortedKeys(texts) {
		for _, s := range secrets {
			if len(s) >= 6 && strings.Contains(texts[name], s) {
				t.Errorf("%s contains a secret of the phase", name)
			}
		}
	}

	for _, p := range pairs {
		raw, err := hex.DecodeString(p.Password)
		if err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(raw)
		var hash []byte
		if err := w.owner.QueryRow(ctx, `SELECT secret_hash FROM api_credentials WHERE key_id = $1`, p.Username).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if string(hash) != string(want[:]) {
			t.Errorf("api_credentials of %s does not hold the SHA-256 of the password", p.Username)
		}
	}
	t.Logf("searched %d texts for %d secrets of %d worlds", len(texts), len(secrets), len(pairs))
}
