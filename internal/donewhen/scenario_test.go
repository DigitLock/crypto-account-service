package donewhen

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// world is the common precondition of docs/test-plan-s3.md §2 with the binaries: Anvil with the contracts, the source
// anvil set to that deployment with casctl, tenant A with its token, processor pair, wallet connection W and card
// card_A, tenant cas-platform with the treasury connection T named by casctl source set-treasury, server and
// card-auth running; then the operations of T701, synced.
type world struct {
	chain *testchain.Chain
	owner *pgxpool.Pool

	server, cardAuth *process
	serverHealth     int

	casctlEnv []string // owner role: tenants, tokens, sources
	simEnv    []string // processor API of card-auth with A's pair

	tenantA, platform uuid.UUID
	wallet            common.Address
	w, t              uuid.UUID // the wallet and the treasury connections

	refundBlock  uint64 // block of the refund of T701
	snapshot     string // evm_snapshot taken after T701 synced, for T702
	snapshotHead uint64
}

// Test values of sources.config, written as the test's own values (SRS — EVM Connector §3.1): short intervals, so
// the scenario takes seconds, and a budget no test run reaches.
const testIntervals = `{"sync_interval": {"balances": "1s", "logs": "1s"}, "completeness_interval": "1s", "rpc_rate_limit": 1000}`

// shared is the world of the package; built by the first row that runs.
var shared struct {
	mu      sync.Mutex
	w       *world
	tried   bool
	skipped bool
}

// scene returns the shared world, built on the first call. A row run after a build that failed fails; after a build
// that skipped (no test database, no Anvil) it skips.
func scene(t *testing.T) *world {
	t.Helper()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.w != nil {
		return shared.w
	}
	if shared.tried {
		if shared.skipped {
			t.Skip("the world of the scenario was skipped")
		}
		t.Fatal("the world of the scenario did not build: see the first row that ran")
	}
	shared.tried = true
	built := false
	defer func() {
		if !built {
			shared.skipped = t.Skipped()
		}
	}()
	w := build(keeper{t})
	built = true
	shared.w = w
	return w
}

// build sets up the world as an operator would, then runs the operations of T701 and waits until server synced them.
func build(k keeper) *world {
	k.Helper()
	start := time.Now()
	w := &world{owner: testdb.Open(k)}
	testdb.Clean(k)
	c := testchain.Start(k)
	w.chain = c

	// The source anvil: the values of this deployment with casctl (S3 D-17, D-27), the test intervals with SQL. The
	// config of the seeded source is restored at the end.
	var saved []byte
	if err := w.owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&saved); err != nil {
		k.Fatal(err)
	}
	k.Cleanup(func() {
		_, _ = w.owner.Exec(ctx, `UPDATE sources SET config = $1 WHERE code = 'anvil'`, saved)
	})
	w.casctlEnv = []string{"CASCTL_DATABASE_URL=" + testdb.URL(k)}
	runCasctl(k, w.casctlEnv, "source", "set", "anvil", "controller_address="+c.Controller.Hex())
	runCasctl(k, w.casctlEnv, "source", "set", "anvil", "backfill_floor="+strconv.FormatUint(deploymentBlock(k, c, c.Token), 10))
	runCasctl(k, w.casctlEnv, "source", "set", "anvil", "token_address="+c.Token.Hex())
	if _, err := w.owner.Exec(ctx, `UPDATE sources SET config = config || $1::jsonb WHERE code = 'anvil'`, testIntervals); err != nil {
		k.Fatal(err)
	}
	var mode string
	var confirmations int
	if err := w.owner.QueryRow(ctx, `SELECT config ->> 'finality_mode', (config ->> 'finality_confirmations')::int
		FROM sources WHERE code = 'anvil'`).Scan(&mode, &confirmations); err != nil || mode != "confirmations" || confirmations != 10 {
		k.Fatalf("finality of anvil: %q %d (%v), want confirmations 10 (test plan §2)", mode, confirmations, err)
	}

	// Tenants, tokens and the processor pair with casctl (S3 D-1).
	runCasctl(k, w.casctlEnv, "tenant", "create", "partner-a")
	tokenA := strings.TrimSpace(runCasctl(k, w.casctlEnv, "token", "issue", "partner-a"))
	pair := strings.TrimSpace(runCasctl(k, w.casctlEnv, "processor", "issue", "partner-a"))
	username, password, ok := strings.Cut(pair, ":")
	if !ok {
		k.Fatal("casctl processor issue printed no username:password")
	}
	runCasctl(k, w.casctlEnv, "tenant", "create", "cas-platform")
	tokenPlatform := strings.TrimSpace(runCasctl(k, w.casctlEnv, "token", "issue", "cas-platform"))
	if err := w.owner.QueryRow(ctx, `SELECT id FROM tenants WHERE name = 'partner-a'`).Scan(&w.tenantA); err != nil {
		k.Fatal(err)
	}
	if err := w.owner.QueryRow(ctx, `SELECT id FROM tenants WHERE name = 'cas-platform'`).Scan(&w.platform); err != nil {
		k.Fatal(err)
	}

	// server on the role cas_server with the endpoint of Anvil.
	grpcPort := freePort(k)
	w.serverHealth = freePort(k)
	w.server = startProcess(k, "server", bin.server, map[string]string{
		"DATABASE_URL":         testdb.ServerURL(k),
		"CAS_MASTER_KEY":       masterKey(k),
		"GRPC_PORT":            strconv.Itoa(grpcPort),
		"HEALTH_HTTP_PORT":     strconv.Itoa(w.serverHealth),
		"EVM_RPC_URL_ANVIL":    c.RPCURL,
		"SYNC_TICK":            "200ms",
		"SYNC_BACKOFF_INITIAL": "1s",
		"SYNC_BACKOFF_MAX":     "2s",
		"SHUTDOWN_TIMEOUT":     "5s",
	}, w.serverHealth)
	api := grpcClient(k, grpcPort)
	connections := casv1.NewConnectionServiceClient(api)

	// The treasury connection T through the API, named with casctl source set-treasury (S3 D-2, D-22, D-32).
	resp, err := connections.CreateConnection(bearer(tokenPlatform), &casv1.CreateConnectionRequest{OwnerRef: "treasury",
		Source: "anvil", Label: "Treasury", Credential: &casv1.CreateConnectionRequest_Wallet{Wallet: &casv1.Wallet{Address: c.Treasury.Hex()}}})
	if err != nil {
		k.Fatalf("CreateConnection of the treasury: %v", err)
	}
	w.t = uuid.MustParse(resp.GetConnection().GetConnectionId())
	runCasctl(k, w.casctlEnv, "source", "set-treasury", "anvil", w.t.String())

	// Tenant A: the wallet connection W and the card through the API.
	w.wallet = c.NewWallet(k)
	resp, err = connections.CreateConnection(bearer(tokenA), &casv1.CreateConnectionRequest{OwnerRef: "owner-a",
		Source: "anvil", Label: "W", Credential: &casv1.CreateConnectionRequest_Wallet{Wallet: &casv1.Wallet{Address: w.wallet.Hex()}}})
	if err != nil {
		k.Fatalf("CreateConnection of W: %v", err)
	}
	w.w = uuid.MustParse(resp.GetConnection().GetConnectionId())
	if _, err := casv1.NewCardServiceClient(api).RegisterCard(bearer(tokenA), &casv1.RegisterCardRequest{
		CardRef: "card_A", OwnerRef: "owner-a", ConnectionId: w.w.String(), DailyLimit: usdc(200).String(),
	}); err != nil {
		k.Fatalf("RegisterCard: %v", err)
	}

	// The chain side of the setup: the refund allowance of the treasury, the wallet limit and allowance of W.
	c.ApproveRefunds(k, usdc(1000))
	c.SetDailyLimit(k, w.wallet, usdc(100))
	c.Approve(k, w.wallet, usdc(100))

	// card-auth as in S2, with the finality rule of the source (S3 D-18).
	caHTTP, caHealth := freePort(k), freePort(k)
	w.cardAuth = startProcess(k, "card-auth", bin.cardAuth, map[string]string{
		"CARD_AUTH_DATABASE_URL":           testdb.CardAuthURL(k),
		"OPERATOR_PRIVATE_KEY":             "0x" + hex.EncodeToString(c.OperatorKey.Value()),
		"CARD_AUTH_CHAIN_ID":               strconv.Itoa(testchain.ChainID),
		"CARD_AUTH_RPC_URL":                c.RPCURL,
		"CARD_AUTH_CONTROLLER_ADDRESS":     c.Controller.Hex(),
		"CARD_AUTH_TOKEN_ADDRESS":          c.Token.Hex(),
		"CARD_AUTH_TOKEN_DECIMALS":         strconv.Itoa(testchain.TokenDecimals),
		"CARD_AUTH_HTTP_PORT":              strconv.Itoa(caHTTP),
		"CARD_AUTH_HEALTH_PORT":            strconv.Itoa(caHealth),
		"CARD_AUTH_SHUTDOWN_TIMEOUT":       "5s",
		"CARD_AUTH_TRACKER_INTERVAL":       "250ms",
		"CARD_AUTH_RECEIPT_POLL_INTERVAL":  "50ms",
		"CARD_AUTH_FINALITY_CONFIRMATIONS": "10",
		"CARD_AUTH_RETURN_RETRY_INTERVAL":  "2s",
	}, caHealth)
	w.simEnv = []string{
		"CASCTL_CARD_AUTH_URL=http://127.0.0.1:" + strconv.Itoa(caHTTP),
		"CASCTL_PROCESSOR_USERNAME=" + username,
		"CASCTL_PROCESSOR_PASSWORD=" + password,
	}
	k.Logf("setup: %s", time.Since(start).Round(time.Millisecond))

	// The operations of T701: mint to W, transfer in, transfer out, a debit by an authorization, a refund by a return.
	start = time.Now()
	x := c.NewWallet(k)
	c.Mint(k, w.wallet, usdc(500))
	c.Mint(k, x, usdc(1000))
	c.Transfer(k, x, w.wallet, usdc(100))
	c.Transfer(k, w.wallet, x, usdc(30))
	a := w.sim(k, "authorize", "--auth-id", "t701-1", "--card-ref", "card_A", "--amount", "20.00", "--currency", "USD")
	if a.Body["decision"] != "APPROVED" || a.Body["token_amount"] != "20000000" {
		k.Fatalf("authorize 20 USD: %v\n%s", a, w.cardAuth.logs.String())
	}
	r := w.sim(k, "return", "--auth-id", "t701-1", "--return-id", "t701-r1", "--type", "REFUND", "--amount", "5.00")
	if r.Body["status"] != "ACCEPTED" || r.Body["token_amount"] != "5000000" {
		k.Fatalf("return 5 USD: %v\n%s", r, w.cardAuth.logs.String())
	}

	// Mine to finality and wait for the runs of server: the debit and the refund final for card-auth, and a checkpoint
	// of W and of T at or above the block of the refund.
	w.settle(k, 90*time.Second, "T701 final and synced", func() bool {
		if w.str(k, `SELECT status FROM authorizations WHERE tenant_id = $1 AND auth_id = 't701-1'`, w.tenantA) != "DEBIT_CONFIRMED" ||
			w.str(k, `SELECT status FROM returns WHERE tenant_id = $1 AND return_id = 't701-r1'`, w.tenantA) != "CONFIRMED" {
			return false
		}
		if w.refundBlock == 0 {
			w.refundBlock, _ = strconv.ParseUint(w.str(k, `SELECT block_number::text FROM operator_txs
				WHERE purpose = 'REFUND' AND status = 'CONFIRMED'`), 10, 64)
		}
		at := int64(w.refundBlock)
		return at > 0 && w.checkpointBlock(k, w.w) >= at && w.checkpointBlock(k, w.t) >= at
	})
	k.Logf("T701 operations final and synced: %s", time.Since(start).Round(time.Millisecond))

	// The state for T702: below the cursors of W and T once they move on.
	c.Call(k, &w.snapshot, "evm_snapshot")
	w.snapshotHead = c.Head(k)
	return w
}

// sim runs casctl sim with A's processor pair and requires exit 0 and HTTP 200.
func (w *world) sim(t testing.TB, args ...string) simAnswer {
	t.Helper()
	out := runCasctl(t, w.simEnv, append([]string{"sim"}, args...)...)
	var a simAnswer
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &a); err != nil || a.Status != 200 {
		t.Fatalf("casctl sim %s: %v, %v", args[0], err, a)
	}
	return a
}

// settle mines a block per tick until done holds.
func (w *world) settle(t testing.TB, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s\nserver:\n%s\ncard-auth:\n%s", what, within, w.server.logs.String(), w.cardAuth.logs.String())
		}
		w.chain.Mine(t)
		time.Sleep(100 * time.Millisecond)
	}
}

// wait polls done without mining.
func (w *world) wait(t testing.TB, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s\nserver:\n%s", what, within, w.server.logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// str returns one value as text, empty when there is no row or it is NULL.
func (w *world) str(t testing.TB, sql string, args ...any) string {
	t.Helper()
	var s *string
	if err := w.owner.QueryRow(ctx, sql, args...).Scan(&s); err != nil || s == nil {
		return ""
	}
	return *s
}

// checkpointBlock returns the block of the checkpoint of a connection, or −1 without one.
func (w *world) checkpointBlock(t testing.TB, conn uuid.UUID) int64 {
	t.Helper()
	n, err := strconv.ParseInt(w.str(t, `SELECT block_number::text FROM balance_checkpoints WHERE connection_id = $1`, conn), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// entries returns the ledger entries of a connection as "TYPE DIRECTION amount", sorted.
func (w *world) entries(t testing.TB, conn uuid.UUID) []string {
	t.Helper()
	rows, err := w.owner.Query(ctx, `SELECT type || ' ' || direction || ' ' || trim_scale(amount)::text || ' ' || asset || ' ' || leg
		FROM ledger_entries WHERE connection_id = $1`, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// logsCursor is the logs cursor of a connection with its stream state.
type logsCursor struct {
	NextBlock uint64 `json:"next_block"`
	LastHash  string `json:"last_hash"`
	raw       string
	mode      string
	lastError string
	failures  int
}

func (w *world) cursor(t testing.TB, conn uuid.UUID) logsCursor {
	t.Helper()
	var c logsCursor
	var lastError *string
	if err := w.owner.QueryRow(ctx, `SELECT cursor::text, mode, last_error, consecutive_failures FROM sync_cursors
		WHERE connection_id = $1 AND stream = 'logs'`, conn).Scan(&c.raw, &c.mode, &lastError, &c.failures); err != nil {
		t.Fatal(err)
	}
	if lastError != nil {
		c.lastError = *lastError
	}
	if err := json.Unmarshal([]byte(c.raw), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// run is a stored reconciliation run.
type run struct {
	toBlock    int64
	totals     map[string]any
	mismatches []map[string]any
}

// newestRun returns the newest run of a tenant on anvil, or false without one.
func (w *world) newestRun(t testing.TB, tenant uuid.UUID) (run, bool) {
	t.Helper()
	var r run
	var totals, mismatches []byte
	err := w.owner.QueryRow(ctx, `SELECT to_block, totals, mismatches FROM reconciliation_runs
		WHERE tenant_id = $1 AND source_id = (SELECT id FROM sources WHERE code = 'anvil')
		ORDER BY created_at DESC, id DESC LIMIT 1`, tenant).Scan(&r.toBlock, &totals, &mismatches)
	if err != nil {
		return run{}, false
	}
	if json.Unmarshal(totals, &r.totals) != nil || json.Unmarshal(mismatches, &r.mismatches) != nil {
		t.Fatalf("a stored run is not JSON: %s %s", totals, mismatches)
	}
	return r, true
}

// totalsText returns the totals of a run as key=value in the order of S3 D-40.
func (r run) totalsText() string {
	var parts []string
	for _, k := range []string{"authorizations_checked", "debits_count", "debits_amount", "returns_checked", "refunds_count", "refunds_amount"} {
		parts = append(parts, k+"="+fmt.Sprint(r.totals[k]))
	}
	return strings.Join(parts, " ")
}

// tokenBalance returns the token balance of addr in base units.
func (w *world) tokenBalance(t testing.TB, addr common.Address) *big.Int {
	t.Helper()
	var out hexutil.Bytes
	w.chain.Call(t, &out, "eth_call", map[string]any{
		"to": w.chain.Token, "data": hexutil.Bytes(testchain.TokenCalldata(t, "balanceOf", addr)),
	}, "latest")
	return new(big.Int).SetBytes(out)
}

// S3-T701 — Req: FR-317, FR-307, FR-308, FR-315; done-when 1. Mint to W, transfer in, transfer out, a debit through
// the processor API of card-auth, a refund through a return; mined to finality and synced by server: the entries of
// W and T each once, the checkpoints of W and T with gap 0, ledger_gap 0 in /metrics of server.
func TestT701_ScriptedScenario(t *testing.T) {
	w := scene(t)

	// W: 500 + 100 − 30 − 20 + 5 = 555. T: 20 − 5 = 15.
	wantW := []string{"CARD_DEBIT OUT 20 USDC SINGLE", "CARD_REFUND IN 5 USDC SINGLE", "DEPOSIT IN 100 USDC SINGLE",
		"DEPOSIT IN 500 USDC SINGLE", "WITHDRAWAL OUT 30 USDC SINGLE"}
	wantT := []string{"CARD_DEBIT IN 20 USDC SINGLE", "CARD_REFUND OUT 5 USDC SINGLE"}
	if got := w.entries(t, w.w); !slices.Equal(got, wantW) {
		t.Errorf("entries of W:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantW, "\n"))
	}
	if got := w.entries(t, w.t); !slices.Equal(got, wantT) {
		t.Errorf("entries of T:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantT, "\n"))
	}
	for _, c := range []struct {
		name    string
		conn    uuid.UUID
		addr    common.Address
		balance string
	}{{"W", w.w, w.wallet, "555"}, {"T", w.t, w.chain.Treasury, "15"}} {
		var asset, balance, total, gap string
		if err := w.owner.QueryRow(ctx, `SELECT asset, trim_scale(balance)::text, trim_scale(ledger_total)::text, trim_scale(gap)::text
			FROM balance_checkpoints WHERE connection_id = $1 AND native_asset = $2`, c.conn, w.chain.Token.Hex()).
			Scan(&asset, &balance, &total, &gap); err != nil {
			t.Fatalf("checkpoint of %s: %v", c.name, err)
		}
		if asset != "USDC" || balance != c.balance || total != c.balance || gap != "0" {
			t.Errorf("checkpoint of %s: %s balance %s, ledger total %s, gap %s; want USDC %s, %s, 0", c.name, asset, balance, total, gap,
				c.balance, c.balance)
		}
		if got := w.tokenBalance(t, c.addr); got.Cmp(usdc(mustInt(t, c.balance))) != 0 {
			t.Errorf("%s holds %s base units on chain, want %s USDC", c.name, got, c.balance)
		}
		if cur := w.cursor(t, c.conn); cur.mode != "INCREMENTAL" || cur.failures != 0 || cur.lastError != "" {
			t.Errorf("logs stream of %s: %s, %d failures, last error %q", c.name, cur.mode, cur.failures, cur.lastError)
		}
	}
	metrics := metricsText(t, w.serverHealth)
	for name, conn := range map[string]uuid.UUID{"W": w.w, "T": w.t} {
		if v, ok := metricValue(metrics, "ledger_gap", map[string]string{"source": "anvil", "connection": conn.String(), "asset": "USDC"}); !ok || v != "0" {
			t.Errorf("ledger_gap of %s = %q (%v), want 0", name, v, ok)
		}
	}
}

func mustInt(t testing.TB, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// S3-T703 — Req: FR-21, FR-22; done-when 3. The worker of server stores runs of A and cas-platform without mismatch;
// a debit with an authId of no tenant, sent with the operator key and made final, gives the next run of cas-platform
// an UNKNOWN_DEBIT and reconciliation_mismatches{source="anvil",type="UNKNOWN_DEBIT"} 1; A's run stays empty.
func TestT703_ReconciliationOfTheScenario(t *testing.T) {
	w := scene(t)
	c := w.chain
	wantA := "authorizations_checked=1 debits_count=1 debits_amount=20000000 returns_checked=1 refunds_count=1 refunds_amount=5000000"
	wantPlatform := "authorizations_checked=0 debits_count=1 debits_amount=20000000 returns_checked=0 refunds_count=1 refunds_amount=5000000"

	// The runs of the synced scenario: at the block of T's cursor, A's with the authorization and its return.
	w.settle(t, 30*time.Second, "runs of A and cas-platform at T's cursor", func() bool {
		cur := w.cursor(t, w.t)
		a, okA := w.newestRun(t, w.tenantA)
		p, okP := w.newestRun(t, w.platform)
		return okA && okP && a.toBlock == int64(cur.NextBlock)-1 && p.toBlock == a.toBlock && a.totalsText() == wantA
	})
	a, _ := w.newestRun(t, w.tenantA)
	p, _ := w.newestRun(t, w.platform)
	if p.totalsText() != wantPlatform {
		t.Errorf("totals of the cas-platform run: %s, want %s", p.totalsText(), wantPlatform)
	}
	if n := w.str(t, `SELECT count(*)::text FROM reconciliation_runs WHERE mismatches <> '[]'::jsonb`); n != "0" {
		t.Errorf("%s runs with a mismatch before the unknown debit", n)
	}
	if n := w.str(t, `SELECT count(DISTINCT tenant_id)::text FROM reconciliation_runs`); n != "2" {
		t.Errorf("runs of %s tenants, want A and cas-platform", n)
	}
	if len(a.mismatches) != 0 || len(p.mismatches) != 0 {
		t.Errorf("mismatches: A %v, cas-platform %v", a.mismatches, p.mismatches)
	}
	if v, ok := metricValue(metricsText(t, w.serverHealth), "reconciliation_mismatches", map[string]string{"source": "anvil", "type": "UNKNOWN_DEBIT"}); !ok || v != "0" {
		t.Errorf("reconciliation_mismatches UNKNOWN_DEBIT = %q (%v), want 0", v, ok)
	}

	// A debit of a wallet of no connection with an authId of no tenant, sent with the operator key.
	v := c.NewWallet(t)
	c.Mint(t, v, usdc(50))
	c.SetDailyLimit(t, v, usdc(50))
	c.Approve(t, v, usdc(50))
	authID, authHex := hexID(t)
	hash := c.Debit(t, v, usdc(7), authID)
	block := blockOf(t, c, hash)

	w.settle(t, 30*time.Second, "a cas-platform run at or above the block of the unknown debit", func() bool {
		p, ok := w.newestRun(t, w.platform)
		return ok && p.toBlock >= int64(block)
	})
	p, _ = w.newestRun(t, w.platform)
	if len(p.mismatches) != 1 {
		t.Fatalf("mismatches of the cas-platform run: %v, want one UNKNOWN_DEBIT", p.mismatches)
	}
	m := p.mismatches[0]
	if m["type"] != "UNKNOWN_DEBIT" || !strings.EqualFold(fmt.Sprint(m["chain_auth_id"]), authHex) ||
		!strings.EqualFold(fmt.Sprint(m["tx_hash"]), hash.Hex()) || m["actual_amount"] != "7000000" || m["auth_id"] != nil {
		t.Errorf("mismatch %v, want UNKNOWN_DEBIT of %s in %s, 7000000, no auth_id", m, authHex, hash.Hex())
	}
	a, ok := w.newestRun(t, w.tenantA)
	if !ok || a.toBlock != p.toBlock || len(a.mismatches) != 0 || a.totalsText() != wantA {
		t.Errorf("A's run at block %d: %v, %s; want at %d without mismatch, %s", a.toBlock, a.mismatches, a.totalsText(), p.toBlock, wantA)
	}
	w.wait(t, 10*time.Second, "reconciliation_mismatches UNKNOWN_DEBIT 1", func() bool {
		v, ok := metricValue(metricsText(t, w.serverHealth), "reconciliation_mismatches", map[string]string{"source": "anvil", "type": "UNKNOWN_DEBIT"})
		return ok && v == "1"
	})
}

// S3-T702 — Req: FR-312; done-when 2. Runs last: it breaks the chain. card-auth stops; the chain returns to the
// state saved after T701, below the logs cursors of W and T; other blocks are mined past the cursors and to finality.
// Both logs streams fail with REORG_BELOW_FINAL; no ledger entry, cursor or checkpoint changes;
// evm_reorg_below_final_total grows; the critical log line carries both hashes.
func TestT702_ReorgBelowTheCursor(t *testing.T) {
	w := scene(t)
	c := w.chain
	if !w.cardAuth.stop() {
		t.Error("card-auth did not stop within 15 s of SIGTERM")
	}

	// The cursors above the saved state, then still: nothing is mined until the revert.
	w.settle(t, 30*time.Second, "logs cursors of W and T above the saved state", func() bool {
		return w.cursor(t, w.w).NextBlock-1 > w.snapshotHead && w.cursor(t, w.t).NextBlock-1 > w.snapshotHead
	})
	w.wait(t, 15*time.Second, "logs cursors of W and T at the final block", func() bool {
		next := c.Head(t) - 10 + 1
		return w.cursor(t, w.w).NextBlock == next && w.cursor(t, w.t).NextBlock == next
	})
	before := map[uuid.UUID]logsCursor{w.w: w.cursor(t, w.w), w.t: w.cursor(t, w.t)}
	ledger := w.str(t, `SELECT string_agg(e::text, E'\n' ORDER BY seq) FROM ledger_entries e`)
	checkpoints := w.str(t, `SELECT string_agg(b::text, E'\n' ORDER BY connection_id, native_asset) FROM balance_checkpoints b`)
	reorgsBefore := reorgCount(t, w)

	// Back to the saved state. A mint of a new wallet of no connection makes the next block differ from the one it
	// replaces: empty blocks could repeat the same timestamps and so the same hashes.
	var reverted bool
	c.Call(t, &reverted, "evm_revert", w.snapshot)
	if !reverted {
		t.Fatal("evm_revert answered false")
	}
	c.Mint(t, c.NewWallet(t), big.NewInt(1))
	top := max(before[w.w].NextBlock, before[w.t].NextBlock) - 1
	if head := c.Head(t); top+10 > head {
		c.MineBlocks(t, top+10-head)
	}
	for id, cur := range before {
		if got := blockHash(t, c, cur.NextBlock-1); strings.EqualFold(got.Hex(), cur.LastHash) {
			t.Fatalf("block %d of %s kept its hash %s after the revert", cur.NextBlock-1, id, cur.LastHash)
		}
	}

	// Both streams fail with REORG_BELOW_FINAL, and the critical line names the stored and the found hash.
	critical := func() []logLine {
		var out []logLine
		for _, l := range logLines(w.server.logs.String()) {
			if l.str("level") == "ERROR" && strings.HasPrefix(l.str("msg"), "critical: REORG_BELOW_FINAL") {
				out = append(out, l)
			}
		}
		return out
	}
	withBoth := func(id uuid.UUID) []logLine {
		var out []logLine
		for _, l := range critical() {
			if l.str("connection_id") == id.String() && l.str("found_hash") != "" {
				out = append(out, l)
			}
		}
		return out
	}
	w.wait(t, 30*time.Second, "REORG_BELOW_FINAL on the logs streams of W and T", func() bool {
		for id := range before {
			if cur := w.cursor(t, id); !strings.Contains(cur.lastError, "REORG_BELOW_FINAL") || len(withBoth(id)) == 0 {
				return false
			}
		}
		return true
	})
	lines := critical()
	reorgsAfter := reorgCount(t, w)

	for id, cur := range before {
		after := w.cursor(t, id)
		if after.raw != cur.raw || after.failures == 0 {
			t.Errorf("logs cursor of %s: %s, %d failures; want %s unchanged and failed", id, after.raw, after.failures, cur.raw)
		}
		found := blockHash(t, c, cur.NextBlock-1).Hex()
		for _, l := range withBoth(id) {
			if !strings.EqualFold(l.str("stored_hash"), cur.LastHash) || !strings.EqualFold(l.str("found_hash"), found) ||
				l["block"] != float64(cur.NextBlock-1) || l.str("source") != "anvil" {
				t.Errorf("critical line of %s: %v; want block %d, stored %s, found %s", id, l, cur.NextBlock-1, cur.LastHash, found)
			}
		}
	}
	if got := w.str(t, `SELECT string_agg(e::text, E'\n' ORDER BY seq) FROM ledger_entries e`); got != ledger {
		t.Errorf("ledger entries changed:\nbefore:\n%s\nafter:\n%s", ledger, got)
	}
	if got := w.str(t, `SELECT string_agg(b::text, E'\n' ORDER BY connection_id, native_asset) FROM balance_checkpoints b`); got != checkpoints {
		t.Errorf("balance_checkpoints changed:\nbefore:\n%s\nafter:\n%s", checkpoints, got)
	}
	// The counter is raised before the line is written: it is at least the lines seen before it was read.
	if reorgsAfter < reorgsBefore+2 || reorgsAfter < reorgsBefore+len(lines) {
		t.Errorf("evm_reorg_below_final_total %d → %d with %d critical lines; want +2 or more, at least one per line",
			reorgsBefore, reorgsAfter, len(lines))
	}
	t.Logf("REORG_BELOW_FINAL: %d critical lines, evm_reorg_below_final_total %d → %d", len(lines), reorgsBefore, reorgsAfter)
}

// reorgCount returns evm_reorg_below_final_total{source="anvil"} of server, 0 without the series.
func reorgCount(t testing.TB, w *world) int {
	t.Helper()
	v, ok := metricValue(metricsText(t, w.serverHealth), "evm_reorg_below_final_total", map[string]string{"source": "anvil"})
	if !ok {
		return 0
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("evm_reorg_below_final_total %q", v)
	}
	return int(n)
}
