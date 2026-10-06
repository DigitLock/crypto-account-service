package processorapi_test

import (
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	opqueue "github.com/DigitLock/crypto-account-service/internal/operator"
	"github.com/DigitLock/crypto-account-service/internal/signer"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/tracker"
)

// Phase 5 of docs/test-plan-s2.md, the part of st6b: the debit side of UC-3 on Anvil. Every scenario ends with the
// check of the one nonce sequence: next_nonce equals the operator's count on chain, no PLANNED slot, no gap.

// cycleUntilStatus runs tracker cycles until the authorization reaches status.
func (e *env) cycleUntilStatus(t *testing.T, authID, status string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		e.tracker.Cycle(ctx)
		got := e.row(t, e.tenantA, authID).Status
		if got == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s after 8 s of cycles, want %s\n%s", authID, got, status, e.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// debitTx is the newest debit or release slot of an authorization.
func (e *env) debitTx(t *testing.T, authID string) opTx {
	t.Helper()
	txs := e.opTxs(t, authID)
	if len(txs) == 0 {
		t.Fatalf("%s has no operator transaction", authID)
	}
	return txs[len(txs)-1]
}

func (e *env) replacedHashes(t *testing.T, authID string) int {
	t.Helper()
	return e.count(t, `SELECT cardinality(t.replaced_hashes) FROM operator_txs t JOIN authorizations a ON a.id = t.authorization_id
		WHERE a.auth_id = $1 ORDER BY t.nonce DESC LIMIT 1`, authID)
}

func (e *env) snapshot(t *testing.T) string {
	t.Helper()
	var id string
	e.chain.Call(t, &id, "evm_snapshot")
	return id
}

func (e *env) revert(t *testing.T, id string) {
	t.Helper()
	var ok bool
	e.chain.Call(t, &ok, "evm_revert", id)
	if !ok {
		t.Fatal("evm_revert failed")
	}
}

// restartTracker is a new tracker on a new queue with a sound database, as after a restart of card-auth; it becomes
// the tracker of the env.
func (e *env) restartTracker(t *testing.T) {
	t.Helper()
	operator, err := signer.New(e.chain.OperatorKey)
	if err != nil {
		t.Fatal(err)
	}
	e.queue = opqueue.New(e.cardAuth, e.reader, operator, testchain.ChainID, 500*time.Millisecond, e.log, e.clock.Now)
	tr, err := tracker.New(e.cardAuth, e.queue, tracker.Config{
		Controller: e.chain.Controller, Token: e.chain.Token, RefundGasLimit: config.DefaultRefundGasLimit,
		DebitGasLimit: config.DefaultDebitGasLimit, DebitValidity: 4 * time.Second, FeeBumpPercent: 25,
		Interval: time.Second, RetryInterval: 30 * time.Second, FinalityMode: config.FinalityModeConfirmations,
		FinalityConfirmations: 2,
	}, tracker.NewMetrics(prometheus.NewRegistry()), e.log, e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.tracker = tr
}

// S2-T510, S2-T522 — Req: FR-19, UC-3 row 1, §2.5.1
func TestT510_Finality(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true, confirmations: 10})
	approvedWith(t, e.authorize(t, authReq("auth-510", "card_A", "5", "USD")), "5000000")
	e.mine(t, 9)
	e.tracker.Cycle(ctx)
	if got := e.row(t, e.tenantA, "auth-510").Status; got != decision.StatusApproved {
		t.Fatalf("9 blocks on top: %s, want APPROVED", got)
	}
	e.mine(t, 1)
	e.tracker.Cycle(ctx)
	got := e.row(t, e.tenantA, "auth-510")
	if got.Status != decision.StatusDebitConfirmed || got.Events[len(got.Events)-1] != "APPROVED>DEBIT_CONFIRMED:" {
		t.Errorf("10 blocks on top: %s %v, want DEBIT_CONFIRMED", got.Status, got.Events)
	}
	if tx := e.debitTx(t, "auth-510"); tx.Status != "CONFIRMED" {
		t.Errorf("operator_txs %s, want CONFIRMED", tx.Status)
	}
	var wei hexutil.Big
	e.chain.Call(t, &wei, "eth_getBalance", e.chain.Operator, "latest")
	// A float64 gauge holds about 16 significant digits of the wei balance.
	want, _ := new(big.Float).SetInt((*big.Int)(&wei)).Float64()
	if got := e.gauge(t, "operator_gas_balance"); got <= 0 || got/want < 0.999999 || got/want > 1.000001 {
		t.Errorf("operator_gas_balance %v, want %v", got, want)
	}
	e.noNonceGap(t)
}

// S2-T511 — Req: UC-3 row 2, FR-18, EC-7
func TestT511_DroppedDebitResubmitted(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	snap := e.snapshot(t)
	approvedWith(t, e.authorize(t, authReq("auth-511", "card_A", "5", "USD")), "5000000")
	before := e.debitTx(t, "auth-511")
	e.revert(t, snap)
	e.mine(t, 2)

	e.tracker.Cycle(ctx)
	after := e.debitTx(t, "auth-511")
	if after.Nonce != before.Nonce || e.replacedHashes(t, "auth-511") != 1 || string(after.TxHash) == string(before.TxHash) {
		t.Errorf("resubmission: nonce %d → %d, %d replaced hashes; want the same slot, a new hash", before.Nonce, after.Nonce,
			e.replacedHashes(t, "auth-511"))
	}
	if got := e.row(t, e.tenantA, "auth-511").Status; got != decision.StatusApproved {
		t.Errorf("%s, want APPROVED to stay", got)
	}
	var validUntil time.Time
	if err := e.owner.QueryRow(ctx, `SELECT valid_until FROM authorizations WHERE auth_id = 'auth-511'`).Scan(&validUntil); err != nil {
		t.Fatal(err)
	}
	if !validUntil.After(time.Now().Add(time.Second)) {
		t.Errorf("valid_until %s is not a new one", validUntil)
	}
	e.mine(t, 3)
	e.cycleUntilStatus(t, "auth-511", decision.StatusDebitConfirmed)
	if n := e.debitedFor(t, "auth-511"); n != 1 {
		t.Errorf("%d Debited events, want 1", n)
	}
	e.noNonceGap(t)
}

// S2-T512, S2-T522 — Req: UC-3 row 3, FR-18, FR-14
func TestT512_ResubmissionFails(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	snap := e.snapshot(t)
	approvedWith(t, e.authorize(t, authReq("auth-512", "card_A", "5", "USD")), "5000000")
	acceptedWith(t, e.ret(t, "auth-512", returnReq("rv-512", "2")), "2000000")
	e.revert(t, snap)
	e.chain.Approve(t, e.wallet, big.NewInt(0))
	e.mine(t, 2)

	e.cycleUntilStatus(t, "auth-512", decision.StatusDebitLost)
	got := e.row(t, e.tenantA, "auth-512")
	if got.Events[len(got.Events)-1] != "APPROVED>DEBIT_LOST:" {
		t.Errorf("history %v", got.Events)
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations WHERE auth_id = 'auth-512' AND debited_amount = 0 AND returned_amount = 0`); n != 1 {
		t.Error("debited_amount and returned_amount are not 0")
	}
	if r := e.returnRow(t, "rv-512"); r.Status != "NOTHING_TO_RETURN" || r.TokenAmount != "0" {
		t.Errorf("open return %+v, want NOTHING_TO_RETURN 0", r)
	}
	if v := e.counter(t, "debits_lost_total"); v != 1 {
		t.Errorf("debits_lost_total = %v", v)
	}
	if !strings.Contains(e.logs.String(), "DEBIT_LOST") {
		t.Error("no alert in the log")
	}
	e.noNonceGap(t)
}

// S2-T513, S2-T522 — Req: UC-3 rows 4–5, FR-17, EC-6
func TestT513_LateDebit(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.chain.SetAutomine(t, false)
	declinedWith(t, e.authorize(t, authReq("auth-513", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
	validUntil := e.validUntil(t, "auth-513")
	e.chain.Call(t, nil, "evm_setNextBlockTimestamp", validUntil.Unix()-1)
	e.chain.Mine(t)
	e.chain.SetAutomine(t, true)

	e.cycleUntilStatus(t, "auth-513", decision.StatusLateDebit)
	var returnID, typ, amount string
	if err := e.owner.QueryRow(ctx, `SELECT return_id, type, token_amount::text FROM returns`).Scan(&returnID, &typ, &amount); err != nil {
		t.Fatal(err)
	}
	if typ != "LATE_DEBIT" || amount != "5000000" || returnID != tracker.LateDebitReturnID(decision.ChainAuthID(e.tenantA, "auth-513").Bytes()) {
		t.Errorf("return %s %s %s", returnID, typ, amount)
	}
	if v := e.counter(t, "late_debits_total"); v != 1 {
		t.Errorf("late_debits_total = %v", v)
	}
	e.restartTracker(t) // a restart creates no second return
	e.settle(t)
	e.cycleUntilStatus(t, "auth-513", decision.StatusLateDebitRefunded)
	if n := e.count(t, `SELECT count(*) FROM returns`); n != 1 {
		t.Errorf("%d returns, want 1", n)
	}
	debited, refunded := e.onChain(t, "auth-513")
	if debited.String() != "5000000" || refunded.String() != "5000000" {
		t.Errorf("on chain debited %s refunded %s", debited, refunded)
	}
	e.noNonceGap(t)
}

func (e *env) validUntil(t *testing.T, authID string) time.Time {
	t.Helper()
	var v time.Time
	if err := e.owner.QueryRow(ctx, `SELECT valid_until FROM authorizations WHERE auth_id = $1`, authID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// counter returns the value of a counter without labels.
func (e *env) counter(t *testing.T, name string) float64 {
	t.Helper()
	families, err := e.metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("no metric %s", name)
	return 0
}

// S2-T514 — Req: UC-3 row 6
func TestT514_ExpiredAfterTimeout(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.chain.SetAutomine(t, false)
	declinedWith(t, e.authorize(t, authReq("auth-514", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
	e.chain.Call(t, nil, "evm_setNextBlockTimestamp", e.validUntil(t, "auth-514").Unix()+1)
	e.chain.Mine(t) // the debit is mined past validUntil: AuthExpired
	e.chain.SetAutomine(t, true)

	e.cycleUntilStatus(t, "auth-514", decision.StatusDeclined)
	got := e.row(t, e.tenantA, "auth-514")
	if got.DeclineReason != decision.ReasonTimeout || got.Events[len(got.Events)-1] != "TIMED_OUT>DECLINED:TIMEOUT" {
		t.Errorf("row %s %v", got.DeclineReason, got.Events)
	}
	if tx := e.debitTx(t, "auth-514"); tx.Status != "REVERTED" || tx.BlockNumber == nil {
		t.Errorf("operator_txs %+v, want REVERTED", tx)
	}
	e.noNonceGap(t)
}

// S2-T515, S2-T522 — Req: UC-3 row 7, FR-20. A refund, which has no deadline, stuck behind a raised base fee.
func TestT515_StuckReplaced(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	approvedWith(t, e.authorize(t, authReq("auth-515", "card_A", "5", "USD")), "5000000")
	acceptedWith(t, e.ret(t, "auth-515", returnReq("rv-515", "")), "5000000")
	e.chain.SetAutomine(t, false)
	e.tracker.Cycle(ctx)
	if r := e.returnRow(t, "rv-515"); r.Status != "SUBMITTED" {
		t.Fatalf("refund %s, want SUBMITTED", r.Status)
	}
	var oldHash []byte
	if err := e.owner.QueryRow(ctx, `SELECT tx_hash FROM operator_txs WHERE purpose = 'REFUND'`).Scan(&oldHash); err != nil {
		t.Fatal(err)
	}
	old := e.txFees(t, common.BytesToHash(oldHash))
	e.chain.Call(t, nil, "anvil_setNextBlockBaseFeePerGas", (*hexutil.Big)(new(big.Int).Mul(old.Cap, big.NewInt(4))))
	e.chain.Mine(t) // the refund does not fit the base fee and stays pending

	e.clock.Set(time.Now().Add(2 * time.Second))
	e.tracker.Cycle(ctx)
	if v := e.gauge(t, "operator_tx_pending_seconds"); v <= 0 {
		t.Errorf("operator_tx_pending_seconds = %v while the refund is pending", v)
	}
	e.clock.Set(time.Now().Add(4 * time.Second)) // pending longer than tracker_interval × 3
	e.tracker.Cycle(ctx)
	var newHash []byte
	var replaced int
	if err := e.owner.QueryRow(ctx, `SELECT tx_hash, cardinality(replaced_hashes) FROM operator_txs WHERE purpose = 'REFUND'`).
		Scan(&newHash, &replaced); err != nil {
		t.Fatal(err)
	}
	if replaced != 1 || string(newHash) == string(oldHash) {
		t.Fatalf("replaced_hashes %d; want the old hash kept and a new one current", replaced)
	}
	bumped := opqueue.Fees{Tip: old.Tip, Cap: old.Cap}.Bumped(25)
	if f := e.txFees(t, common.BytesToHash(newHash)); f.Tip.Cmp(bumped.Tip) < 0 || f.Cap.Cmp(bumped.Cap) < 0 {
		t.Errorf("replacement fees tip %s cap %s, want at least %s and %s (+25 %%)", f.Tip, f.Cap, bumped.Tip, bumped.Cap)
	}
	e.chain.SetAutomine(t, true)
	e.chain.Mine(t)
	e.clock.Set(time.Time{})
	e.cycleUntil(t, "rv-515", "INCLUDED")
	e.tracker.Cycle(ctx)
	if v := e.gauge(t, "operator_tx_pending_seconds"); v != 0 {
		t.Errorf("operator_tx_pending_seconds = %v after the inclusion, want 0", v)
	}
	if n := e.refundedFor(t, "rv-515"); n != 1 {
		t.Errorf("%d Refunded events", n)
	}
	e.noNonceGap(t)
}

func (e *env) txFees(t *testing.T, hash common.Hash) opqueue.Fees {
	t.Helper()
	var tx struct {
		Tip hexutil.Big `json:"maxPriorityFeePerGas"`
		Cap hexutil.Big `json:"maxFeePerGas"`
	}
	e.chain.Call(t, &tx, "eth_getTransactionByHash", hash)
	return opqueue.Fees{Tip: (*big.Int)(&tx.Tip), Cap: (*big.Int)(&tx.Cap)}
}

// S2-T516 — Req: UC-3 row 8, FR-20. The debit is kept out of the blocks by a raised base fee until past validUntil.
func TestT516_ReleaseAfterValidUntil(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.chain.SetAutomine(t, false)
	declinedWith(t, e.authorize(t, authReq("auth-516", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
	pending := e.debitTx(t, "auth-516")
	fees := e.txFees(t, common.BytesToHash(pending.TxHash))
	e.chain.Call(t, nil, "anvil_setNextBlockBaseFeePerGas", (*hexutil.Big)(new(big.Int).Mul(fees.Cap, big.NewInt(4))))
	e.chain.Call(t, nil, "evm_setNextBlockTimestamp", e.validUntil(t, "auth-516").Unix()+1)
	e.chain.Mine(t)
	e.chain.SetAutomine(t, true)

	e.cycleUntilStatus(t, "auth-516", decision.StatusDeclined)
	deadline := time.Now().Add(8 * time.Second)
	for e.debitTx(t, "auth-516").Status != "RELEASED" && time.Now().Before(deadline) {
		e.tracker.Cycle(ctx)
		time.Sleep(50 * time.Millisecond)
	}
	tx := e.debitTx(t, "auth-516")
	if tx.Status != "RELEASED" || tx.Purpose != "RELEASE" || tx.Nonce != pending.Nonce || e.replacedHashes(t, "auth-516") != 1 {
		t.Errorf("slot %+v, want RELEASE / RELEASED in nonce %d with the debit hash kept", tx, pending.Nonce)
	}
	if n := e.debitedFor(t, "auth-516"); n != 0 {
		t.Error("the debit landed")
	}
	approvedWith(t, e.authorize(t, authReq("auth-516-next", "card_A", "1", "USD")), "1000000")
	if next := e.debitTx(t, "auth-516-next"); next.Nonce != pending.Nonce+1 {
		t.Errorf("the next authorization used nonce %d, want %d", next.Nonce, pending.Nonce+1)
	}
	e.noNonceGap(t)
}

// S2-T517 — Req: UC-3 rows 9 and 11, FR-16, EC-18, D-21. The process stops before storing the outcome: the row stays
// DEBIT_SUBMITTED. A new tracker reads the chain first; past the deadline the row is TIMED_OUT (row 11), so a debit
// that landed is a late debit, returned in full.
func TestT517_RestartBetweenSendAndReceipt(t *testing.T) {
	c := testchain.Start(t)
	stop := func(p *pgxpool.Pool) decision.DB {
		return failingDB{Pool: p, match: "FinishSubmittedAuthorization"}
	}

	t.Run("debited > 0: TIMED_OUT, then LATE_DEBIT, no second debit", func(t *testing.T) {
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true, debitDB: stop})
		e.chain.SetAutomine(t, false)
		declinedWith(t, e.authorize(t, authReq("auth-517-a", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
		if got := e.row(t, e.tenantA, "auth-517-a").Status; got != decision.StatusDebitSubmitted {
			t.Fatalf("before the restart: %s, want DEBIT_SUBMITTED", got)
		}
		e.chain.Mine(t) // the debit lands while the process is down
		e.chain.SetAutomine(t, true)
		sent := e.chain.TxCount(t, e.chain.Operator)
		e.clock.Set(time.Now().Add(3 * time.Second))
		e.restartTracker(t)
		got := e.row(t, e.tenantA, "auth-517-a")
		if got.Status != decision.StatusLateDebit || !strings.Contains(strings.Join(got.Events, " "), "DEBIT_SUBMITTED>TIMED_OUT:TIMEOUT TIMED_OUT>LATE_DEBIT:") {
			t.Errorf("after the start: %s %v, want TIMED_OUT then LATE_DEBIT", got.Status, got.Events)
		}
		if n := e.chain.TxCount(t, e.chain.Operator); n != sent {
			t.Errorf("the start sent %d transactions", n-sent)
		}
		e.clock.Set(time.Time{})
		e.settle(t)
		e.cycleUntilStatus(t, "auth-517-a", decision.StatusLateDebitRefunded)
		e.noNonceGap(t)
	})

	t.Run("debited = 0: wait until validUntil, then DECLINED", func(t *testing.T) {
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true, debitDB: stop})
		e.chain.SetAutomine(t, false)
		defer e.chain.SetAutomine(t, true)
		declinedWith(t, e.authorize(t, authReq("auth-517-b", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
		sent := e.chain.TxCount(t, e.chain.Operator)
		e.clock.Set(time.Now().Add(3 * time.Second))
		e.restartTracker(t)
		if got := e.row(t, e.tenantA, "auth-517-b").Status; got != decision.StatusTimedOut {
			t.Errorf("not expired: %s, want TIMED_OUT and waiting", got)
		}
		if n := e.chain.TxCount(t, e.chain.Operator); n != sent {
			t.Errorf("the start sent %d transactions", n-sent)
		}
		e.chain.Call(t, nil, "evm_setNextBlockTimestamp", e.validUntil(t, "auth-517-b").Unix()+1)
		e.chain.Mine(t)
		e.cycleUntilStatus(t, "auth-517-b", decision.StatusDeclined)
		e.noNonceGap(t)
	})
}

// S2-T518 — Req: UC-3 rows 9 and 11, FR-16, FR-20, D-22
func TestT518_PlannedSlotNeverSent(t *testing.T) {
	c := testchain.Start(t)

	t.Run("TIMED_OUT with a PLANNED slot", func(t *testing.T) {
		p := newRPCProxy(t, c.RPCURL)
		e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true})
		p.mu.Lock()
		p.failMethod = "eth_maxPriorityFeePerGas"
		p.mu.Unlock()
		declinedWith(t, e.authorize(t, authReq("auth-518-a", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
		p.mu.Lock()
		p.failMethod = ""
		p.mu.Unlock()
		planned := e.debitTx(t, "auth-518-a")

		e.cycleUntilStatus(t, "auth-518-a", decision.StatusDeclined)
		deadline := time.Now().Add(8 * time.Second)
		for e.debitTx(t, "auth-518-a").Status != "RELEASED" && time.Now().Before(deadline) {
			e.tracker.Cycle(ctx)
			time.Sleep(50 * time.Millisecond)
		}
		if tx := e.debitTx(t, "auth-518-a"); tx.Status != "RELEASED" || tx.Purpose != "RELEASE" || tx.Nonce != planned.Nonce {
			t.Errorf("slot %+v, want RELEASED in nonce %d", tx, planned.Nonce)
		}
		if n := e.debitedFor(t, "auth-518-a"); n != 0 {
			t.Error("a debit was sent for the PLANNED slot")
		}
		approvedWith(t, e.authorize(t, authReq("auth-518-next", "card_A", "1", "USD")), "1000000")
		e.noNonceGap(t)
	})

	t.Run("restart: DEBIT_SUBMITTED with a PLANNED slot", func(t *testing.T) {
		on := &atomic.Bool{}
		on.Store(true)
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true, debitDB: func(p *pgxpool.Pool) decision.DB {
			return failingDB{Pool: p, match: "MarkOperatorTxSent|FinishSubmittedAuthorization", on: on}
		}})
		declinedWith(t, e.authorize(t, authReq("auth-518-b", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
		on.Store(false)
		if got := e.row(t, e.tenantA, "auth-518-b").Status; got != decision.StatusDebitSubmitted || e.debitTx(t, "auth-518-b").Status != "PLANNED" {
			t.Fatalf("before the restart: %s with %s, want DEBIT_SUBMITTED with PLANNED", got, e.debitTx(t, "auth-518-b").Status)
		}
		e.tracker.Cycle(ctx)
		if tx := e.debitTx(t, "auth-518-b"); tx.Status != "PLANNED" {
			t.Errorf("before deadline_at + %s the slot was touched: %s", decision.OutcomeWriteTimeout, tx.Status)
		}
		e.clock.Set(time.Now().Add(3 * time.Second))
		e.restartTracker(t)
		e.clock.Set(time.Time{})
		e.cycleUntilStatus(t, "auth-518-b", decision.StatusDeclined)
		deadline := time.Now().Add(8 * time.Second)
		for e.debitTx(t, "auth-518-b").Status != "RELEASED" && time.Now().Before(deadline) {
			e.tracker.Cycle(ctx)
			time.Sleep(50 * time.Millisecond)
		}
		if tx := e.debitTx(t, "auth-518-b"); tx.Status != "RELEASED" {
			t.Errorf("slot %s, want RELEASED", tx.Status)
		}
		if got := e.row(t, e.tenantA, "auth-518-b"); !strings.Contains(strings.Join(got.Events, " "), "DEBIT_SUBMITTED>TIMED_OUT:TIMEOUT TIMED_OUT>DECLINED:TIMEOUT") {
			t.Errorf("history %v", got.Events)
		}
		e.noNonceGap(t)
	})
}

// S2-T520 — Req: ADR-10. The block of the stored hash changes: the tracker reads the receipt again before deciding.
func TestT520_ReorgDetection(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	snap := e.snapshot(t)
	approvedWith(t, e.authorize(t, authReq("auth-520", "card_A", "5", "USD")), "5000000")
	before := e.debitTx(t, "auth-520")
	var raw hexutil.Bytes
	e.chain.Call(t, &raw, "eth_getRawTransactionByHash", common.BytesToHash(before.TxHash))
	e.revert(t, snap)
	e.mine(t, 1)
	var hash common.Hash
	e.chain.Call(t, &hash, "eth_sendRawTransaction", raw) // the same debit, now one block later
	waitReceipt(t, e, hash)
	sent := e.chain.TxCount(t, e.chain.Operator)

	e.tracker.Cycle(ctx)
	after := e.debitTx(t, "auth-520")
	if *after.BlockNumber == *before.BlockNumber || string(after.BlockHash) == string(before.BlockHash) || string(after.TxHash) != string(before.TxHash) {
		t.Errorf("block %d → %d: the new block is not stored", *before.BlockNumber, *after.BlockNumber)
	}
	if e.replacedHashes(t, "auth-520") != 0 || e.chain.TxCount(t, e.chain.Operator) != sent {
		t.Error("the tracker sent a transaction after the reorg")
	}
	if !strings.Contains(e.logs.String(), "reorg: the block of an approved debit changed") {
		t.Error("the reorg is not logged")
	}
	e.mine(t, 2)
	e.cycleUntilStatus(t, "auth-520", decision.StatusDebitConfirmed)
	e.noNonceGap(t)
}

// UC-3 row 12 (owner's decision of 2026-10-06): RECEIVED past deadline_at — a decline write of steps 5 to 9a that
// failed — becomes DECLINED / TIMEOUT; nothing was reserved or sent.
func TestUC3Row12_ReceivedPastDeadline(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	for _, c := range []struct {
		id string
		at time.Duration
	}{{"auth-r12-old", -10 * time.Second}, {"auth-r12-new", 0}} {
		e.exec(t, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, card_id, request_hash, fiat_amount,
			fiat_currency, status, received_at, deadline_at) VALUES ($1, $2, $3, $4, '\x00', 5, 'USD', 'RECEIVED', $5, $5::timestamptz + interval '2.5 seconds')`,
			e.tenantA, c.id, decision.ChainAuthID(e.tenantA, c.id).Bytes(), e.cardA, time.Now().Add(c.at))
		e.exec(t, `INSERT INTO authorization_events (authorization_id, to_status, created_at)
			SELECT id, 'RECEIVED', received_at FROM authorizations WHERE auth_id = $1`, c.id)
	}
	sent := e.chain.TxCount(t, e.chain.Operator)
	e.tracker.Cycle(ctx)
	if got := e.row(t, e.tenantA, "auth-r12-old"); got.Status != decision.StatusDeclined || got.DeclineReason != decision.ReasonTimeout ||
		got.DecidedAt == nil || got.Events[len(got.Events)-1] != "RECEIVED>DECLINED:TIMEOUT" {
		t.Errorf("past the deadline: %s %s %v", got.Status, got.DeclineReason, got.Events)
	}
	if got := e.row(t, e.tenantA, "auth-r12-new").Status; got != decision.StatusReceived {
		t.Errorf("within the deadline: %s, want RECEIVED untouched", got)
	}
	if n := e.chain.TxCount(t, e.chain.Operator); n != sent || e.count(t, `SELECT count(*) FROM operator_txs`) != 0 {
		t.Error("something was reserved or sent")
	}
}

// dropApproved approves a debit of the card and drops it from the chain with a snapshot and a revert, so the
// tracker's next cycle resubmits it (row 2). Mining stops.
func (e *env) dropApproved(t *testing.T, authID string) {
	t.Helper()
	snap := e.snapshot(t)
	approvedWith(t, e.authorize(t, authReq(authID, "card_A", "5", "USD")), "5000000")
	e.revert(t, snap)
	e.mine(t, 2)
	e.chain.SetAutomine(t, false)
}

// S2-T523 — Req: UC-3 row 7, FR-20, rules of S2 st6b. A resubmitted debit of an APPROVED authorization stuck before
// its validUntil behind a raised base fee is replaced in its nonce with both fees + 25 %, then mines.
func TestT523_StuckDebitReplaced(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.dropApproved(t, "auth-523")
	e.tracker.Cycle(ctx) // row 2: the resubmission, pending
	stuck := e.debitTx(t, "auth-523")
	if stuck.Status != "SENT" || e.replacedHashes(t, "auth-523") != 1 {
		t.Fatalf("resubmission %+v", stuck)
	}
	old := e.txFees(t, common.BytesToHash(stuck.TxHash))
	e.chain.Call(t, nil, "anvil_setNextBlockBaseFeePerGas", (*hexutil.Big)(new(big.Int).Mul(old.Cap, big.NewInt(4))))
	e.chain.Mine(t) // the resubmission no longer fits the base fee; the chain is still before its validUntil
	if ts := e.headTime(t); ts > e.validUntil(t, "auth-523").Unix() {
		t.Fatal("the chain passed validUntil before the replacement")
	}

	e.clock.Set(time.Now().Add(1500 * time.Millisecond)) // unmined longer than tracker_interval since its send
	e.tracker.Cycle(ctx)
	replaced := e.debitTx(t, "auth-523")
	if replaced.Nonce != stuck.Nonce || replaced.Purpose != "DEBIT" || e.replacedHashes(t, "auth-523") != 2 ||
		string(replaced.TxHash) == string(stuck.TxHash) {
		t.Fatalf("replacement %+v, want a new debit hash in nonce %d", replaced, stuck.Nonce)
	}
	bumped := opqueue.Fees{Tip: old.Tip, Cap: old.Cap}.Bumped(25)
	if f := e.txFees(t, common.BytesToHash(replaced.TxHash)); f.Tip.Cmp(bumped.Tip) < 0 || f.Cap.Cmp(bumped.Cap) < 0 {
		t.Errorf("replacement fees tip %s cap %s, want at least %s and %s", f.Tip, f.Cap, bumped.Tip, bumped.Cap)
	}
	e.chain.SetAutomine(t, true)
	e.chain.Mine(t)
	e.clock.Set(time.Time{})
	e.mine(t, 2)
	e.cycleUntilStatus(t, "auth-523", decision.StatusDebitConfirmed)
	if n := e.debitedFor(t, "auth-523"); n != 1 {
		t.Errorf("%d Debited events, want 1", n)
	}
	e.noNonceGap(t)
}

func (e *env) headTime(t *testing.T) int64 {
	t.Helper()
	var head struct {
		Time hexutil.Uint64 `json:"timestamp"`
	}
	e.chain.Call(t, &head, "eth_getBlockByNumber", "latest", false)
	return int64(head.Time)
}

// S2-T524 — Req: UC-3 rows 2, 3 and 8, D-23. A resubmitted debit held out until past its validUntil is released, then
// resubmitted again with a new validUntil, and mines. The authorization stays APPROVED; no DEBIT_LOST.
func TestT524_ResubmissionPastValidUntil(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.dropApproved(t, "auth-524")
	e.tracker.Cycle(ctx) // row 2: the first resubmission, pending
	first := e.debitTx(t, "auth-524")
	fees := e.txFees(t, common.BytesToHash(first.TxHash))
	validUntil := e.validUntil(t, "auth-524")
	e.chain.Call(t, nil, "anvil_setNextBlockBaseFeePerGas", (*hexutil.Big)(new(big.Int).Mul(fees.Cap, big.NewInt(4))))
	e.chain.Call(t, nil, "evm_setNextBlockTimestamp", validUntil.Unix()+1)
	e.chain.Mine(t) // past validUntil, the resubmission unmined
	e.chain.SetAutomine(t, true)
	// The tracker's clock follows the chain, which the test moved ahead: the next validUntil is ahead of the chain.
	e.clock.Set(time.Unix(validUntil.Unix()+2, 0))

	deadline := time.Now().Add(10 * time.Second)
	for e.debitedFor(t, "auth-524") == 0 && time.Now().Before(deadline) {
		e.tracker.Cycle(ctx)
		if got := e.row(t, e.tenantA, "auth-524").Status; got != decision.StatusApproved {
			t.Fatalf("%s, want APPROVED throughout", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	txs := e.opTxs(t, "auth-524")
	if len(txs) != 2 || txs[0].Purpose != "RELEASE" || txs[0].Status != "RELEASED" || txs[0].Nonce != first.Nonce ||
		txs[1].Purpose != "DEBIT" || txs[1].Nonce != first.Nonce+1 {
		t.Fatalf("slots %+v, want the first released and a new debit in the next nonce", txs)
	}
	if !e.validUntil(t, "auth-524").After(validUntil) {
		t.Error("the second resubmission has no new validUntil")
	}
	e.mine(t, 2)
	e.cycleUntilStatus(t, "auth-524", decision.StatusDebitConfirmed)
	if n := e.debitedFor(t, "auth-524"); n != 1 {
		t.Errorf("%d Debited events, want exactly 1", n)
	}
	if v := e.counter(t, "debits_lost_total"); v != 0 {
		t.Errorf("debits_lost_total = %v, want 0", v)
	}
	logs := e.logs.String()
	if strings.Contains(logs, "DEBIT_LOST") || !strings.Contains(logs, "ALERT: the resubmitted debit of an approved authorization did not execute before its validUntil") {
		t.Errorf("want the D-23 alert and no DEBIT_LOST:\n%s", logs)
	}
	e.noNonceGap(t)
}

// S2-T525 — Req: rules of S2 st6b. A nonce of the operator used on chain by a transaction that is none of the row's
// hashes: the operator key was used outside card-auth. One alert per row per process; the row is left as it is.
func TestT525_NonceUsedOutsideCardAuth(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.chain.SetAutomine(t, false)
	declinedWith(t, e.authorize(t, authReq("auth-525", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
	slot := e.debitTx(t, "auth-525")
	fees := e.txFees(t, common.BytesToHash(slot.TxHash))

	// Outside card-auth: the same key fills the nonce with a transfer to itself, above the fees of the debit.
	key, err := crypto.ToECDSA(e.chain.OperatorKey.Value())
	if err != nil {
		t.Fatal(err)
	}
	b := fees.Bumped(50)
	to := e.chain.Operator
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(testchain.ChainID), Nonce: uint64(slot.Nonce),
		GasTipCap: b.Tip, GasFeeCap: b.Cap, Gas: 21000, To: &to, Value: new(big.Int)}), types.LatestSignerForChainID(big.NewInt(testchain.ChainID)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	e.chain.Call(t, nil, "eth_sendRawTransaction", hexutil.Bytes(raw))
	e.chain.Mine(t)
	e.chain.SetAutomine(t, true)

	for range 4 {
		e.tracker.Cycle(ctx)
	}
	const msg = "ALERT: the operator key was used outside card-auth"
	if n := strings.Count(e.logs.String(), msg); n != 1 {
		t.Errorf("%d alerts, want exactly 1 after 4 cycles", n)
	}
	if !strings.Contains(e.logs.String(), `"level":"ERROR","msg":"`+msg) {
		t.Error("the alert is not at Error level")
	}
	if got := e.debitTx(t, "auth-525"); got.Status != "SENT" || string(got.TxHash) != string(slot.TxHash) || e.replacedHashes(t, "auth-525") != 0 {
		t.Errorf("the row changed: %+v", got)
	}
	e.noNonceGap(t)
}

// The tracker touches only the rows of its own chain and operator key: the database may hold rows of an earlier key
// (a rotation) or of another deployment. Their PLANNED and SENT slots are never released or replaced with this key.
func TestTrackerTouchesOnlyItsOwnRows(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	foreign := common.HexToAddress("0x00000000000000000000000000000000000f0e1d")
	e.insertAuthorization(t, "auth-foreign", decision.StatusTimedOut)
	e.exec(t, `UPDATE authorizations SET chain_id = $1, valid_until = now() - interval '1 hour' WHERE auth_id = 'auth-foreign'`, testchain.ChainID)
	e.exec(t, `INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose, authorization_id, status, tx_hash)
		SELECT $1, $2, 7, 'DEBIT', id, 'SENT', '\x`+strings.Repeat("ab", 32)+`' FROM authorizations WHERE auth_id = 'auth-foreign'`,
		testchain.ChainID, foreign.Bytes())
	e.exec(t, `INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose, authorization_id, status)
		SELECT $1, $2, 8, 'DEBIT', id, 'PLANNED' FROM authorizations WHERE auth_id = 'auth-foreign'`, testchain.ChainID, foreign.Bytes())
	sent := e.chain.TxCount(t, e.chain.Operator)

	for range 3 {
		e.tracker.Cycle(ctx)
	}
	if n := e.chain.TxCount(t, e.chain.Operator); n != sent {
		t.Errorf("the operator sent %d transactions for rows of another key", n-sent)
	}
	if got := e.row(t, e.tenantA, "auth-foreign").Status; got != decision.StatusTimedOut {
		t.Errorf("the authorization of another key moved to %s", got)
	}
	if n := e.count(t, `SELECT count(*) FROM operator_txs WHERE operator_address = $1 AND status IN ('SENT', 'PLANNED')`, foreign.Bytes()); n != 2 {
		t.Errorf("%d foreign slots left as they were, want 2", n)
	}
	if v := e.gauge(t, "operator_tx_pending_seconds"); v != 0 {
		t.Errorf("operator_tx_pending_seconds = %v: it counts another key's rows", v)
	}
}
