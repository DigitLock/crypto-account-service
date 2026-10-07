package reconcile_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"

	"github.com/DigitLock/crypto-account-service/internal/reconcile"
)

func wantTotals(t *testing.T, r reconcile.Run, want reconcile.Totals) {
	t.Helper()
	if r.Totals != want {
		t.Errorf("run of %s: totals %+v, want %+v", r.Tenant, r.Totals, want)
	}
}

func wantMismatches(t *testing.T, r reconcile.Run, want ...reconcile.Mismatch) {
	t.Helper()
	if len(r.Mismatches) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(r.Mismatches, want) {
		t.Errorf("run of %s: mismatches\n%+v\nwant\n%+v", r.Tenant, r.Mismatches, want)
	}
}

// clean is the scenario of T601 and T611 for tenant-a: a1 DEBIT_CONFIRMED and debited 20, a2 APPROVED and debited
// 10, a3 DECLINED without a debit, and r1, a CONFIRMED return of 5 of a1, refunded; every row with its transaction.
func (h *harness) clean(t *testing.T) {
	t.Helper()
	w := h.wallet(t)
	a1 := h.authorization(t, "tenant-a", "a1", "DEBIT_CONFIRMED", units(20), h.at(1), h.at(1))
	hash, block := h.debit(t, w, units(20), h.chainID(t, "tenant-a", "a1"))
	h.operatorTx(t, "DEBIT", a1, hash, "CONFIRMED", block)
	a2 := h.authorization(t, "tenant-a", "a2", "APPROVED", units(10), h.at(2), h.at(2))
	hash, block = h.debit(t, w, units(10), h.chainID(t, "tenant-a", "a2"))
	h.operatorTx(t, "DEBIT", a2, hash, "INCLUDED", block)
	h.authorization(t, "tenant-a", "a3", "DECLINED", units(5), h.at(3), h.at(3))
	r1 := h.returnRow(t, "tenant-a", a1, "r1", "CONFIRMED", units(5))
	hash, block = h.refund(t, h.chainID(t, "tenant-a", "a1"), h.chainID(t, "tenant-a", "r1"), units(5))
	h.operatorTx(t, "REFUND", r1, hash, "CONFIRMED", block)
}

// lateDebit adds to tenant-a a TIMED_OUT authorization a0, received before a1, whose debit of 4 landed late, and its
// LATE_DEBIT return CONFIRMED with its REFUND row and Refunded of 4. Rules 1 and 2a do not check TIMED_OUT: a0 and its
// debit are not counted in A's totals or period; its return is checked by rule 3 (S3 D-40 refined).
func (h *harness) lateDebit(t *testing.T) {
	t.Helper()
	a0 := h.authorization(t, "tenant-a", "a0", "TIMED_OUT", units(4), h.at(0), h.at(0))
	hash, block := h.debit(t, h.wallet(t), units(4), h.chainID(t, "tenant-a", "a0"))
	h.operatorTx(t, "DEBIT", a0, hash, "CONFIRMED", block)
	r0 := h.returnRowOfType(t, "tenant-a", a0, "late-a0", "LATE_DEBIT", "CONFIRMED", units(4))
	hash, block = h.refund(t, h.chainID(t, "tenant-a", "a0"), h.chainID(t, "tenant-a", "late-a0"), units(4))
	h.operatorTx(t, "REFUND", r0, hash, "CONFIRMED", block)
}

// S3-T601 — Req: Card Spend UC-4 basic flow; S3 D-40. Authorizations and a return of tenant A with their debits and
// refund on Anvil, final; T synced to a checkpoint: one run for A and one for cas-platform, no mismatch, totals of
// D-40. Then a TIMED_OUT authorization with a late debit and its LATE_DEBIT return (lateDebit): A counts the return,
// not the authorization or its debit; still no mismatch.
func TestT601_CleanRun(t *testing.T) {
	h := setup(t)
	h.clean(t)
	h.sync(t)

	runs := h.reconcile(t)
	if len(runs) != 2 || runs[0].Tenant != "tenant-a" || runs[1].Tenant != "cas-platform" || runs[0].Platform || !runs[1].Platform {
		t.Fatalf("runs %+v, want tenant-a then cas-platform", runs)
	}
	if n := countRows(t, h, `SELECT count(*) FROM balance_checkpoints WHERE connection_id = $1 AND gap = 0`, h.treasury); n != 1 {
		t.Fatalf("%d checkpoints of T with gap 0, want 1: T is not synced to a checkpoint", n)
	}
	for _, r := range runs {
		wantMismatches(t, r)
	}
	wantTotals(t, runs[0], reconcile.Totals{AuthorizationsChecked: 3, DebitsCount: 2, DebitsAmount: "30000000",
		ReturnsChecked: 1, RefundsCount: 1, RefundsAmount: "5000000"})
	wantTotals(t, runs[1], reconcile.Totals{DebitsCount: 2, DebitsAmount: "30000000", RefundsCount: 1, RefundsAmount: "5000000"})

	// S3 D-40 refined: a TIMED_OUT authorization with a late debit and its LATE_DEBIT return, refunded. A counts the
	// return only; the platform run counts every event; no mismatch.
	h.lateDebit(t)
	h.sync(t)
	runs = h.reconcile(t)
	if len(runs) != 2 {
		t.Fatalf("%d runs after the late debit, want 2", len(runs))
	}
	for _, r := range runs {
		wantMismatches(t, r)
	}
	a := runOf(t, runs, "tenant-a")
	wantTotals(t, a, reconcile.Totals{AuthorizationsChecked: 3, DebitsCount: 2, DebitsAmount: "30000000",
		ReturnsChecked: 2, RefundsCount: 2, RefundsAmount: "9000000"})
	if !a.PeriodFrom.Equal(h.at(1)) {
		t.Errorf("period_from of A %s, want received_at of a1 %s: a0 is not counted", a.PeriodFrom, h.at(1))
	}
	wantTotals(t, runOf(t, runs, "cas-platform"), reconcile.Totals{DebitsCount: 3, DebitsAmount: "34000000",
		RefundsCount: 2, RefundsAmount: "9000000"})
}

// S3-T602 — Req: Card Spend UC-4 rule 1, FR-21. APPROVED authorizations of A within the boundary without a Debited:
// MISSING_DEBIT with auth_id, chain_auth_id, expected_amount, and the hash of its DEBIT transaction when there is one.
func TestT602_MissingDebit(t *testing.T) {
	h := setup(t)
	sent := randomHash(t)
	a1 := h.authorization(t, "tenant-a", "a1", "APPROVED", units(8), h.at(1), h.at(1))
	h.operatorTx(t, "DEBIT", a1, sent, "SENT", 0)
	h.authorization(t, "tenant-a", "a2", "DEBIT_CONFIRMED", units(9), h.at(2), h.at(2))
	h.sync(t)

	runs := h.reconcile(t)
	a := runOf(t, runs, "tenant-a")
	want := []reconcile.Mismatch{
		{Type: reconcile.MissingDebit, AuthID: "a1", ChainAuthID: hexOf(h.chainID(t, "tenant-a", "a1")), TxHash: strings.ToLower(sent.Hex()), ExpectedAmount: "8000000"},
		{Type: reconcile.MissingDebit, AuthID: "a2", ChainAuthID: hexOf(h.chainID(t, "tenant-a", "a2")), ExpectedAmount: "9000000"},
	}
	if want[0].ChainAuthID > want[1].ChainAuthID {
		want[0], want[1] = want[1], want[0]
	}
	wantMismatches(t, a, want...)
	wantTotals(t, a, reconcile.Totals{AuthorizationsChecked: 2, DebitsAmount: "0", RefundsAmount: "0"})
	wantMismatches(t, runOf(t, runs, "cas-platform"))
}

// S3-T603 — Req: Card Spend UC-4 rule 1, FR-21; S3 D-21. token_amount 25, the event 20: AMOUNT_MISMATCH with
// expected_amount = token_amount, actual_amount = the amount of the event, tx_hash of the event.
func TestT603_AmountMismatchOfADebit(t *testing.T) {
	h := setup(t)
	w := h.wallet(t)
	a1 := h.authorization(t, "tenant-a", "a1", "DEBIT_CONFIRMED", units(25), h.at(1), h.at(1))
	hash, block := h.debit(t, w, units(20), h.chainID(t, "tenant-a", "a1"))
	h.operatorTx(t, "DEBIT", a1, hash, "CONFIRMED", block)
	h.sync(t)

	runs := h.reconcile(t)
	a := runOf(t, runs, "tenant-a")
	wantMismatches(t, a, reconcile.Mismatch{Type: reconcile.AmountMismatch, AuthID: "a1",
		ChainAuthID: hexOf(h.chainID(t, "tenant-a", "a1")), TxHash: strings.ToLower(hash.Hex()),
		ExpectedAmount: "25000000", ActualAmount: "20000000"})
	wantTotals(t, a, reconcile.Totals{AuthorizationsChecked: 1, DebitsCount: 1, DebitsAmount: "20000000", RefundsAmount: "0"})
	wantMismatches(t, runOf(t, runs, "cas-platform"))
}

// S3-T604 — Req: Card Spend UC-4 rule 2, FR-21, §4 issue 4. A debit sent with the operator key and an authId of no
// tenant: UNKNOWN_DEBIT in the cas-platform run with chain_auth_id, tx_hash, actual_amount and no auth_id; A's run
// does not hold it.
func TestT604_UnknownDebit(t *testing.T) {
	h := setup(t)
	h.clean(t)
	unknown := [32]byte{0xa6, 0x04}
	copy(unknown[2:], randomHash(t).Bytes())
	hash, _ := h.debit(t, h.wallet(t), units(3), unknown)
	h.sync(t)

	runs := h.reconcile(t)
	platform := runOf(t, runs, "cas-platform")
	wantMismatches(t, platform, reconcile.Mismatch{Type: reconcile.UnknownDebit, ChainAuthID: hexOf(unknown),
		TxHash: strings.ToLower(hash.Hex()), ActualAmount: "3000000"})
	wantTotals(t, platform, reconcile.Totals{DebitsCount: 3, DebitsAmount: "33000000", RefundsCount: 1, RefundsAmount: "5000000"})
	wantMismatches(t, runOf(t, runs, "tenant-a"))
}

// S3-T605 — Req: Card Spend UC-4 rule 3, FR-21. A CONFIRMED return of A whose REFUND block is at or below to_block,
// without a Refunded: MISSING_REFUND with return_id, chain_refund_id, expected_amount and the hash of its REFUND
// transaction.
func TestT605_MissingRefund(t *testing.T) {
	h := setup(t)
	w := h.wallet(t)
	a1 := h.authorization(t, "tenant-a", "a1", "DEBIT_CONFIRMED", units(20), h.at(1), h.at(1))
	hash, block := h.debit(t, w, units(20), h.chainID(t, "tenant-a", "a1"))
	h.operatorTx(t, "DEBIT", a1, hash, "CONFIRMED", block)
	r1 := h.returnRow(t, "tenant-a", a1, "r1", "CONFIRMED", units(5))
	refundHash := randomHash(t)
	h.operatorTx(t, "REFUND", r1, refundHash, "CONFIRMED", block)
	h.sync(t)

	runs := h.reconcile(t)
	a := runOf(t, runs, "tenant-a")
	wantMismatches(t, a, reconcile.Mismatch{Type: reconcile.MissingRefund, ReturnID: "r1",
		ChainRefundID: hexOf(h.chainID(t, "tenant-a", "r1")), TxHash: strings.ToLower(refundHash.Hex()), ExpectedAmount: "5000000"})
	wantTotals(t, a, reconcile.Totals{AuthorizationsChecked: 1, DebitsCount: 1, DebitsAmount: "20000000",
		ReturnsChecked: 1, RefundsAmount: "0"})
	wantMismatches(t, runOf(t, runs, "cas-platform"))
}

// S3-T606 — Req: Card Spend UC-4 rule 3, FR-21. token_amount of the return 6, the event 5: AMOUNT_MISMATCH with
// return_id, chain_refund_id, tx_hash and both amounts.
func TestT606_AmountMismatchOfARefund(t *testing.T) {
	h := setup(t)
	w := h.wallet(t)
	a1 := h.authorization(t, "tenant-a", "a1", "DEBIT_CONFIRMED", units(20), h.at(1), h.at(1))
	hash, block := h.debit(t, w, units(20), h.chainID(t, "tenant-a", "a1"))
	h.operatorTx(t, "DEBIT", a1, hash, "CONFIRMED", block)
	r1 := h.returnRow(t, "tenant-a", a1, "r1", "CONFIRMED", units(6))
	refundHash, refundBlock := h.refund(t, h.chainID(t, "tenant-a", "a1"), h.chainID(t, "tenant-a", "r1"), units(5))
	h.operatorTx(t, "REFUND", r1, refundHash, "CONFIRMED", refundBlock)
	h.sync(t)

	runs := h.reconcile(t)
	a := runOf(t, runs, "tenant-a")
	wantMismatches(t, a, reconcile.Mismatch{Type: reconcile.AmountMismatch, ReturnID: "r1",
		ChainRefundID: hexOf(h.chainID(t, "tenant-a", "r1")), TxHash: strings.ToLower(refundHash.Hex()),
		ExpectedAmount: "6000000", ActualAmount: "5000000"})
	wantTotals(t, a, reconcile.Totals{AuthorizationsChecked: 1, DebitsCount: 1, DebitsAmount: "20000000",
		ReturnsChecked: 1, RefundsCount: 1, RefundsAmount: "5000000"})
	wantMismatches(t, runOf(t, runs, "cas-platform"))
}

// S3-T607 — Req: Card Spend UC-4 rule 4, FR-22. A refund with the operator key and a refundId of no return:
// UNKNOWN_REFUND in the cas-platform run with chain_refund_id, tx_hash, actual_amount; not in A's run.
func TestT607_UnknownRefund(t *testing.T) {
	h := setup(t)
	h.clean(t)
	unknown := [32]byte{0xa6, 0x07}
	copy(unknown[2:], randomHash(t).Bytes())
	hash, _ := h.refund(t, h.chainID(t, "tenant-a", "a1"), unknown, units(3))
	h.sync(t)

	runs := h.reconcile(t)
	platform := runOf(t, runs, "cas-platform")
	wantMismatches(t, platform, reconcile.Mismatch{Type: reconcile.UnknownRefund, ChainRefundID: hexOf(unknown),
		TxHash: strings.ToLower(hash.Hex()), ActualAmount: "3000000"})
	wantTotals(t, platform, reconcile.Totals{DebitsCount: 2, DebitsAmount: "30000000", RefundsCount: 2, RefundsAmount: "8000000"})
	a := runOf(t, runs, "tenant-a")
	wantMismatches(t, a)
	wantTotals(t, a, reconcile.Totals{AuthorizationsChecked: 3, DebitsCount: 2, DebitsAmount: "30000000",
		ReturnsChecked: 1, RefundsCount: 1, RefundsAmount: "5000000"})
}

// S3-T608 — Req: Card Spend UC-4 rule 5; S3 D-24. 50 USDC minted to the treasury below backfill_floor: the
// checkpoint of T has balance 70, ledger total 20, gap 50; TREASURY_MISMATCH in the cas-platform run with asset,
// checkpoint_balance and ledger_total in asset units.
func TestT608_TreasuryMismatch(t *testing.T) {
	h := setup(t)
	w := h.wallet(t)
	h.c.Mint(t, h.c.Treasury, units(50))
	h.exec(t, `UPDATE sources SET config = config || jsonb_build_object('backfill_floor', $1::bigint) WHERE code = 'anvil'`,
		int64(h.c.Head(t)+1))
	a1 := h.authorization(t, "tenant-a", "a1", "DEBIT_CONFIRMED", units(20), h.at(1), h.at(1))
	hash, block := h.debit(t, w, units(20), h.chainID(t, "tenant-a", "a1"))
	h.operatorTx(t, "DEBIT", a1, hash, "CONFIRMED", block)
	h.sync(t)

	runs := h.reconcile(t)
	platform := runOf(t, runs, "cas-platform")
	wantMismatches(t, platform, reconcile.Mismatch{Type: reconcile.TreasuryMismatch, Asset: "USDC",
		CheckpointBalance: "70", LedgerTotal: "20"})
	wantMismatches(t, runOf(t, runs, "tenant-a"))
}

// S3-T609 — Req: Card Spend UC-4 eligibility; S3 D-7. Skipped by the first run: an APPROVED authorization with
// valid_until after period_to; a CONFIRMED return whose REFUND block is above to_block; a CONFIRMED return whose
// REFUND row has no block. After T syncs past valid_until and the late refund, the second run checks the first two;
// the return without a block stays skipped until its block is known.
func TestT609_Boundary(t *testing.T) {
	h := setup(t)
	w := h.wallet(t)
	a1 := h.authorization(t, "tenant-a", "a1", "DEBIT_CONFIRMED", units(20), h.at(1), h.at(1))
	hash, block := h.debit(t, w, units(20), h.chainID(t, "tenant-a", "a1"))
	h.operatorTx(t, "DEBIT", a1, hash, "CONFIRMED", block)
	late := h.headTime(t).Add(time.Hour)
	h.authorization(t, "tenant-a", "a-late", "APPROVED", units(7), h.at(2), late)
	noBlock := h.returnRow(t, "tenant-a", a1, "r-no-block", "CONFIRMED", units(3))
	noBlockHash, noBlockBlock := h.refund(t, h.chainID(t, "tenant-a", "a1"), h.chainID(t, "tenant-a", "r-no-block"), units(3))
	h.operatorTx(t, "REFUND", noBlock, noBlockHash, "CONFIRMED", 0)
	h.sync(t)

	toBlock, periodTo := h.cursor(t)
	if !periodTo.Before(late) {
		t.Fatalf("period_to %s is not before valid_until %s", periodTo, late)
	}
	rLate := h.returnRow(t, "tenant-a", a1, "r-late", "CONFIRMED", units(4))
	lateHash, lateBlock := h.refund(t, h.chainID(t, "tenant-a", "a1"), h.chainID(t, "tenant-a", "r-late"), units(4))
	h.operatorTx(t, "REFUND", rLate, lateHash, "CONFIRMED", lateBlock)
	if int64(lateBlock) <= toBlock {
		t.Fatalf("the late refund is in block %d, at or below to_block %d", lateBlock, toBlock)
	}

	first := runOf(t, h.reconcile(t), "tenant-a")
	wantMismatches(t, first)
	wantTotals(t, first, reconcile.Totals{AuthorizationsChecked: 1, DebitsCount: 1, DebitsAmount: "20000000", RefundsAmount: "0"})

	h.c.Call(t, nil, "evm_increaseTime", 2*60*60)
	h.sync(t)
	if _, periodTo := h.cursor(t); periodTo.Before(late) {
		t.Fatalf("period_to %s is still before valid_until %s", periodTo, late)
	}
	second := runOf(t, h.reconcile(t), "tenant-a")
	wantMismatches(t, second, reconcile.Mismatch{Type: reconcile.MissingDebit, AuthID: "a-late",
		ChainAuthID: hexOf(h.chainID(t, "tenant-a", "a-late")), ExpectedAmount: "7000000"})
	wantTotals(t, second, reconcile.Totals{AuthorizationsChecked: 2, DebitsCount: 1, DebitsAmount: "20000000",
		ReturnsChecked: 1, RefundsCount: 1, RefundsAmount: "4000000"})

	h.exec(t, `UPDATE operator_txs SET block_number = $2 WHERE return_row_id = $1`, noBlock, int64(noBlockBlock))
	third := runOf(t, h.reconcile(t), "tenant-a")
	wantTotals(t, third, reconcile.Totals{AuthorizationsChecked: 2, DebitsCount: 1, DebitsAmount: "20000000",
		ReturnsChecked: 2, RefundsCount: 2, RefundsAmount: "7000000"})
}

// storedRun is a row of reconciliation_runs.
type storedRun struct {
	tenantID             uuid.UUID
	sourceID             int16
	periodFrom, periodTo time.Time
	toBlock              int64
	totals               reconcile.Totals
	mismatches           []map[string]string
}

func (h *harness) storedRun(t *testing.T, id uuid.UUID) storedRun {
	t.Helper()
	var r storedRun
	var totals, mismatches []byte
	if err := h.owner.QueryRow(ctx, `SELECT tenant_id, source_id, period_from, period_to, to_block, totals, mismatches
		FROM reconciliation_runs WHERE id = $1`, id).Scan(&r.tenantID, &r.sourceID, &r.periodFrom, &r.periodTo, &r.toBlock,
		&totals, &mismatches); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(totals, &raw); err != nil || len(raw) != 6 {
		t.Errorf("totals %s: want the six fields of SRS — Core §2.1.1", totals)
	}
	if err := json.Unmarshal(totals, &r.totals); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mismatches, &r.mismatches); err != nil || r.mismatches == nil {
		t.Fatalf("mismatches %s: want a JSON array", mismatches)
	}
	return r
}

func countRows(t *testing.T, h *harness, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.owner.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// S3-T611 — Req: Card Spend UC-4, FR-21; Core §2.4; S3 D-7, S3 D-26, S3 D-40. The scenario of T601 with the late
// debit of a TIMED_OUT authorization received first (not counted: period_from stays a1), one more APPROVED
// authorization of A without a debit, and tenant C whose only authorization is beyond the boundary. One row per
// tenant and source: period_from the oldest authorization checked, the oldest event for cas-platform, period_to when
// nothing is checked; period_to = last_time and to_block = next_block − 1 of T's cursor; totals; mismatches with type,
// IDs and amounts.
func TestT611_StoredRuns(t *testing.T) {
	h := setup(t)
	h.clean(t)
	h.lateDebit(t)
	a4 := h.authorization(t, "tenant-a", "a4", "APPROVED", units(7), h.at(4), h.at(4))
	sent := randomHash(t)
	h.operatorTx(t, "DEBIT", a4, sent, "SENT", 0)
	h.authorization(t, "tenant-c", "c1", "APPROVED", units(1), h.at(0), h.headTime(t).Add(24*time.Hour))
	h.sync(t)
	toBlock, periodTo := h.cursor(t)
	var oldestEvent time.Time
	if err := h.owner.QueryRow(ctx, `SELECT min(occurred_at) FROM ledger_entries WHERE connection_id = $1
		AND type IN ('CARD_DEBIT', 'CARD_REFUND')`, h.treasury).Scan(&oldestEvent); err != nil {
		t.Fatal(err)
	}

	runs := h.reconcile(t)
	if len(runs) != 3 || runs[0].Tenant != "tenant-a" || runs[1].Tenant != "tenant-c" || runs[2].Tenant != "cas-platform" {
		t.Fatalf("runs %+v, want tenant-a, tenant-c, cas-platform", runs)
	}
	if n := countRows(t, h, `SELECT count(*) FROM reconciliation_runs`); n != 3 {
		t.Fatalf("%d stored runs, want 3", n)
	}
	want := map[string]struct {
		periodFrom time.Time
		totals     reconcile.Totals
		mismatches []map[string]string
	}{
		"tenant-a": {h.at(1), reconcile.Totals{AuthorizationsChecked: 4, DebitsCount: 2, DebitsAmount: "30000000",
			ReturnsChecked: 2, RefundsCount: 2, RefundsAmount: "9000000"}, []map[string]string{{
			"type": "MISSING_DEBIT", "auth_id": "a4", "chain_auth_id": hexOf(h.chainID(t, "tenant-a", "a4")),
			"tx_hash": strings.ToLower(sent.Hex()), "expected_amount": "7000000",
		}}},
		"tenant-c":     {periodTo, reconcile.Totals{DebitsAmount: "0", RefundsAmount: "0"}, []map[string]string{}},
		"cas-platform": {oldestEvent, reconcile.Totals{DebitsCount: 3, DebitsAmount: "34000000", RefundsCount: 2, RefundsAmount: "9000000"}, []map[string]string{}},
	}
	for _, r := range runs {
		got := h.storedRun(t, r.ID)
		w := want[r.Tenant]
		if got.tenantID != h.tenants[r.Tenant] || got.sourceID != h.sourceID {
			t.Errorf("%s: tenant %s, source %d", r.Tenant, got.tenantID, got.sourceID)
		}
		if !got.periodFrom.Equal(w.periodFrom) || !got.periodTo.Equal(periodTo) || got.toBlock != toBlock {
			t.Errorf("%s: period %s – %s, to_block %d; want %s – %s, %d", r.Tenant, got.periodFrom, got.periodTo,
				got.toBlock, w.periodFrom, periodTo, toBlock)
		}
		if got.totals != w.totals {
			t.Errorf("%s: stored totals %+v, want %+v", r.Tenant, got.totals, w.totals)
		}
		if !reflect.DeepEqual(got.mismatches, w.mismatches) {
			t.Errorf("%s: stored mismatches %v, want %v", r.Tenant, got.mismatches, w.mismatches)
		}
	}
	if oldestEvent.After(periodTo) || h.at(1).After(oldestEvent) {
		t.Errorf("the times of the scenario are out of order: %s, %s, %s", h.at(1), oldestEvent, periodTo)
	}
}

// S3-T615 — Req: Card Spend UC-4 rule 2a; S3 D-21, S3 D-25. Authorizations of A set DECLINED and DEBIT_LOST by the
// test while their Debited exists: UNEXPECTED_DEBIT in A's run with auth_id, chain_auth_id, tx_hash, actual_amount;
// nothing in the cas-platform run.
func TestT615_UnexpectedDebit(t *testing.T) {
	h := setup(t)
	w := h.wallet(t)
	hashes := map[string]common.Hash{}
	for i, c := range []struct {
		authID, status string
		amount         int64
	}{{"a-declined", "DECLINED", 6}, {"a-lost", "DEBIT_LOST", 7}} {
		row := h.authorization(t, "tenant-a", c.authID, c.status, units(c.amount), h.at(i), h.at(i))
		hash, block := h.debit(t, w, units(c.amount), h.chainID(t, "tenant-a", c.authID))
		h.operatorTx(t, "DEBIT", row, hash, "CONFIRMED", block)
		hashes[c.authID] = hash
	}
	h.sync(t)

	runs := h.reconcile(t)
	want := []reconcile.Mismatch{
		{Type: reconcile.UnexpectedDebit, AuthID: "a-declined", ChainAuthID: hexOf(h.chainID(t, "tenant-a", "a-declined")),
			TxHash: strings.ToLower(hashes["a-declined"].Hex()), ActualAmount: "6000000"},
		{Type: reconcile.UnexpectedDebit, AuthID: "a-lost", ChainAuthID: hexOf(h.chainID(t, "tenant-a", "a-lost")),
			TxHash: strings.ToLower(hashes["a-lost"].Hex()), ActualAmount: "7000000"},
	}
	if want[0].ChainAuthID > want[1].ChainAuthID {
		want[0], want[1] = want[1], want[0]
	}
	a := runOf(t, runs, "tenant-a")
	wantMismatches(t, a, want...)
	wantTotals(t, a, reconcile.Totals{AuthorizationsChecked: 2, DebitsCount: 2, DebitsAmount: "13000000", RefundsAmount: "0"})
	wantMismatches(t, runOf(t, runs, "cas-platform"))
}
