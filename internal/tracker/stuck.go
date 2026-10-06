package tracker

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"

	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/operator"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Rows 7 to 9 of UC-3 (FR-20), for debits, refunds and releases alike: an operator transaction that is not mined is
// replaced in its slot with both fees raised by fee_bump_percent; a debit past its validUntil is released by a
// zero-value transfer of the operator to itself; a PLANNED debit slot is released at once (D-22). An unfilled nonce
// would block every later operator transaction.

// refundStuckFactor: a refund has no deadline; it is replaced when pending longer than tracker_interval × 3.
const refundStuckFactor = 3

// sent records the time the tracker sent a transaction into a slot: the age of a pending transaction counts from it.
func (t *Tracker) sent(id uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastSent[id] = t.now()
}

// pendingSince is the time of the last send of a slot by this process, else the time the slot was reserved.
func (t *Tracker) pendingSince(id uuid.UUID, created time.Time) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if at, ok := t.lastSent[id]; ok && at.After(created) {
		return at
	}
	return created
}

// stuckPass runs rows 7 to 9 over every operator transaction that is not mined.
func (t *Tracker) stuckPass(ctx context.Context) {
	rows, err := repository.New(t.db).ListUnminedOperatorTxs(ctx, repository.ListUnminedOperatorTxsParams{
		ChainID: int64(t.queue.ChainID()), OperatorAddress: t.operator(),
	})
	if err != nil {
		t.logger.ErrorContext(ctx, "tracker: list the unmined operator transactions failed", "error", decision.ErrorDetail(err))
		return
	}
	if len(rows) == 0 {
		return
	}
	count, err := t.queue.NonceCount(ctx)
	if err != nil {
		t.logger.WarnContext(ctx, "tracker: stuck transactions not checked", "error", err.Error())
		return
	}
	_, chainTime, err := t.queue.Head(ctx)
	if err != nil {
		t.logger.WarnContext(ctx, "tracker: stuck transactions not checked", "error", err.Error())
		return
	}
	for _, row := range rows {
		if err := t.unmined(ctx, row, count, chainTime); err != nil {
			t.logger.WarnContext(ctx, "tracker: a stuck operator transaction was not handled", "nonce", row.Nonce,
				"purpose", row.Purpose, "error", err.Error())
		}
	}
}

func (t *Tracker) unmined(ctx context.Context, row repository.ListUnminedOperatorTxsRow, count, chainTime uint64) error {
	inFlight := row.AuthorizationStatus != nil &&
		(*row.AuthorizationStatus == decision.StatusReceived || *row.AuthorizationStatus == decision.StatusDebitSubmitted)
	slot := operator.Slot{ID: row.ID, Nonce: uint64(row.Nonce)}

	if row.Status == txPlanned {
		// Row 9 (D-22): a PLANNED debit slot without a hash is never sent as a debit. Not while UC-1 still owns it.
		if inFlight {
			return nil
		}
		_, err := t.queue.Send(ctx, slot, t.releaseCall(), nil)
		t.sent(row.ID)
		if err == nil {
			t.logger.InfoContext(ctx, "planned debit slot released", "nonce", row.Nonce)
		}
		return err
	}

	hash := common.BytesToHash(row.TxHash)
	if r := t.queue.Receipt(ctx, hash); r != nil {
		if row.Purpose == operator.PurposeRelease {
			return t.setTx(ctx, row.ID, txReleased, r)
		}
		return nil // debits and refunds are followed by their own rows
	}
	if uint64(row.Nonce) < count {
		// The slot is used on chain, by an earlier hash of the row.
		return t.resolveUsedSlot(ctx, row)
	}
	if inFlight {
		return nil // UC-1 is still waiting for this debit
	}

	age := t.now().Sub(t.pendingSince(row.ID, row.CreatedAt))
	switch row.Purpose {
	case operator.PurposeDebit:
		if row.ValidUntil != nil && chainTime > uint64(row.ValidUntil.Unix()) {
			// Row 8: past validUntil the debit can only revert: the slot is released.
			return t.replace(ctx, slot, hash, t.releaseCall(), "debit past validUntil released")
		}
		if age <= t.cfg.Interval {
			return nil
		}
		// Row 7. Read first: a debit already on chain by another hash is not sent again.
		debited, err := t.debited(ctx, row.ChainAuthID)
		if err != nil || debited.Sign() > 0 {
			return err
		}
		call, err := t.debitCall(row.WalletAddress, row.ChainAuthID, row.TokenAmount, *row.ValidUntil)
		if err != nil {
			return err
		}
		return t.replace(ctx, slot, hash, call, "stuck debit replaced")
	case operator.PurposeRefund:
		if age <= refundStuckFactor*t.cfg.Interval {
			return nil
		}
		used, err := t.refundUsed(ctx, row.ChainRefundID, nil)
		if err != nil || used {
			return err
		}
		return t.replace(ctx, slot, hash, t.refundCall(row.RefundAuthID, row.ChainRefundID, row.RefundAmount), "stuck refund replaced")
	case operator.PurposeRelease:
		if age <= t.cfg.Interval {
			return nil
		}
		return t.replace(ctx, slot, hash, t.releaseCall(), "stuck release replaced")
	}
	return nil
}

// replace sends call in the slot of hash with both fees at least fee_bump_percent above those of hash, or the market
// fees when they are higher. The fees of hash are read from the node, else taken from the last send of the slot by
// this process: a node may evict a transaction whose fee cap fell below the base fee. The old hash moves to
// replaced_hashes.
func (t *Tracker) replace(ctx context.Context, slot operator.Slot, hash common.Hash, call operator.Call, msg string) error {
	var floor *operator.Fees
	f := t.queue.PendingFees(ctx, hash)
	if f == nil {
		f = t.queue.SentFees(slot.ID)
	}
	if f != nil {
		b := f.Bumped(t.cfg.FeeBumpPercent)
		floor = &b
	}
	newHash, err := t.queue.SendWithFloor(ctx, slot, call, &hash, floor)
	t.sent(slot.ID)
	if err != nil {
		return err
	}
	t.logger.WarnContext(ctx, msg, "nonce", slot.Nonce, "purpose", call.Purpose, "old_tx_hash", hash.Hex(), "tx_hash", newHash.Hex())
	return nil
}

// releaseCall is a zero-value transfer of the operator to itself: it fills a nonce and does nothing else.
func (t *Tracker) releaseCall() operator.Call {
	return operator.Call{Purpose: operator.PurposeRelease, To: t.queue.Address(), Gas: releaseGas}
}

// resolveUsedSlot makes current the earlier hash of a row whose slot the chain used: a debit or refund that landed
// before its replacement or release did.
func (t *Tracker) resolveUsedSlot(ctx context.Context, row repository.ListUnminedOperatorTxsRow) error {
	for _, h := range row.ReplacedHashes {
		r := t.queue.Receipt(ctx, common.BytesToHash(h))
		if r == nil {
			continue
		}
		purpose := operator.PurposeDebit
		if row.ReturnRowID != nil {
			purpose = operator.PurposeRefund
		}
		status := txIncluded
		if r.Status != types.ReceiptStatusSuccessful {
			status = txReverted
		}
		p := repository.SetOperatorTxResolvedParams{ID: row.ID, TxHash: h, Purpose: purpose, Status: status}
		if r.BlockHash != (common.Hash{}) {
			n := r.BlockNumber.Int64()
			p.BlockNumber, p.BlockHash = &n, r.BlockHash.Bytes()
		}
		t.logger.InfoContext(ctx, "slot used by an earlier transaction", "nonce", row.Nonce, "tx_hash", common.BytesToHash(h).Hex())
		return dbErr(repository.New(t.db).SetOperatorTxResolved(ctx, p))
	}
	// The operator key was used outside card-auth. The row is left for manual handling (Deployment Guide); the alert
	// is written once per row by this process.
	t.mu.Lock()
	seen := t.foreignNonce[row.ID]
	t.foreignNonce[row.ID] = true
	t.mu.Unlock()
	if !seen {
		t.logger.ErrorContext(ctx, alert+"the operator key was used outside card-auth: a nonce of the operator is used on chain by none of the transactions of its row; left for manual handling",
			"nonce", row.Nonce, "purpose", row.Purpose, "tx_hash", common.BytesToHash(row.TxHash).Hex())
	}
	return nil
}
