package processorapi_test

import (
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/crs/crstest"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	opqueue "github.com/DigitLock/crypto-account-service/internal/operator"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/tracker"
)

// Phase 5 of docs/test-plan-s2.md, the part of st6a: UC-2 and the tracker loop that executes returns, on Anvil.

var refundedTopic = crypto.Keccak256Hash([]byte("Refunded(bytes32,bytes32,address,uint256)"))

// ret sends POST /v1/authorizations/{auth_id}/returns as tenant A.
func (e *env) ret(t *testing.T, authID string, body any) response {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/authorizations/"+authID+"/returns", mustJSON(t, body), basic(e.pairA))
}

func returnReq(returnID, amount string) map[string]any {
	b := map[string]any{"return_id": returnID, "type": "REVERSAL"}
	if amount != "" {
		b["amount"] = amount
	}
	return b
}

func acceptedWith(t *testing.T, r response, tokenAmount string) {
	t.Helper()
	if r.status != http.StatusOK || r.str("status") != "ACCEPTED" || r.str("token_amount") != tokenAmount {
		t.Errorf("answer %d %s, want 200 ACCEPTED with %s", r.status, r.raw, tokenAmount)
	}
}

// returnRow is a returns row.
type returnRow struct {
	Status, TokenAmount, Fiat string
	Attempts                  int32
}

func (e *env) returnRow(t *testing.T, returnID string) returnRow {
	t.Helper()
	var r returnRow
	if err := e.owner.QueryRow(ctx, `SELECT status, token_amount::text, COALESCE(fiat_amount::text, ''), attempts
		FROM returns WHERE return_id = $1`, returnID).Scan(&r.Status, &r.TokenAmount, &r.Fiat, &r.Attempts); err != nil {
		t.Fatalf("return %s: %v", returnID, err)
	}
	return r
}

// onChain returns debited and refunded of authorizations(authId) of the controller.
func (e *env) onChain(t *testing.T, authID string) (debited, refunded *big.Int) {
	t.Helper()
	c, err := bindings.NewCardSpendControllerCaller(e.chain.Controller, e.reader.Endpoint().Client)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Authorizations(nil, decision.ChainAuthID(e.tenantA, authID))
	if err != nil {
		t.Fatal(err)
	}
	return a.Debited, a.Refunded
}

// refundedFor returns the number of Refunded events of the return_ids of tenant A.
func (e *env) refundedFor(t *testing.T, returnIDs ...string) int {
	t.Helper()
	n := 0
	for _, id := range returnIDs {
		var logs []any
		e.chain.Call(t, &logs, "eth_getLogs", map[string]any{
			"address": e.chain.Controller, "fromBlock": "0x0", "toBlock": "latest",
			"topics": []any{refundedTopic, nil, decision.ChainRefundID(e.tenantA, id)},
		})
		n += len(logs)
	}
	return n
}

// cycleUntil runs tracker cycles, without mining, until the return reaches status; Anvil mines a sent transaction a
// moment after eth_sendRawTransaction answers.
func (e *env) cycleUntil(t *testing.T, returnID, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.tracker.Cycle(ctx)
		if got := e.returnRow(t, returnID); got.Status == status {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("%s: %s after 5 s, want %s", returnID, got.Status, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mine mines n empty blocks.
func (e *env) mine(t *testing.T, n int) {
	t.Helper()
	for range n {
		e.chain.Mine(t)
	}
}

// settle runs tracker cycles with blocks on top until every return is CONFIRMED or a limit is reached.
func (e *env) settle(t *testing.T) {
	t.Helper()
	for range 6 {
		e.tracker.Cycle(ctx)
		if e.count(t, `SELECT count(*) FROM returns WHERE status IN ('ACCEPTED', 'SUBMITTED', 'INCLUDED', 'RETRYING')`) == 0 {
			return
		}
		e.mine(t, 2)
	}
}

// noNonceGap checks the one nonce sequence of debits and refunds: next_nonce equals the operator's count on chain,
// and every slot below it has a transaction.
func (e *env) noNonceGap(t *testing.T) {
	t.Helper()
	next := e.count(t, `SELECT next_nonce FROM operator_accounts`)
	if onChain := e.chain.TxCount(t, e.chain.Operator); uint64(next) != onChain {
		t.Errorf("next_nonce %d, the operator's count on chain %d", next, onChain)
	}
	if n := e.count(t, `SELECT count(*) FROM operator_txs WHERE status = 'PLANNED'`); n != 0 {
		t.Errorf("%d PLANNED slots left", n)
	}
	if n := e.count(t, `SELECT count(DISTINCT nonce) FROM operator_txs`); n != next-e.count(t, `SELECT COALESCE(min(nonce), 0) FROM operator_txs`) {
		t.Errorf("%d distinct nonces below next_nonce %d: a gap", n, next)
	}
}

// S2-T501 — Req: UC-2, FR-12, EC-8
func TestT501_FullReversal(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	approvedWith(t, e.authorize(t, authReq("auth-501", "card_A", "25.40", "USD")), "25400000")
	before := e.balance(t, e.wallet)

	acceptedWith(t, e.ret(t, "auth-501", returnReq("rv-501", "")), "25400000")
	e.cycleUntil(t, "rv-501", "INCLUDED")
	if got := e.returnRow(t, "rv-501"); got.Attempts != 1 {
		t.Errorf("INCLUDED after %d attempts, want 1", got.Attempts)
	}
	e.mine(t, 1)
	e.tracker.Cycle(ctx)
	if got := e.returnRow(t, "rv-501"); got.Status != "INCLUDED" {
		t.Errorf("1 block on top of 2: %s, want INCLUDED", got.Status)
	}
	e.mine(t, 1)
	e.tracker.Cycle(ctx)
	if got := e.returnRow(t, "rv-501"); got.Status != "CONFIRMED" || got.TokenAmount != "25400000" {
		t.Errorf("2 blocks on top: %+v, want CONFIRMED", got)
	}
	if n := e.count(t, `SELECT count(*) FROM operator_txs WHERE purpose = 'REFUND' AND status = 'CONFIRMED' AND block_number IS NOT NULL`); n != 1 {
		t.Errorf("%d CONFIRMED REFUND rows with a block", n)
	}
	debited, refunded := e.onChain(t, "auth-501")
	if debited.String() != "25400000" || refunded.String() != "25400000" || e.refundedFor(t, "rv-501") != 1 {
		t.Errorf("on chain debited %s refunded %s, %d Refunded events", debited, refunded, e.refundedFor(t, "rv-501"))
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations WHERE auth_id = 'auth-501' AND returned_amount = 25400000`); n != 1 {
		t.Error("returned_amount is not the refunded value of the contract")
	}
	if back := new(big.Int).Sub(e.balance(t, e.wallet), before); back.String() != "25400000" {
		t.Errorf("the wallet got back %s", back)
	}
	e.noNonceGap(t)
}

// S2-T502, S2-T503 — Req: UC-2 step 5, FR-12, EC-17
func TestT502_PartialReturns(t *testing.T) {
	e := newEnv(t, options{realDebit: true})
	e.crs.Set("EUR", crstest.Answer{RateDecimal: "1.1642000000"})
	approvedWith(t, e.authorize(t, authReq("auth-502", "card_A", "25.40", "EUR")), "29866387")

	acceptedWith(t, e.ret(t, "auth-502", returnReq("rv-502-1", "10.00")), "11758420")

	t.Run("S2-T503 above the remainder", func(t *testing.T) {
		r := e.ret(t, "auth-502", returnReq("rv-503", "15.41"))
		if r.status != http.StatusUnprocessableEntity || r.str("error", "code") != "RETURN_EXCEEDS_DEBIT" {
			t.Errorf("answer %d %s, want 422 RETURN_EXCEEDS_DEBIT", r.status, r.raw)
		}
		if n := e.count(t, `SELECT count(*) FROM returns WHERE return_id = 'rv-503'`); n != 0 {
			t.Error("the refused return was stored")
		}
	})

	acceptedWith(t, e.ret(t, "auth-502", returnReq("rv-502-2", "15.40")), "18107967")
	e.settle(t)
	debited, refunded := e.onChain(t, "auth-502")
	if debited.String() != "29866387" || refunded.String() != "29866387" {
		t.Errorf("on chain debited %s, refunded %s: the total must equal the debit exactly", debited, refunded)
	}
	for _, id := range []string{"rv-502-1", "rv-502-2"} {
		if got := e.returnRow(t, id); got.Status != "CONFIRMED" {
			t.Errorf("%s: %s", id, got.Status)
		}
	}
	if got := e.returnRow(t, "rv-502-1"); got.Fiat != "10.0000" {
		t.Errorf("fiat_amount %s", got.Fiat)
	}
	e.noNonceGap(t)

	t.Run("S2-T521 status query with returns", func(t *testing.T) {
		r := e.get(t, "auth-502")
		returnsBody, _ := r.body["returns"].([]any)
		if len(returnsBody) != 2 || r.str("returned_amount") != "29866387" || r.str("debited_amount") != "29866387" {
			t.Fatalf("status query %s", r.raw)
		}
		for i, want := range []struct{ id, amount string }{{"rv-502-1", "11758420"}, {"rv-502-2", "18107967"}} {
			got := returnsBody[i].(map[string]any)
			if got["return_id"] != want.id || got["token_amount"] != want.amount || got["status"] != "CONFIRMED" ||
				len(got["tx_hash"].(string)) != 66 || got["type"] != "REVERSAL" {
				t.Errorf("returns[%d] = %v", i, got)
			}
		}
	})
}

// S2-T504 — Req: FR-13, EC-9
func TestT504_SameReturnID(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	approvedWith(t, e.authorize(t, authReq("auth-504", "card_A", "10", "USD")), "10000000")
	first := e.ret(t, "auth-504", returnReq("rv-504", "4"))
	acceptedWith(t, first, "4000000")
	e.settle(t)

	again := e.ret(t, "auth-504", map[string]any{"type": "REVERSAL", "amount": "4.0000", "return_id": "rv-504", "note": "x"})
	if again.status != http.StatusOK || again.str("token_amount") != "4000000" || again.str("status") != "CONFIRMED" {
		t.Errorf("repeated: %d %s, want the stored return in its current status", again.status, again.raw)
	}
	e.settle(t)
	if n := e.refundedFor(t, "rv-504"); n != 1 {
		t.Errorf("%d Refunded events, want 1", n)
	}
	for name, c := range map[string]struct {
		authID string
		body   map[string]any
	}{
		"another amount":        {"auth-504", returnReq("rv-504", "5")},
		"another type":          {"auth-504", map[string]any{"return_id": "rv-504", "type": "REFUND", "amount": "4"}},
		"another authorization": {"auth-504-other", returnReq("rv-504", "4")},
	} {
		t.Run(name, func(t *testing.T) {
			r := e.ret(t, c.authID, c.body)
			if r.status != http.StatusConflict || r.str("error", "code") != "RETURN_ID_CONFLICT" {
				t.Errorf("answer %d %s, want 409 RETURN_ID_CONFLICT", r.status, r.raw)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM returns`); n != 1 {
		t.Errorf("%d returns stored", n)
	}
}

// insertAuthorization stores an authorization of tenant A in a status, as the engine would have left it.
func (e *env) insertAuthorization(t *testing.T, authID, status string) {
	t.Helper()
	e.exec(t, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, card_id, request_hash, fiat_amount,
		fiat_currency, token, token_amount, status, decline_reason, received_at)
		VALUES ($1, $2, $3, $4, '\x00', 5, 'USD', 'USDC', 5000000, $5, CASE WHEN $5 IN ('DECLINED', 'TIMED_OUT') THEN 'TIMEOUT' END, now())`,
		e.tenantA, authID, decision.ChainAuthID(e.tenantA, authID).Bytes(), e.cardA, status)
}

// S2-T505 — Req: EC-16
func TestT505_DecisionPending(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	for _, status := range []string{decision.StatusReceived, decision.StatusDebitSubmitted} {
		t.Run(status, func(t *testing.T) {
			id := "auth-505-" + strings.ToLower(status)
			e.insertAuthorization(t, id, status)
			r := e.ret(t, id, returnReq("rv-"+id, ""))
			if r.status != http.StatusConflict || r.str("error", "code") != "AUTHORIZATION_IN_PROGRESS" {
				t.Errorf("answer %d %s, want 409 AUTHORIZATION_IN_PROGRESS", r.status, r.raw)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM returns`); n != 0 {
		t.Errorf("%d returns stored", n)
	}
}

// S2-T506 — Req: UC-2 step 4
func TestT506_NothingToReturn(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	before := e.chain.TxCount(t, e.chain.Operator)
	for _, status := range []string{decision.StatusDeclined, decision.StatusTimedOut, decision.StatusLateDebit,
		decision.StatusLateDebitRefunded, decision.StatusDebitLost} {
		t.Run(status, func(t *testing.T) {
			id := "auth-506-" + strings.ToLower(status)
			e.insertAuthorization(t, id, status)
			r := e.ret(t, id, returnReq("rv-"+id, "1"))
			if r.status != http.StatusOK || r.str("status") != "NOTHING_TO_RETURN" || r.str("token_amount") != "0" {
				t.Errorf("answer %d %s, want 200 NOTHING_TO_RETURN 0", r.status, r.raw)
			}
			if got := e.returnRow(t, "rv-"+id); got.Status != "NOTHING_TO_RETURN" || got.TokenAmount != "0" {
				t.Errorf("row %+v", got)
			}
		})
	}
	e.tracker.Cycle(ctx)
	if n := e.chain.TxCount(t, e.chain.Operator); n != before {
		t.Errorf("the operator sent %d transactions", n-before)
	}
}

// S2-T507, S2-T521 — Req: FR-15, EC-14, §2.1.4
func TestT507_Tombstone(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	r := e.ret(t, "auth-507", returnReq("rv-507", "5"))
	if r.status != http.StatusOK || r.str("status") != "NOTHING_TO_RETURN" || r.str("token_amount") != "0" || r.str("auth_id") != "auth-507" {
		t.Errorf("answer %d %s, want NOTHING_TO_RETURN", r.status, r.raw)
	}
	tomb := e.row(t, e.tenantA, "auth-507")
	if tomb.Status != "DECLINED" || tomb.DeclineReason != "REVERSED_BEFORE_AUTH" || tomb.CardID != nil || tomb.RequestHash != nil ||
		tomb.Amount != "" || tomb.Currency != "" || tomb.TokenAmount != "" || strings.Join(tomb.Events, " ") != ">DECLINED:REVERSED_BEFORE_AUTH" {
		t.Errorf("tombstone %+v", tomb)
	}

	declinedWith(t, e.authorize(t, authReq("auth-507", "card_A", "5", "USD")), decision.StatusDeclined, decision.ReasonReversedBeforeAuth)
	if after := e.row(t, e.tenantA, "auth-507"); after.CardID != nil || len(after.Events) != 1 {
		t.Errorf("the authorization read the card or changed the tombstone: %+v", after)
	}

	t.Run("S2-T521 status query of the tombstone", func(t *testing.T) {
		g := e.get(t, "auth-507")
		if g.str("status") != "DECLINED" || g.str("decline_reason") != "REVERSED_BEFORE_AUTH" || g.str("debited_amount") != "0" ||
			g.str("returned_amount") != "0" {
			t.Errorf("status query %s", g.raw)
		}
		for _, absent := range []string{"amount", "currency", "token", "token_amount", "tx_hash", "quote"} {
			if _, ok := g.body[absent]; ok {
				t.Errorf("%s is present: %s", absent, g.raw)
			}
		}
		rets, _ := g.body["returns"].([]any)
		hist, _ := g.body["history"].([]any)
		if len(rets) != 1 || len(hist) != 1 {
			t.Fatalf("returns %v, history %v", rets, hist)
		}
		if rr := rets[0].(map[string]any); rr["status"] != "NOTHING_TO_RETURN" || rr["token_amount"] != "0" || rr["tx_hash"] != nil {
			t.Errorf("return %v", rr)
		}
	})
}

// S2-T508, S2-T522 — Req: EC-12, FR-14, §2.5.1. The fake clock moves the return_retry_interval of 30 s.
func TestT508_TreasuryCannotFund(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true, noRefundAllowance: true})
	e.chain.ApproveRefunds(t, big.NewInt(0))
	approvedWith(t, e.authorize(t, authReq("auth-508", "card_A", "5", "USD")), "5000000")
	now := time.Now()
	e.clock.Set(now)
	acceptedWith(t, e.ret(t, "auth-508", returnReq("rv-508", "")), "5000000")

	e.cycleUntil(t, "rv-508", "RETRYING")
	if got := e.returnRow(t, "rv-508"); got.Attempts != 1 {
		t.Fatalf("first attempt: %+v, want RETRYING after 1", got)
	}
	if got := e.gauge(t, "returns_not_confirmed"); got != 1 {
		t.Errorf("returns_not_confirmed = %v, want 1", got)
	}
	if got := e.gauge(t, "treasury_refund_capacity"); got != 0 {
		t.Errorf("treasury_refund_capacity = %v, want 0: no allowance", got)
	}
	e.clock.Set(now.Add(29 * time.Second))
	e.tracker.Cycle(ctx)
	if got := e.returnRow(t, "rv-508"); got.Attempts != 1 {
		t.Errorf("retried before return_retry_interval: %+v", got)
	}
	e.clock.Set(now.Add(30 * time.Second))
	e.tracker.Cycle(ctx)
	e.cycleUntil(t, "rv-508", "RETRYING")
	if got := e.returnRow(t, "rv-508"); got.Attempts != 2 {
		t.Fatalf("second attempt: %+v, want RETRYING after 2", got)
	}

	e.chain.ApproveRefunds(t, usdc(t, "1000"))
	e.clock.Set(now.Add(60 * time.Second))
	e.settle(t)
	if got := e.returnRow(t, "rv-508"); got.Status != "CONFIRMED" || got.Attempts != 3 {
		t.Errorf("after the allowance: %+v, want CONFIRMED after 3 attempts", got)
	}
	if got := e.gauge(t, "returns_not_confirmed"); got != 0 {
		t.Errorf("returns_not_confirmed = %v, want 0", got)
	}
	// The chain reads of the metrics run at most once per 60 s (rules of S2 st9b): the next one shows the state after
	// the refund. The treasury holds the 5 USDC of the debit, less the 5 returned; capacity is min(balance, allowance).
	e.clock.Set(now.Add(120 * time.Second))
	e.tracker.Cycle(ctx)
	if got, want := e.gauge(t, "treasury_refund_capacity"), e.balance(t, e.chain.Treasury); got != float64(want.Int64()) {
		t.Errorf("treasury_refund_capacity = %v, want the treasury balance %s", got, want)
	}
	if n := e.count(t, `SELECT count(*) FROM operator_txs WHERE purpose = 'REFUND' AND status = 'REVERTED'`); n != 2 {
		t.Errorf("%d REVERTED refunds, want 2", n)
	}
	e.noNonceGap(t)
}

// gauge returns the value of a gauge without labels.
func (e *env) gauge(t *testing.T, name string) float64 {
	t.Helper()
	families, err := e.metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no metric %s", name)
	return 0
}

// S2-T509 — Req: FR-13. The contract refuses a used refundId on its own.
func TestT509_RefundReplayed(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	approvedWith(t, e.authorize(t, authReq("auth-509", "card_A", "5", "USD")), "5000000")
	acceptedWith(t, e.ret(t, "auth-509", returnReq("rv-509", "")), "5000000")
	e.settle(t)
	walletBefore := e.balance(t, e.wallet)

	var returnRowID uuid.UUID
	if err := e.owner.QueryRow(ctx, `SELECT id FROM returns WHERE return_id = 'rv-509'`).Scan(&returnRowID); err != nil {
		t.Fatal(err)
	}
	abi, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	refundID := decision.ChainRefundID(e.tenantA, "rv-509")
	data, err := abi.Pack("refund", [32]byte(decision.ChainAuthID(e.tenantA, "auth-509")), [32]byte(refundID), big.NewInt(5000000))
	if err != nil {
		t.Fatal(err)
	}

	// The revert reason, simulated with the same call of the operator.
	var out string
	err = e.reader.Endpoint().Client.Client().CallContext(ctx, &out, "eth_call",
		map[string]any{"from": e.chain.Operator, "to": e.chain.Controller, "data": "0x" + hex.EncodeToString(data)}, "latest")
	var dataErr rpc.DataError
	selector := "0x" + hex.EncodeToString(crypto.Keccak256([]byte("RefundAlreadyUsed()"))[:4])
	if !errors.As(err, &dataErr) || !strings.HasPrefix(dataErr.ErrorData().(string), selector) {
		t.Errorf("simulated replay: %v, want the error RefundAlreadyUsed", err)
	}

	// The replay itself, sent through the operator queue with a new nonce, outside the API.
	var slot opqueue.Slot
	if err := pgx.BeginFunc(ctx, e.cardAuth, func(tx pgx.Tx) error {
		slot, err = e.queue.Reserve(ctx, repository.New(tx), opqueue.PurposeRefund, nil, &returnRowID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hash, err := e.queue.Send(ctx, slot, opqueue.Call{Purpose: opqueue.PurposeRefund, To: e.chain.Controller, Data: data,
		Gas: config.DefaultRefundGasLimit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt := waitReceipt(t, e, hash)
	if receipt.Status != 0 {
		t.Error("the replayed refund did not revert")
	}
	if got := e.balance(t, e.wallet); got.Cmp(walletBefore) != 0 || e.refundedFor(t, "rv-509") != 1 {
		t.Errorf("a second transfer: wallet %s → %s, %d Refunded events", walletBefore, got, e.refundedFor(t, "rv-509"))
	}
	if got := e.returnRow(t, "rv-509"); got.Status != "CONFIRMED" || got.Attempts != 1 {
		t.Errorf("the service row changed: %+v", got)
	}
}

func waitReceipt(t *testing.T, e *env, hash common.Hash) *receiptStatus {
	t.Helper()
	var r *receiptStatus
	waitFor(t, func() bool {
		r = nil
		e.chain.Call(t, &r, "eth_getTransactionReceipt", hash)
		return r != nil
	})
	return r
}

type receiptStatus struct {
	Status hexutil.Uint64 `json:"status"`
}

// S2-T519 — Req: FR-16, FR-13. A new tracker stands for the restarted process.
func TestT519_RestartWithOpenReturn(t *testing.T) {
	c := testchain.Start(t)

	t.Run("refund on chain: INCLUDED without a second send", func(t *testing.T) {
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true})
		approvedWith(t, e.authorize(t, authReq("auth-519-a", "card_A", "5", "USD")), "5000000")
		acceptedWith(t, e.ret(t, "auth-519-a", returnReq("rv-519-a", "")), "5000000")
		e.chain.SetAutomine(t, false)
		e.tracker.Cycle(ctx)
		if got := e.returnRow(t, "rv-519-a"); got.Status != "SUBMITTED" {
			t.Fatalf("before the stop: %s", got.Status)
		}
		e.chain.Mine(t) // the refund lands while the process is down
		e.chain.SetAutomine(t, true)
		sent := e.chain.TxCount(t, e.chain.Operator)

		restarted := e.newTracker(t)
		if err := restarted.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.returnRow(t, "rv-519-a"); got.Status != "INCLUDED" || got.Attempts != 1 {
			t.Errorf("after the start: %+v, want INCLUDED, no new attempt", got)
		}
		e.mine(t, 2)
		restarted.Cycle(ctx)
		if got := e.returnRow(t, "rv-519-a"); got.Status != "CONFIRMED" {
			t.Errorf("then %s, want CONFIRMED", got.Status)
		}
		if n := e.chain.TxCount(t, e.chain.Operator); n != sent {
			t.Errorf("the start sent %d transactions", n-sent)
		}
		e.noNonceGap(t)
	})

	t.Run("refund not on chain: sent again in the same slot", func(t *testing.T) {
		p := newRPCProxy(t, c.RPCURL)
		e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true})
		approvedWith(t, e.authorize(t, authReq("auth-519-b", "card_A", "5", "USD")), "5000000")
		acceptedWith(t, e.ret(t, "auth-519-b", returnReq("rv-519-b", "")), "5000000")
		// The process stops after storing the hash and before the transaction reached the network.
		p.mu.Lock()
		p.failMethod = "eth_sendRawTransaction"
		p.mu.Unlock()
		e.tracker.Cycle(ctx)
		p.mu.Lock()
		p.failMethod = ""
		p.mu.Unlock()
		var nonce int64
		var hash []byte
		if err := e.owner.QueryRow(ctx, `SELECT nonce, tx_hash FROM operator_txs WHERE purpose = 'REFUND'`).Scan(&nonce, &hash); err != nil {
			t.Fatal(err)
		}
		if got := e.returnRow(t, "rv-519-b"); got.Status != "SUBMITTED" || e.refundedFor(t, "rv-519-b") != 0 {
			t.Fatalf("before the stop: %+v", got)
		}

		restarted := e.newTracker(t)
		if err := restarted.Start(ctx); err != nil {
			t.Fatal(err)
		}
		e.tracker = restarted
		e.cycleUntil(t, "rv-519-b", "INCLUDED")
		var newNonce int64
		var replaced int
		if err := e.owner.QueryRow(ctx, `SELECT nonce, cardinality(replaced_hashes) FROM operator_txs WHERE purpose = 'REFUND'`).
			Scan(&newNonce, &replaced); err != nil {
			t.Fatal(err)
		}
		if newNonce != nonce || replaced != 1 {
			t.Errorf("slot nonce %d → %d, %d replaced hashes; want the same nonce and the old hash kept", nonce, newNonce, replaced)
		}
		if n := e.refundedFor(t, "rv-519-b"); n != 1 {
			t.Errorf("after the start: %d Refunded events, want 1", n)
		}
		e.noNonceGap(t)
	})
}

// newTracker is a second tracker on the env, as after a restart of card-auth.
func (e *env) newTracker(t *testing.T) *tracker.Tracker {
	t.Helper()
	tr, err := tracker.New(e.cardAuth, e.queue, tracker.Config{
		Controller: e.chain.Controller, Token: e.chain.Token, RefundGasLimit: config.DefaultRefundGasLimit,
		DebitGasLimit: config.DefaultDebitGasLimit, DebitValidity: 4 * time.Second, FeeBumpPercent: 25,
		Interval: time.Second, RetryInterval: 30 * time.Second, FinalityMode: config.FinalityModeConfirmations,
		FinalityConfirmations: 2,
	}, tracker.NewMetrics(prometheus.NewRegistry()), e.log, e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// UC-2 step 5, FR-12: two partial returns at once whose sum exceeds the remainder: exactly one is accepted.
func TestConcurrentReturnsCannotExceedTheDebit(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	approvedWith(t, e.authorize(t, authReq("auth-cc", "card_A", "10", "USD")), "10000000")
	answers := make([]response, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			answers[i] = e.ret(t, "auth-cc", returnReq("rv-cc-"+string(rune('a'+i)), "6"))
		}()
	}
	close(start)
	wg.Wait()
	accepted, refused := 0, 0
	for _, a := range answers {
		switch {
		case a.status == http.StatusOK && a.str("status") == "ACCEPTED":
			accepted++
		case a.status == http.StatusUnprocessableEntity && a.str("error", "code") == "RETURN_EXCEEDS_DEBIT":
			refused++
		}
	}
	if accepted != 1 || refused != 1 {
		t.Errorf("%d accepted, %d refused; want 1 and 1: %s / %s", accepted, refused, answers[0].raw, answers[1].raw)
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations WHERE auth_id = 'auth-cc' AND returned_amount = 6000000`); n != 1 {
		t.Error("returned_amount is not 6000000")
	}
}

// FR-14, UC-3 row 3: the exception wired for st6b. The open returns of an authorization that becomes DEBIT_LOST close
// as NOTHING_TO_RETURN with no tokens, and returned_amount drops by their amounts; a confirmed return stays.
func TestFR14_DebitLostClosesOpenReturns(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	approvedWith(t, e.authorize(t, authReq("auth-fr14", "card_A", "10", "USD")), "10000000")
	acceptedWith(t, e.ret(t, "auth-fr14", returnReq("rv-fr14-1", "3")), "3000000")
	e.settle(t)
	acceptedWith(t, e.ret(t, "auth-fr14", returnReq("rv-fr14-2", "4")), "4000000")
	var id uuid.UUID
	if err := e.owner.QueryRow(ctx, `SELECT id FROM authorizations WHERE auth_id = 'auth-fr14'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var closed int64
	if err := pgx.BeginFunc(ctx, e.cardAuth, func(tx pgx.Tx) error {
		var err error
		closed, err = tracker.CloseOpenReturns(ctx, repository.New(tx), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Errorf("%d returns closed, want 1", closed)
	}
	if got := e.returnRow(t, "rv-fr14-2"); got.Status != "NOTHING_TO_RETURN" || got.TokenAmount != "0" {
		t.Errorf("open return %+v", got)
	}
	if got := e.returnRow(t, "rv-fr14-1"); got.Status != "CONFIRMED" {
		t.Errorf("confirmed return %+v", got)
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations WHERE id = $1 AND returned_amount = 3000000`, id); n != 1 {
		t.Error("returned_amount did not drop to 3000000")
	}
}

// S2-T202 — Req: §2.1.1, D-18. An internal failure of a return is 500 INTERNAL without internal detail, as the
// OpenAPI states.
func TestT202_ReturnInternalFailure(t *testing.T) {
	e := newEnv(t, options{noCRS: true, returnsDB: func(p *pgxpool.Pool) decision.DB {
		return failingDB{Pool: p, match: "name: LockAuthorizationForReturn :one"}
	}})
	r := e.ret(t, "auth-202", returnReq("rv-202", ""))
	if r.status != http.StatusInternalServerError || r.str("error", "code") != "INTERNAL" || strings.Contains(string(r.raw), "injected") {
		t.Errorf("answer %d %s, want 500 INTERNAL without detail", r.status, r.raw)
	}
	if n := e.count(t, `SELECT count(*) FROM returns`); n != 0 {
		t.Errorf("%d returns stored", n)
	}
}
