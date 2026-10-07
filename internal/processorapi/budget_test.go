package processorapi_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/tracker"
)

// The RPC budget of a tracker cycle (SRS — Card Spend UC-3, rules of S2 st9b), counted by the proxy between card-auth
// and Anvil. Finality by the tag finalized: Anvil answers it with the latest block − 64, so the rows of a fresh chain
// are above the final block until 64 blocks are mined on top.

// calls are the JSON-RPC calls of a set of proxied requests, by method; a batch counts each of its calls.
type calls map[string]int

func (c calls) total() int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

func (c calls) String() string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+strconv.Itoa(c[k]))
	}
	return fmt.Sprintf("%d calls (%s)", c.total(), strings.Join(parts, ", "))
}

type rpcCall struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func decodeCalls(t *testing.T, reqs []proxied) []rpcCall {
	t.Helper()
	var out []rpcCall
	for _, r := range reqs {
		var batch []rpcCall
		if json.Unmarshal(r.body, &batch) == nil {
			out = append(out, batch...)
			continue
		}
		var one rpcCall
		if err := json.Unmarshal(r.body, &one); err != nil {
			t.Fatalf("a request that is not JSON-RPC: %s", r.body)
		}
		out = append(out, one)
	}
	return out
}

func countCalls(t *testing.T, reqs []proxied) calls {
	t.Helper()
	c := calls{}
	for _, call := range decodeCalls(t, reqs) {
		c[call.Method]++
	}
	return c
}

// cycleCalls runs one tracker cycle and returns the requests it sent through the proxy.
func (e *env) cycleCalls(t *testing.T, p *rpcProxy) []proxied {
	t.Helper()
	p.take()
	e.tracker.Cycle(ctx)
	return p.take()
}

// budgetEnv is an env whose card-auth reads through a counting proxy, finality by the tag finalized.
func budgetEnv(t *testing.T) (*env, *rpcProxy) {
	t.Helper()
	c := testchain.Start(t)
	p := newRPCProxy(t, c.RPCURL)
	return newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true, finalityTag: true}), p
}

func (e *env) statusCount(t *testing.T, status string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM authorizations WHERE status = $1`, status)
}

func (e *env) anvilMine(t *testing.T, n uint64) {
	t.Helper()
	e.chain.Call(t, nil, "anvil_mine", hexutil.Uint64(n))
}

// logsSince returns the log lines written after mark, the length of the log at that time.
func (e *env) logsSince(mark int) string { return e.logs.String()[mark:] }

// S2 st9b a, b, c: with every row above the final block a cycle costs the same calls for 1 row and for 30; once
// the rows are final, the calls are bounded by their distinct blocks.
func TestTrackerBudget_ApprovedRows(t *testing.T) {
	e, p := budgetEnv(t)
	const rows = 30
	approvedWith(t, e.authorize(t, authReq("budget-1", "card_A", "1", "USD")), "1000000")
	// The first cycle also reads the metrics, then not for 60 s (TestTrackerBudget_IdleMetrics): the clock stays.
	e.clock.Set(time.Now())
	metrics := countCalls(t, e.cycleCalls(t, p))
	t.Logf("first cycle, with the reads of the metrics: %s", metrics)
	one := countCalls(t, e.cycleCalls(t, p))
	for i := 2; i <= rows; i++ {
		approvedWith(t, e.authorize(t, authReq("budget-"+strconv.Itoa(i), "card_A", "1", "USD")), "1000000")
	}
	if n := e.statusCount(t, decision.StatusApproved); n != rows {
		t.Fatalf("%d APPROVED rows, want %d", n, rows)
	}
	all := countCalls(t, e.cycleCalls(t, p))
	again := countCalls(t, e.cycleCalls(t, p))
	t.Logf("one cycle above the final block: 1 row %s; %d rows %s; next cycle %s", one, rows, all, again)
	if all.total() != one.total() || again.total() != one.total() {
		t.Errorf("calls per cycle depend on the rows: 1 row %d, %d rows %d, next cycle %d", one.total(), rows, all.total(), again.total())
	}
	if n := e.statusCount(t, decision.StatusApproved); n != rows {
		t.Errorf("%d APPROVED rows after the cycles above the final block, want %d", n, rows)
	}

	e.anvilMine(t, 70)
	final := countCalls(t, e.cycleCalls(t, p))
	distinct := e.count(t, `SELECT count(DISTINCT block_number) FROM operator_txs WHERE purpose = 'DEBIT'`)
	t.Logf("the cycle that sets %d rows final, %d distinct blocks: %s", rows, distinct, final)
	if n := e.statusCount(t, decision.StatusDebitConfirmed); n != rows {
		t.Errorf("%d DEBIT_CONFIRMED, want %d", n, rows)
	}
	if final.total() > one.total()+distinct {
		t.Errorf("%d calls for %d distinct blocks, want at most %d", final.total(), distinct, one.total()+distinct)
	}
	e.noNonceGap(t)
}

var refundUsedSelector = hexutil.Encode(crypto.Keccak256([]byte("refundUsed(bytes32)"))[:4])

// refundUsedCalls counts the eth_call of refundUsed among the requests.
func refundUsedCalls(t *testing.T, reqs []proxied) int {
	t.Helper()
	n := 0
	for _, c := range decodeCalls(t, reqs) {
		if c.Method == "eth_call" && len(c.Params) > 0 && strings.Contains(string(c.Params[0]), strings.TrimPrefix(refundUsedSelector, "0x")) {
			n++
		}
	}
	return n
}

// S2 st9b b, finding 2 of the live run: an INCLUDED refund above the final block — the final block still below the
// deployment of the controller — makes no refundUsed call, sealed or preconfirmed; it is CONFIRMED once final.
func TestTrackerBudget_IncludedRefundBeforeFinal(t *testing.T) {
	for _, preconfirmed := range []bool{false, true} {
		t.Run("preconfirmed "+strconv.FormatBool(preconfirmed), func(t *testing.T) {
			e, p := budgetEnv(t)
			approvedWith(t, e.authorize(t, authReq("budget-r", "card_A", "5", "USD")), "5000000")
			acceptedWith(t, e.ret(t, "budget-r", returnReq("rv-budget", "2")), "2000000")
			p.mu.Lock()
			p.zeroBlockHash = preconfirmed
			p.mu.Unlock()
			e.cycleUntil(t, "rv-budget", "INCLUDED")
			p.mu.Lock()
			p.zeroBlockHash = false
			p.mu.Unlock()

			mark := len(e.logs.String())
			for range 3 {
				if n := refundUsedCalls(t, e.cycleCalls(t, p)); n != 0 {
					t.Errorf("an INCLUDED refund above the final block: %d refundUsed calls in a cycle, want 0", n)
				}
			}
			if got := e.returnRow(t, "rv-budget"); got.Status != "INCLUDED" {
				t.Errorf("return %s before the final block reaches it, want INCLUDED", got.Status)
			}
			e.anvilMine(t, 70)
			e.cycleUntil(t, "rv-budget", "CONFIRMED")
			if logs := e.logsSince(mark); strings.Contains(logs, "no contract code") || strings.Contains(logs, "following a refund failed") {
				t.Errorf("a refund failure was logged:\n%s", logs)
			}
			e.noNonceGap(t)
		})
	}
}

// S2 st9b d: an endpoint that answers HTTP 429 to everything ends the cycle at its first call, with one WARN line;
// nothing changes, and the next decision and the next cycle work once the endpoint answers again.
func TestTrackerBudget_RateLimitEndsTheCycle(t *testing.T) {
	e, p := budgetEnv(t)
	for i := 1; i <= 3; i++ {
		approvedWith(t, e.authorize(t, authReq("limit-"+strconv.Itoa(i), "card_A", "1", "USD")), "1000000")
	}
	acceptedWith(t, e.ret(t, "limit-1", returnReq("rv-limit", "1")), "1000000")
	e.cycleUntil(t, "rv-limit", "INCLUDED")
	e.anvilMine(t, 70) // every row is due to become final: the cycle would read

	p.set("429", 0)
	mark := len(e.logs.String())
	reqs := e.cycleCalls(t, p)
	logs := e.logsSince(mark)
	p.set("", 0)
	if len(reqs) != 1 {
		t.Errorf("%d requests in a cycle answered 429, want 1: %s", len(reqs), countCalls(t, reqs))
	}
	if n := strings.Count(logs, "\n"); n != 1 || !strings.Contains(logs, `"level":"WARN"`) ||
		!strings.Contains(logs, "tracker: the endpoint answered with a rate limit; the cycle ended") ||
		!strings.Contains(logs, "HTTP 429") {
		t.Errorf("want one WARN line of the rate limit, got %d lines:\n%s", n, logs)
	}
	if n := e.statusCount(t, decision.StatusApproved); n != 3 {
		t.Errorf("%d APPROVED after the limited cycle, want 3: no row moves", n)
	}
	if got := e.returnRow(t, "rv-limit"); got.Status != "INCLUDED" {
		t.Errorf("return %s after the limited cycle, want INCLUDED", got.Status)
	}

	approvedWith(t, e.authorize(t, authReq("limit-after", "card_A", "1", "USD")), "1000000")
	if reqs := e.cycleCalls(t, p); len(reqs) < 2 {
		t.Errorf("the next cycle sent %d requests: it did not run on its schedule", len(reqs))
	}
	if n := e.statusCount(t, decision.StatusDebitConfirmed); n != 3 {
		t.Errorf("%d DEBIT_CONFIRMED after the next cycle, want 3", n)
	}
	if got := e.returnRow(t, "rv-limit"); got.Status != "CONFIRMED" {
		t.Errorf("return %s after the next cycle, want CONFIRMED", got.Status)
	}
	e.anvilMine(t, 70)
	e.cycleUntilStatus(t, "limit-after", decision.StatusDebitConfirmed)
	e.noNonceGap(t)
}

// S2 st9b e: the same failure in many rows of a cycle is logged once, with the number of rows. The final block is
// read once per cycle, so a failed read is shared by every row.
func TestTrackerBudget_SameFailureLoggedOnce(t *testing.T) {
	e, p := budgetEnv(t)
	const rows = 5
	for i := 1; i <= rows; i++ {
		approvedWith(t, e.authorize(t, authReq("fail-"+strconv.Itoa(i), "card_A", "1", "USD")), "1000000")
	}
	p.failing("eth_getBlockByNumber")
	mark := len(e.logs.String())
	reqs := e.cycleCalls(t, p)
	logs := e.logsSince(mark)
	p.failing("")
	if n := countCalls(t, reqs)["eth_getBlockByNumber"]; n != 1 {
		t.Errorf("%d reads of eth_getBlockByNumber, want 1: the final block, shared", n)
	}
	if n := strings.Count(logs, "tracker: an authorization was not moved"); n != 1 || !strings.Contains(logs, `"rows":5`) {
		t.Errorf("want one line for the %d rows with \"rows\":5, got %d:\n%s", rows, n, logs)
	}
	e.anvilMine(t, 70)
	for i := 1; i <= rows; i++ {
		e.cycleUntilStatus(t, "fail-"+strconv.Itoa(i), decision.StatusDebitConfirmed)
	}
	e.noNonceGap(t)
}

// S2 st9b, idle budget (owner's decision of 2026-10-06): with no open row a cycle reads the chain only for the metrics
// operator_gas_balance and treasury_refund_capacity, and those at most once per 60 s; the first cycle reads them.
func TestTrackerBudget_IdleMetrics(t *testing.T) {
	e, p := budgetEnv(t)
	start := time.Now()
	e.clock.Set(start)
	idle := calls{}
	for s := 0; s <= 10; s += 2 {
		e.clock.Set(start.Add(time.Duration(s) * time.Second))
		for m, n := range countCalls(t, e.cycleCalls(t, p)) {
			idle[m] += n
		}
	}
	t.Logf("6 idle cycles over 10 s: %s", idle)
	want := calls{"eth_getBalance": 1, "eth_call": 2}
	if idle.String() != want.String() {
		t.Errorf("6 idle cycles over 10 s: %s, want %s", idle, want)
	}
	e.clock.Set(start.Add(58 * time.Second))
	if c := countCalls(t, e.cycleCalls(t, p)); c.total() != 0 {
		t.Errorf("an idle cycle at 58 s: %s, want none", c)
	}
	e.clock.Set(start.Add(60 * time.Second))
	if c := countCalls(t, e.cycleCalls(t, p)); c.String() != want.String() {
		t.Errorf("the idle cycle at 60 s: %s, want %s", c, want)
	}
	if got := e.gauge(t, "operator_gas_balance"); got <= 0 {
		t.Errorf("operator_gas_balance %v after the reads", got)
	}
	e.noNonceGap(t)
}

// limiting makes the proxy answer every request that carries method with HTTP 429; "" ends it.
func (p *rpcProxy) limiting(method string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limitMethod = method
}

// S2-T526 — Req: UC-2 step 7, UC-3 rules of S2 st9b, FR-14. A rate limit at the fee read of a refund attempt is a rate
// limit of the cycle: one WARN line, the return keeps its status and its attempts, the slot stays PLANNED without a
// hash, and the next cycle sends the refund in that slot.
func TestT526_RateLimitedRefundAttempt(t *testing.T) {
	e, p := budgetEnv(t)
	approvedWith(t, e.authorize(t, authReq("t526", "card_A", "1", "USD")), "1000000")
	acceptedWith(t, e.ret(t, "t526", returnReq("rv-t526", "1")), "1000000")
	before := e.returnRow(t, "rv-t526")
	slots := func() (planned, hashes int) {
		const q = `SELECT count(*) FILTER (WHERE o.status = 'PLANNED'), count(o.tx_hash)
			FROM operator_txs o JOIN returns r ON r.id = o.return_row_id WHERE r.return_id = 'rv-t526'`
		if err := e.owner.QueryRow(ctx, q).Scan(&planned, &hashes); err != nil {
			t.Fatal(err)
		}
		return planned, hashes
	}

	p.limiting("eth_maxPriorityFeePerGas")
	mark := len(e.logs.String())
	e.tracker.Cycle(ctx)
	logs := e.logsSince(mark)
	p.limiting("")
	if n := strings.Count(logs, `"level":"WARN"`) + strings.Count(logs, `"level":"ERROR"`); n != 1 ||
		!strings.Contains(logs, "tracker: the endpoint answered with a rate limit; the cycle ended") {
		t.Errorf("want one WARN line of the rate limit, got %d WARN or ERROR lines:\n%s", n, logs)
	}
	if strings.Contains(logs, "refund not sent") {
		t.Errorf("a second line for the rate limit of the refund:\n%s", logs)
	}
	if got := e.returnRow(t, "rv-t526"); got.Status != before.Status || got.Attempts != before.Attempts {
		t.Errorf("return %s with %d attempts after the limited cycle, want %s with %d: the row does not move",
			got.Status, got.Attempts, before.Status, before.Attempts)
	}
	if planned, hashes := slots(); planned != 1 || hashes != 0 {
		t.Errorf("%d PLANNED slots and %d hashes of the return, want 1 and 0: nothing was sent", planned, hashes)
	}

	e.cycleUntil(t, "rv-t526", "INCLUDED")
	if got := e.returnRow(t, "rv-t526"); got.Attempts != before.Attempts+1 {
		t.Errorf("%d attempts after the send, want %d", got.Attempts, before.Attempts+1)
	}
	if n := e.count(t, `SELECT count(*) FROM operator_txs o JOIN returns r ON r.id = o.return_row_id
		WHERE r.return_id = 'rv-t526'`); n != 1 {
		t.Errorf("%d slots of the return, want 1: the PLANNED slot is used by the send", n)
	}
	e.noNonceGap(t)
}

// S2-T527 — Req: §3.1 rpc_read_timeout, UC-3. Every contract call of the tracker is bounded by rpc_read_timeout, as
// every other read: an endpoint that does not answer an eth_call holds the call 500 ms, not the cycle.
func TestT527_TrackerCallsBoundedByReadTimeout(t *testing.T) {
	e, p := budgetEnv(t)
	p.mu.Lock()
	p.hangMethod, p.hangFor = "eth_call", 3*time.Second
	p.mu.Unlock()
	start := time.Now()
	err := e.tracker.Start(ctx)
	took := time.Since(start)
	p.mu.Lock()
	p.hangMethod = ""
	p.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "treasury(): no answer within 500ms") {
		t.Fatalf("start with an eth_call that does not answer: %v, want treasury(): no answer within 500ms", err)
	}
	if took > 1500*time.Millisecond {
		t.Errorf("start took %s with an eth_call that does not answer: the call is not bounded by rpc_read_timeout", took)
	}
	if err := e.tracker.Start(ctx); err != nil {
		t.Errorf("start once the endpoint answers: %v", err)
	}
}

// S2-T528 — Req: §2.5.1 treasury_refund_capacity, UC-3 rules of S2 st9b, owner's decision of 2026-10-07. A start
// whose treasury() read is rate-limited leaves the address unset; each metric update reads it again, under the rules
// of the cycle, and reports the capacity once the endpoint answers, without a restart.
func TestT528_TreasuryReadAfterLimitedStart(t *testing.T) {
	e, p := budgetEnv(t)
	approvedWith(t, e.authorize(t, authReq("t528", "card_A", "1", "USD")), "1000000")
	want := e.balance(t, e.chain.Treasury)
	if want.Sign() == 0 {
		t.Fatal("the treasury holds nothing: the capacity would not show the read")
	}
	reg := prometheus.NewRegistry()
	tr, err := tracker.New(e.cardAuth, e.queue, tracker.Config{
		Controller: e.chain.Controller, Token: e.chain.Token, RefundGasLimit: config.DefaultRefundGasLimit,
		DebitGasLimit: config.DefaultDebitGasLimit, DebitValidity: 4 * time.Second, FeeBumpPercent: 25,
		Interval: time.Second, RetryInterval: 30 * time.Second, FinalityMode: config.FinalityModeTag,
		FinalityTag: config.FinalityTagFinalized,
	}, tracker.NewMetrics(reg), e.log, e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	capacity := func() float64 {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if f.GetName() == "treasury_refund_capacity" {
				return f.GetMetric()[0].GetGauge().GetValue()
			}
		}
		t.Fatal("no metric treasury_refund_capacity")
		return 0
	}

	start := time.Now()
	e.clock.Set(start)
	p.limiting("eth_call")
	if err := tr.Start(ctx); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("start with eth_call rate-limited: %v, want the rate limit of treasury()", err)
	}
	tr.Cycle(ctx)
	if got := capacity(); got != 0 {
		t.Errorf("treasury_refund_capacity = %v while treasury() is rate-limited, want not reported (0)", got)
	}

	p.limiting("")
	e.clock.Set(start.Add(61 * time.Second)) // the next metric update (metricsChainInterval)
	tr.Cycle(ctx)
	if got := capacity(); got != float64(want.Int64()) {
		t.Errorf("treasury_refund_capacity = %v after the endpoint answers, want %s: read without a restart", got, want)
	}
}
