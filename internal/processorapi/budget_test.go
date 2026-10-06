package processorapi_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
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
