package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/debit"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/operator"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/returns"
)

// The debit side of UC-3 (S2 st6b): rows 1 to 6, 9, 11 and 12 per authorization; rows 7 to 9 per operator
// transaction in stuck.go. Every decision about an authId reads authorizations(authId) or a receipt first (FR-16).

const (
	txSent     = "SENT"
	txPlanned  = "PLANNED"
	txReleased = "RELEASED"
	releaseGas = 21000
)

// alert starts the message of every log line that needs a person: DEBIT_LOST, a debit resubmitted after its
// validUntil, a nonce used outside card-auth.
const alert = "ALERT: "

// grace is the time after deadline_at a request has to store its own outcome (D-21) before the tracker treats the
// authorization as left behind (rows 11 and 12).
const grace = decision.OutcomeWriteTimeout

// LateDebitReturnID is the return_id of the automatic return of a late debit (row 4): derived from the authorization,
// so a restart cannot create a second one, and longer than 64 characters, so it never meets a processor's return_id.
func LateDebitReturnID(chainAuthID []byte) string {
	return "LATE_DEBIT:" + common.Bytes2Hex(chainAuthID)
}

// debitPass runs the rows of every authorization that is not final. APPROVED rows go last, in descending order of
// the block of their debit: the reorg check of the highest block covers the lower ones (budget.go).
func (t *Tracker) debitPass(ctx context.Context) {
	rows, err := repository.New(t.db).ListAuthorizationsToTrack(ctx, repository.ListAuthorizationsToTrackParams{
		ChainID: t.chainID(), OperatorAddress: t.operator(),
	})
	if err != nil {
		t.failed(ctx, slog.LevelError, "tracker: list the authorizations failed", "error", decision.ErrorDetail(err))
		return
	}
	notMoved := func(a repository.ListAuthorizationsToTrackRow, err error) {
		t.rowFailed(slog.LevelWarn, "tracker: an authorization was not moved", err, "tenant_id", a.TenantID.String(),
			"auth_id", a.AuthID, "status", a.Status)
	}
	type approvedRow struct {
		a     repository.ListAuthorizationsToTrackRow
		txs   []repository.ListAuthorizationTxsRow
		block int64
	}
	var approved []approvedRow
	for _, a := range rows {
		if t.limited() {
			return
		}
		if a.Status != decision.StatusApproved {
			if err := t.trackAuthorization(ctx, a); err != nil {
				notMoved(a, err)
			}
			continue
		}
		txs, err := repository.New(t.db).ListAuthorizationTxs(ctx, &a.ID)
		if err != nil {
			notMoved(a, dbErr(err))
			continue
		}
		block := int64(-1)
		if cur := currentTx(txs); cur != nil {
			block = blockOf(cur.BlockNumber)
		}
		approved = append(approved, approvedRow{a: a, txs: txs, block: block})
	}
	sort.SliceStable(approved, func(i, j int) bool { return approved[i].block > approved[j].block })
	for _, r := range approved {
		if t.limited() {
			return
		}
		if err := t.approved(ctx, r.a, r.txs); err != nil {
			notMoved(r.a, err)
		}
	}
}

func (t *Tracker) trackAuthorization(ctx context.Context, a repository.ListAuthorizationsToTrackRow) error {
	pastDeadline := a.DeadlineAt != nil && t.now().After(a.DeadlineAt.Add(grace))
	switch a.Status {
	case decision.StatusReceived:
		// Row 12: a decline of steps 5 to 9a that was not stored. Nothing was reserved or sent.
		if !pastDeadline {
			return nil
		}
		return t.move(ctx, a, decision.StatusDeclined, moveArgs{reason: decision.ReasonTimeout}, nil)
	case decision.StatusDebitSubmitted:
		// Row 11 (D-21): the outcome was not stored; the processor was told DECLINED / TIMEOUT or nothing.
		if !pastDeadline {
			return nil
		}
		if err := t.move(ctx, a, decision.StatusTimedOut, moveArgs{reason: decision.ReasonTimeout}, nil); err != nil {
			return err
		}
		a.Status = decision.StatusTimedOut
		return t.timedOut(ctx, a)
	case decision.StatusTimedOut:
		return t.timedOut(ctx, a)
	case decision.StatusApproved:
		txs, err := repository.New(t.db).ListAuthorizationTxs(ctx, &a.ID)
		if err != nil {
			return dbErr(err)
		}
		return t.approved(ctx, a, txs)
	case decision.StatusLateDebit:
		// Row 5.
		status, err := repository.New(t.db).GetLateDebitReturnStatus(ctx, a.ID)
		if err != nil || status != returns.StatusConfirmed {
			return dbErr(ignoreNoRows(err))
		}
		return t.move(ctx, a, decision.StatusLateDebitRefunded, moveArgs{}, nil)
	}
	return nil
}

// timedOut is rows 4, 6 and 9 for a TIMED_OUT authorization.
func (t *Tracker) timedOut(ctx context.Context, a repository.ListAuthorizationsToTrackRow) error {
	debited, err := t.debited(ctx, a.ChainAuthID)
	if err != nil {
		return err
	}
	txs, err := repository.New(t.db).ListAuthorizationTxs(ctx, &a.ID)
	if err != nil {
		return dbErr(err)
	}

	// Row 4: the debit landed after the decline.
	if debited.Sign() > 0 {
		if err := t.resolveDebit(ctx, txs); err != nil {
			return err
		}
		returnID := LateDebitReturnID(a.ChainAuthID)
		amount := debited.String()
		err := t.move(ctx, a, decision.StatusLateDebit, moveArgs{debited: &amount, returnedAdd: &amount},
			func(q *repository.Queries) error {
				_, err := q.InsertReturn(ctx, repository.InsertReturnParams{
					TenantID: a.TenantID, AuthorizationID: a.ID, ReturnID: returnID,
					ChainRefundID: decision.ChainRefundID(a.TenantID, returnID).Bytes(), Type: returns.TypeLateDebit,
					TokenAmount: amount, Status: returns.StatusAccepted, CreatedAt: t.now(),
				})
				return err
			})
		if err != nil {
			return err
		}
		t.metrics.lateDebits.Inc()
		t.logger.ErrorContext(ctx, "late debit: the debit landed after the decline; returned in full automatically",
			"tenant_id", a.TenantID.String(), "auth_id", a.AuthID, "token_amount", amount, "return_id", returnID)
		return nil
	}

	// Row 9 (D-22): a PLANNED slot is never sent as a debit; stuck.go releases its nonce.
	for _, tx := range txs {
		if tx.Status == txPlanned {
			return t.declineTimedOut(ctx, a)
		}
	}
	// Row 6: the debit reverted, or the chain is past validUntil and the debit is not on it.
	if cur := currentTx(txs); cur != nil && cur.TxHash != nil {
		r, err := t.receipt(ctx, common.BytesToHash(cur.TxHash))
		if err != nil {
			return err
		}
		if r != nil && r.Status != types.ReceiptStatusSuccessful {
			if err := t.setTx(ctx, cur.ID, txReverted, r); err != nil {
				return err
			}
			return t.declineTimedOut(ctx, a)
		}
	}
	_, ts, err := t.head(ctx)
	if err != nil {
		return err
	}
	if a.ValidUntil != nil && ts > uint64(a.ValidUntil.Unix()) {
		// An unmined debit of this authorization is released by stuck.go (row 8).
		return t.declineTimedOut(ctx, a)
	}
	return nil
}

func (t *Tracker) declineTimedOut(ctx context.Context, a repository.ListAuthorizationsToTrackRow) error {
	return t.move(ctx, a, decision.StatusDeclined, moveArgs{reason: decision.ReasonTimeout}, nil)
}

// approved is rows 1, 2, 3 and 10 and the reorg check for an APPROVED authorization with its operator transactions.
func (t *Tracker) approved(ctx context.Context, a repository.ListAuthorizationsToTrackRow, txs []repository.ListAuthorizationTxsRow) error {
	cur := currentTx(txs)
	if cur == nil {
		return errors.New("an APPROVED authorization without a debit transaction")
	}
	switch {
	case cur.Purpose == operator.PurposeRelease:
		// A resubmission that passed its validUntil unmined and was released (row 8): not executed only because of
		// its validUntil (D-23). The next one waits for the release to fill the slot.
		if cur.Status != txReleased {
			return nil
		}
		return t.notExecuted(ctx, a, txs, cur, "released unmined")
	case cur.Status == txSent:
		// A resubmission in flight (row 2); stuck.go replaces or releases it.
		r, err := t.receipt(ctx, common.BytesToHash(cur.TxHash))
		if err != nil || r == nil {
			return err
		}
		if r.Status == types.ReceiptStatusSuccessful {
			return t.setTx(ctx, cur.ID, txIncluded, r)
		}
		if err := t.setTx(ctx, cur.ID, txReverted, r); err != nil {
			return err
		}
		return t.reverted(ctx, a, txs, cur, r)
	case cur.Status == txReverted:
		r, err := t.receipt(ctx, common.BytesToHash(cur.TxHash))
		if err != nil || r == nil {
			return err
		}
		return t.reverted(ctx, a, txs, cur, r)
	case cur.Status == txIncluded && cur.BlockNumber != nil:
		block := uint64(*cur.BlockNumber)
		final, ok, err := t.finalNumber(ctx)
		if err != nil {
			return err
		}
		if !ok || block > final {
			// Not final: no chain call for finality; only the shared reorg check (S2 st9b b, c).
			same, err := t.unchanged(ctx, block, cur.BlockHash)
			if err != nil || same {
				return err
			}
		} else {
			// Row 1 (FR-19): final by the finality rule of the network, counted from the sealed block. The check right
			// before the row is set final reads its block.
			onChain, err := t.blockHash(ctx, block)
			if err != nil {
				return err
			}
			if onChain == common.BytesToHash(cur.BlockHash) {
				return t.move(ctx, a, decision.StatusDebitConfirmed, moveArgs{}, func(q *repository.Queries) error {
					return q.SetOperatorTxStatus(ctx, repository.SetOperatorTxStatusParams{ID: cur.ID, Status: txConfirmed})
				})
			}
		}
		// ADR-10: the stored block is no longer on the chain. The state is read again before anything is decided.
		t.logger.WarnContext(ctx, "reorg: the block of an approved debit changed", "auth_id", a.AuthID,
			"block_number", *cur.BlockNumber)
	}

	// Row 10, reorg, row 2: the receipt is read again.
	r, err := t.receipt(ctx, common.BytesToHash(cur.TxHash))
	if err != nil {
		return err
	}
	if r != nil && r.Status == types.ReceiptStatusSuccessful {
		return t.setBlock(ctx, cur.ID, r)
	}
	debited, err := t.debited(ctx, a.ChainAuthID)
	if err != nil {
		return err
	}
	if debited.Sign() > 0 {
		// On chain by another hash, or the receipt is not served yet: keep waiting.
		return t.resolveDebit(ctx, txs)
	}
	return t.resubmit(ctx, a, cur)
}

// resubmit is row 2 (FR-18, EC-7): the dropped approved debit is sent again with the same authId and a new validUntil,
// in the same nonce slot while the chain has not used it, otherwise in a new slot.
func (t *Tracker) resubmit(ctx context.Context, a repository.ListAuthorizationsToTrackRow, cur *repository.ListAuthorizationTxsRow) error {
	validUntil := debit.ValidUntil(t.now(), t.cfg.DebitValidity)
	if err := repository.New(t.db).SetAuthorizationValidUntil(ctx, repository.SetAuthorizationValidUntilParams{
		ID: a.ID, ValidUntil: &validUntil,
	}); err != nil {
		return dbErr(err)
	}
	call, err := t.debitCall(a.WalletAddress, a.ChainAuthID, a.TokenAmount, validUntil)
	if err != nil {
		return err
	}
	count, err := t.nonceCount(ctx)
	if err != nil {
		return err
	}
	var hash common.Hash
	if uint64(cur.Nonce) >= count {
		previous := common.BytesToHash(cur.TxHash)
		hash, err = t.queue.Send(ctx, operator.Slot{ID: cur.ID, Nonce: uint64(cur.Nonce)}, call, &previous)
		t.sent(cur.ID)
	} else {
		var slot operator.Slot
		err = pgx.BeginFunc(ctx, t.db, func(tx pgx.Tx) error {
			var err error
			id := a.ID
			slot, err = t.queue.Reserve(ctx, repository.New(tx), operator.PurposeDebit, &id, nil)
			return err
		})
		if err != nil {
			return dbErr(err)
		}
		hash, err = t.queue.Send(ctx, slot, call, nil)
		t.sent(slot.ID)
	}
	if err := t.check(err); err != nil {
		return err
	}
	t.logger.WarnContext(ctx, "approved debit dropped; resubmitted with the same authId", "tenant_id", a.TenantID.String(),
		"auth_id", a.AuthID, "tx_hash", hash.Hex(), "valid_until", validUntil)
	return nil
}

// reverted is row 3 for a resubmitted debit that reverted (D-23): in a block whose timestamp is past the stored
// valid_until it did not execute only because of its validUntil and goes back to row 2; before it, the contract
// refused it and the debit is lost. The revert reason is not decoded.
func (t *Tracker) reverted(ctx context.Context, a repository.ListAuthorizationsToTrackRow, txs []repository.ListAuthorizationTxsRow,
	cur *repository.ListAuthorizationTxsRow, r *types.Receipt) error {
	ts, err := t.blockTime(ctx, r.BlockNumber)
	if err != nil {
		return err // a preconfirmed block without a header yet: the next cycle reads it
	}
	if a.ValidUntil != nil && ts > uint64(a.ValidUntil.Unix()) {
		return t.notExecuted(ctx, a, txs, cur, "reverted past validUntil")
	}
	return t.resubmissionFailed(ctx, a, txs)
}

// notExecuted is D-23: a resubmitted debit that did not execute only because its validUntil passed. With
// authorizations(authId) at 0 it is resubmitted with the same authId and a new validUntil, with an alert on every
// attempt. The authorization stays APPROVED.
func (t *Tracker) notExecuted(ctx context.Context, a repository.ListAuthorizationsToTrackRow, txs []repository.ListAuthorizationTxsRow,
	cur *repository.ListAuthorizationTxsRow, how string) error {
	debited, err := t.debited(ctx, a.ChainAuthID)
	if err != nil {
		return err
	}
	if debited.Sign() > 0 {
		return t.resolveDebit(ctx, txs)
	}
	t.logger.ErrorContext(ctx, alert+"the resubmitted debit of an approved authorization did not execute before its validUntil; resubmitted again",
		"tenant_id", a.TenantID.String(), "auth_id", a.AuthID, "token_amount", a.TokenAmount, "how", how, "nonce", cur.Nonce)
	return t.resubmit(ctx, a, cur)
}

// resubmissionFailed is row 3: the debit is on chain after all — by an earlier hash — or it is lost.
func (t *Tracker) resubmissionFailed(ctx context.Context, a repository.ListAuthorizationsToTrackRow, txs []repository.ListAuthorizationTxsRow) error {
	debited, err := t.debited(ctx, a.ChainAuthID)
	if err != nil {
		return err
	}
	if debited.Sign() > 0 {
		return t.resolveDebit(ctx, txs)
	}
	zero := "0"
	err = t.move(ctx, a, decision.StatusDebitLost, moveArgs{debited: &zero}, func(q *repository.Queries) error {
		_, err := CloseOpenReturns(ctx, q, a.ID)
		return err
	})
	if err != nil {
		return err
	}
	t.metrics.debitsLost.Inc()
	t.logger.ErrorContext(ctx, alert+"DEBIT_LOST: the approved debit was dropped and its resubmission failed; issuer exposure",
		"tenant_id", a.TenantID.String(), "auth_id", a.AuthID, "token_amount", a.TokenAmount)
	return nil
}

// resolveDebit finds the successful receipt of the debit among every hash of the authorization's slots and makes that
// hash current, INCLUDED with its block. It does nothing when none is found yet.
func (t *Tracker) resolveDebit(ctx context.Context, txs []repository.ListAuthorizationTxsRow) error {
	for _, tx := range txs {
		for _, h := range append([][]byte{tx.TxHash}, tx.ReplacedHashes...) {
			if h == nil {
				continue
			}
			r, err := t.receipt(ctx, common.BytesToHash(h))
			if err != nil {
				return err
			}
			if r == nil || r.Status != types.ReceiptStatusSuccessful {
				continue
			}
			p := repository.SetOperatorTxResolvedParams{ID: tx.ID, TxHash: h, Purpose: operator.PurposeDebit, Status: txIncluded}
			if r.BlockHash != (common.Hash{}) {
				n := r.BlockNumber.Int64()
				p.BlockNumber, p.BlockHash = &n, r.BlockHash.Bytes()
			}
			return dbErr(repository.New(t.db).SetOperatorTxResolved(ctx, p))
		}
	}
	return nil
}

// debited reads debited of authorizations(authId) at the latest block.
func (t *Tracker) debited(ctx context.Context, chainAuthID []byte) (*big.Int, error) {
	if t.limited() {
		return nil, errRateLimited
	}
	a, err := t.controller().Authorizations(t.opts(ctx, nil), [32]byte(chainAuthID))
	if err != nil {
		return nil, t.rpcErr("authorizations(authId)", err)
	}
	return a.Debited, nil
}

// debitCall is debit(wallet, amount, authId, validUntil) of the controller.
func (t *Tracker) debitCall(wallet, chainAuthID []byte, tokenAmount string, validUntil time.Time) (operator.Call, error) {
	amount, ok := new(big.Int).SetString(tokenAmount, 10)
	if !ok || len(wallet) != common.AddressLength {
		return operator.Call{}, errors.New("the authorization has no wallet or token amount")
	}
	data, err := t.abi.Pack("debit", common.BytesToAddress(wallet), amount, [32]byte(chainAuthID), uint64(validUntil.Unix()))
	if err != nil {
		return operator.Call{}, err
	}
	return operator.Call{Purpose: operator.PurposeDebit, To: t.cfg.Controller, Data: data, Gas: t.cfg.DebitGasLimit}, nil
}

// moveArgs are the values a status change stores besides the status; nil keeps the stored value.
type moveArgs struct {
	reason      string
	debited     *string
	returnedAdd *string
}

// move changes the status of an authorization from its current one with its event, and runs also in the same
// transaction.
func (t *Tracker) move(ctx context.Context, a repository.ListAuthorizationsToTrackRow, to string, m moveArgs, also func(q *repository.Queries) error) error {
	now := t.now()
	err := pgx.BeginFunc(ctx, t.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		var reason *string
		if m.reason != "" {
			reason = &m.reason
		}
		n, err := q.MoveAuthorization(ctx, repository.MoveAuthorizationParams{
			ID: a.ID, FromStatus: a.Status, ToStatus: to, DeclineReason: reason, DebitedAmount: m.debited,
			ReturnedAdd: m.returnedAdd, DecidedAt: &now,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("the authorization is no longer %s", a.Status)
		}
		if also != nil {
			if err := also(q); err != nil {
				return err
			}
		}
		from := a.Status
		return q.InsertAuthorizationEvent(ctx, repository.InsertAuthorizationEventParams{
			AuthorizationID: a.ID, FromStatus: &from, ToStatus: to, Reason: reason, CreatedAt: now,
		})
	})
	if err != nil {
		return dbErr(err)
	}
	t.logger.InfoContext(ctx, "tracker: authorization moved", "auth_id", a.AuthID, "from", a.Status, "to", to)
	return nil
}

// setBlock stores the block of a receipt on an included transaction: null for a preconfirmed one (row 10).
func (t *Tracker) setBlock(ctx context.Context, id uuid.UUID, r *types.Receipt) error {
	p := repository.SetOperatorTxBlockParams{ID: id}
	if r.BlockHash != (common.Hash{}) {
		n := r.BlockNumber.Int64()
		p.BlockNumber, p.BlockHash = &n, r.BlockHash.Bytes()
	}
	return dbErr(repository.New(t.db).SetOperatorTxBlock(ctx, p))
}

// currentTx is the newest slot of an authorization that is not PLANNED.
func currentTx(txs []repository.ListAuthorizationTxsRow) *repository.ListAuthorizationTxsRow {
	for i := range txs {
		if txs[i].Status != txPlanned {
			return &txs[i]
		}
	}
	return nil
}

func dbErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(decision.ErrorDetail(err))
}

func ignoreNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
