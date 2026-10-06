// Package tracker is the background worker of card-auth (SRS — Card Spend UC-3). This first loop executes the
// returns of UC-2: it sends refund through the operator queue, follows each refund to CONFIRMED by the finality rule
// of the network, retries a failed one after return_retry_interval (FR-14), and on start reads the chain before it
// sends anything again (FR-16). The debit side of UC-3 — finality of debits, late and lost debits, stuck
// transactions — is S2 st6b.
//
// One card-auth instance runs one tracker (§3.2). It runs only in card-auth (ADR-3).
package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/operator"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/returns"
)

// Statuses of operator_txs (SRS — Card Spend §2.4).
const (
	txIncluded  = "INCLUDED"
	txConfirmed = "CONFIRMED"
	txReverted  = "REVERTED"
)

// pageSize bounds the returns sent in one cycle.
const pageSize = 50

// DB is what the tracker needs from the database, role cas_card_auth. *pgxpool.Pool satisfies it.
type DB interface {
	repository.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Config holds the values of SRS — Card Spend §3.1 the tracker uses.
type Config struct {
	Controller            common.Address
	Token                 common.Address
	RefundGasLimit        uint64        // refund_gas_limit
	Interval              time.Duration // tracker_interval
	RetryInterval         time.Duration // return_retry_interval
	FinalityMode          string        // finality_mode: confirmations or tag
	FinalityTag           string        // finality_tag: finalized or safe
	FinalityConfirmations int           // finality_confirmations
}

// Tracker executes returns.
type Tracker struct {
	db       DB
	queue    *operator.Queue
	cfg      Config
	metrics  *Metrics
	logger   *slog.Logger
	now      func() time.Time
	abi      *abi.ABI
	treasury common.Address
}

// New returns a tracker. now is the clock of the retry interval and of created_at.
func New(db DB, queue *operator.Queue, cfg Config, metrics *Metrics, logger *slog.Logger, now func() time.Time) (*Tracker, error) {
	a, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("tracker: controller ABI: %w", err)
	}
	return &Tracker{db: db, queue: queue, cfg: cfg, metrics: metrics, logger: logger, now: now, abi: a}, nil
}

// Run starts the tracker and runs a cycle every tracker_interval until ctx ends.
func (t *Tracker) Run(ctx context.Context) {
	if err := t.Start(ctx); err != nil {
		t.logger.ErrorContext(ctx, "tracker start failed; the cycles go on", "error", err.Error())
	}
	ticker := time.NewTicker(t.cfg.Interval)
	defer ticker.Stop()
	for {
		t.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Start is the start of card-auth for returns (UC-3 row 9, FR-16): the treasury address is read; for every return
// in SUBMITTED or INCLUDED refundUsed(refundId) is read first. Used → INCLUDED, CONFIRMED by the cycles, never sent
// again. Not used → sent again: in the same nonce slot while that slot is still open on chain, else as a new attempt.
func (t *Tracker) Start(ctx context.Context) error {
	treasury, err := t.controller().Treasury(t.opts(ctx, nil))
	if err != nil {
		return fmt.Errorf("treasury(): %s", t.describe(err))
	}
	t.treasury = treasury

	rows, err := repository.New(t.db).ListReturnsInFlight(ctx)
	if err != nil {
		return fmt.Errorf("list the returns in flight: %s", decision.ErrorDetail(err))
	}
	var errs []error
	for _, r := range rows {
		if err := t.restart(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("return %s: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (t *Tracker) restart(ctx context.Context, r repository.ListReturnsInFlightRow) error {
	used, err := t.refundUsed(ctx, r.ChainRefundID, nil)
	if err != nil {
		return err
	}
	if used {
		if r.Status == returns.StatusSubmitted {
			return t.included(ctx, r, nil)
		}
		return nil
	}
	if r.Status == returns.StatusSubmitted && r.TxHash != nil {
		if receipt := t.queue.Receipt(ctx, common.BytesToHash(r.TxHash)); receipt != nil {
			// The refund landed and reverted: the cycle retries it.
			return t.follow(ctx, r)
		}
		count, err := t.queue.NonceCount(ctx)
		if err != nil {
			return err
		}
		if uint64(r.Nonce) >= count {
			// The slot is still open on chain: the same nonce, signed again; the old hash moves to replaced_hashes.
			previous := common.BytesToHash(r.TxHash)
			_, err := t.queue.Send(ctx, operator.Slot{ID: r.TxID, Nonce: uint64(r.Nonce)}, t.refundCall(r.ChainAuthID, r.ChainRefundID, r.TokenAmount), &previous)
			return err
		}
	}
	// The slot is used by another transaction, or the refund is gone: a new attempt at once.
	if err := t.setStatus(ctx, r.ID, returns.StatusRetrying, 0, r.Status); err != nil {
		return err
	}
	return t.attempt(ctx, r.ID, r.ChainAuthID, r.ChainRefundID, r.TokenAmount)
}

// Cycle runs one tracker cycle: send the due returns, follow the returns in flight, update the metrics. Failures
// are logged; the next cycle tries again.
func (t *Tracker) Cycle(ctx context.Context) {
	q := repository.New(t.db)
	due, err := q.ListReturnsToSend(ctx, repository.ListReturnsToSendParams{
		DueBefore: t.now().Add(-t.cfg.RetryInterval), PageLimit: pageSize,
	})
	if err != nil {
		t.logger.ErrorContext(ctx, "tracker: list the returns to send failed", "error", decision.ErrorDetail(err))
	}
	for _, r := range due {
		if err := t.attempt(ctx, r.ID, r.ChainAuthID, r.ChainRefundID, r.TokenAmount); err != nil {
			t.logger.ErrorContext(ctx, "tracker: sending a refund failed", "return_row_id", r.ID.String(), "error", err.Error())
		}
	}

	inFlight, err := q.ListReturnsInFlight(ctx)
	if err != nil {
		t.logger.ErrorContext(ctx, "tracker: list the returns in flight failed", "error", decision.ErrorDetail(err))
	}
	for _, r := range inFlight {
		if err := t.follow(ctx, r); err != nil {
			t.logger.WarnContext(ctx, "tracker: following a refund failed", "return_row_id", r.ID.String(), "error", err.Error())
		}
	}
	t.updateMetrics(ctx)
}

// attempt sends one refund (UC-2 step 7): the slot is reserved through the operator queue — or the PLANNED slot of an
// earlier attempt that was never sent is used again, so no nonce is left unused — the return SUBMITTED with
// attempts + 1, then the hash stored and the transaction sent. A failure before the send makes it RETRYING.
func (t *Tracker) attempt(ctx context.Context, id uuid.UUID, chainAuthID, chainRefundID []byte, tokenAmount string) error {
	var slot operator.Slot
	var skip bool
	err := pgx.BeginFunc(ctx, t.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		r, err := q.LockReturnToSend(ctx, id)
		if err != nil {
			return err
		}
		if r.Status != returns.StatusAccepted && r.Status != returns.StatusRetrying {
			skip = true
			return nil
		}
		if r.PlannedNonce >= 0 {
			slot = operator.Slot{ID: r.PlannedID, Nonce: uint64(r.PlannedNonce)}
		} else if slot, err = t.queue.Reserve(ctx, q, operator.PurposeRefund, nil, &id); err != nil {
			return err
		}
		n, err := q.SetReturnStatus(ctx, repository.SetReturnStatusParams{
			ID: id, Status: returns.StatusSubmitted, Attempt: 1, FromStatuses: []string{r.Status},
		})
		if err == nil && n != 1 {
			err = errors.New("the return changed")
		}
		return err
	})
	if err != nil {
		return errors.New(decision.ErrorDetail(err))
	}
	if skip {
		return nil
	}
	if _, err := t.queue.Send(ctx, slot, t.refundCall(chainAuthID, chainRefundID, tokenAmount), nil); err != nil {
		// Nothing was sent: the slot stays PLANNED and is used by the next attempt (step 8, EC-12).
		t.logger.WarnContext(ctx, "refund not sent; retrying", "return_row_id", id.String(), "nonce", slot.Nonce, "error", err.Error())
		return t.setStatus(ctx, id, returns.StatusRetrying, 0, returns.StatusSubmitted)
	}
	return nil
}

// refundCall is refund(authId, refundId, amount) of the controller.
func (t *Tracker) refundCall(chainAuthID, chainRefundID []byte, tokenAmount string) operator.Call {
	amount, _ := new(big.Int).SetString(tokenAmount, 10)
	data, _ := t.abi.Pack("refund", [32]byte(chainAuthID), [32]byte(chainRefundID), amount)
	return operator.Call{Purpose: operator.PurposeRefund, To: t.cfg.Controller, Data: data, Gas: t.cfg.RefundGasLimit}
}

// follow moves a return in flight: SUBMITTED by its receipt or by refundUsed, INCLUDED to CONFIRMED by the finality
// rule.
func (t *Tracker) follow(ctx context.Context, r repository.ListReturnsInFlightRow) error {
	switch r.Status {
	case returns.StatusSubmitted:
		if r.TxHash == nil {
			// SUBMITTED with a slot that was never sent: a stop between the reservation and the send.
			return t.setStatus(ctx, r.ID, returns.StatusRetrying, 0, returns.StatusSubmitted)
		}
		receipt := t.queue.Receipt(ctx, common.BytesToHash(r.TxHash))
		if receipt == nil {
			used, err := t.refundUsed(ctx, r.ChainRefundID, nil)
			if err != nil {
				return err
			}
			if used {
				// Another transaction of this return landed, a replaced hash of the slot among them.
				return t.included(ctx, r, nil)
			}
			t.stuck(ctx, r)
			return nil
		}
		if receipt.Status == types.ReceiptStatusSuccessful {
			return t.included(ctx, r, receipt)
		}
		if err := t.setTx(ctx, r.TxID, txReverted, receipt); err != nil {
			return err
		}
		// A reverted refund of a used refundId: it is already on chain (FR-13). Never sent again.
		used, err := t.refundUsed(ctx, r.ChainRefundID, nil)
		if err != nil {
			return err
		}
		if used {
			return t.setStatus(ctx, r.ID, returns.StatusIncluded, 0, returns.StatusSubmitted)
		}
		t.logger.WarnContext(ctx, "refund reverted; retrying after return_retry_interval", "return_row_id", r.ID.String(),
			"tx_hash", receipt.TxHash.Hex())
		return t.setStatus(ctx, r.ID, returns.StatusRetrying, 0, returns.StatusSubmitted)

	case returns.StatusIncluded:
		block, final, err := t.finalBlock(ctx)
		if err != nil || !final {
			return err
		}
		used, err := t.refundUsed(ctx, r.ChainRefundID, block)
		if err != nil || !used {
			// Not final yet; a refund that disappeared from the chain is the reorg handling of st6b.
			return err
		}
		if r.TxStatus == txIncluded {
			if err := t.setTx(ctx, r.TxID, txConfirmed, nil); err != nil {
				return err
			}
		}
		return t.setStatus(ctx, r.ID, returns.StatusConfirmed, 0, returns.StatusIncluded)
	}
	return nil
}

// stuck is the seam for st6b: a refund without a receipt and not on chain — replacement with a higher fee on the same
// nonce (UC-3 row 7), release of the nonce (row 8). Until then it is only waited for.
func (t *Tracker) stuck(ctx context.Context, r repository.ListReturnsInFlightRow) {
	t.logger.DebugContext(ctx, "refund not mined yet", "return_row_id", r.ID.String(), "nonce", r.Nonce)
}

// included sets SUBMITTED → INCLUDED; with a receipt its transaction INCLUDED, the block only from a sealed receipt
// (UC-3 row 10).
func (t *Tracker) included(ctx context.Context, r repository.ListReturnsInFlightRow, receipt *types.Receipt) error {
	if receipt != nil {
		if err := t.setTx(ctx, r.TxID, txIncluded, receipt); err != nil {
			return err
		}
	}
	return t.setStatus(ctx, r.ID, returns.StatusIncluded, 0, returns.StatusSubmitted)
}

func (t *Tracker) setStatus(ctx context.Context, id uuid.UUID, status string, attempt int32, from ...string) error {
	_, err := repository.New(t.db).SetReturnStatus(ctx, repository.SetReturnStatusParams{
		ID: id, Status: status, Attempt: attempt, FromStatuses: from,
	})
	if err != nil {
		return errors.New(decision.ErrorDetail(err))
	}
	return nil
}

func (t *Tracker) setTx(ctx context.Context, id uuid.UUID, status string, receipt *types.Receipt) error {
	p := repository.SetOperatorTxStatusParams{ID: id, Status: status}
	if receipt != nil && receipt.BlockHash != (common.Hash{}) && receipt.BlockNumber != nil && receipt.BlockNumber.IsInt64() {
		n := receipt.BlockNumber.Int64()
		p.BlockNumber, p.BlockHash = &n, receipt.BlockHash.Bytes()
	}
	if err := repository.New(t.db).SetOperatorTxStatus(ctx, p); err != nil {
		return errors.New(decision.ErrorDetail(err))
	}
	return nil
}

// finalBlock is the block at which a state is final by the finality rule of the network: the block finality_tag in
// mode tag; the latest block − finality_confirmations in mode confirmations (S2-T510: final after N blocks on top).
// final is false while the chain is shorter than that.
func (t *Tracker) finalBlock(ctx context.Context) (*big.Int, bool, error) {
	if t.cfg.FinalityMode == config.FinalityModeTag {
		if t.cfg.FinalityTag == config.FinalityTagSafe {
			return big.NewInt(int64(rpc.SafeBlockNumber)), true, nil
		}
		return big.NewInt(int64(rpc.FinalizedBlockNumber)), true, nil
	}
	ep := t.queue.Reader().Endpoint()
	latest, err := ep.Client.BlockNumber(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("block number: %s", t.describe(err))
	}
	n := uint64(t.cfg.FinalityConfirmations)
	if latest < n {
		return nil, false, nil
	}
	return new(big.Int).SetUint64(latest - n), true, nil
}

// refundUsed reads refundUsed(refundId) of the controller at block; nil is the latest block.
func (t *Tracker) refundUsed(ctx context.Context, refundID []byte, block *big.Int) (bool, error) {
	used, err := t.controller().RefundUsed(t.opts(ctx, block), [32]byte(refundID))
	if err != nil {
		return false, fmt.Errorf("refundUsed: %s", t.describe(err))
	}
	return used, nil
}

func (t *Tracker) controller() *bindings.CardSpendControllerCaller {
	c, _ := bindings.NewCardSpendControllerCaller(t.cfg.Controller, t.queue.Reader().Endpoint().Client)
	return c
}

func (t *Tracker) opts(ctx context.Context, block *big.Int) *bind.CallOpts {
	return &bind.CallOpts{Context: ctx, BlockNumber: block}
}

func (t *Tracker) describe(err error) string {
	return t.queue.Reader().Describe(err, t.cfg.Interval)
}

// CloseOpenReturns is the exception of FR-14 for a DEBIT_LOST authorization (UC-3 row 3): its open returns close as
// NOTHING_TO_RETURN — no tokens were debited — and returned_amount drops by their amounts. Called by the debit side
// of the tracker (st6b) in the transaction that sets DEBIT_LOST.
func CloseOpenReturns(ctx context.Context, q *repository.Queries, authorizationID uuid.UUID) (int64, error) {
	return q.CloseOpenReturns(ctx, authorizationID)
}
