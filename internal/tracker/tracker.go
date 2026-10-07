// Package tracker is the background worker of card-auth (SRS — Card Spend UC-3). Every tracker_interval it follows
// the debits to finality (rows 1 to 6, 10 to 12, debits.go), replaces or releases stuck operator transactions (rows 7
// to 9, stuck.go), and executes the returns of UC-2: it sends refund through the operator queue, follows each refund to
// CONFIRMED by the finality rule of the network and retries a failed one after return_retry_interval (FR-14). On start
// it reads the chain before it sends anything again (FR-16).
//
// One card-auth instance runs one tracker (§3.2). It runs only in card-auth (ADR-3).
package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
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
	DebitGasLimit         uint64        // debit_gas_limit, of a resubmitted or replaced debit
	DebitValidity         time.Duration // debit_validity: validUntil of a resubmitted debit
	FeeBumpPercent        int           // fee_bump_percent of a replacement
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

	mu          sync.Mutex
	lastSent    map[uuid.UUID]time.Time // slot → last send by this process (stuck.go)
	lastAttempt map[uuid.UUID]time.Time // return → last attempt by this process

	foreignNonce map[uuid.UUID]bool // slots used on chain outside card-auth, alerted once

	cyc *cycle // the RPC budget of the cycle that runs (budget.go)

	metricsReadAt time.Time // the last chain read of the metrics (metrics.go); zero before the first cycle
}

// New returns a tracker. now is the clock of the retry interval and of created_at.
func New(db DB, queue *operator.Queue, cfg Config, metrics *Metrics, logger *slog.Logger, now func() time.Time) (*Tracker, error) {
	a, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("tracker: controller ABI: %w", err)
	}
	return &Tracker{db: db, queue: queue, cfg: cfg, metrics: metrics, logger: logger, now: now, abi: a,
		lastSent: map[uuid.UUID]time.Time{}, lastAttempt: map[uuid.UUID]time.Time{}, foreignNonce: map[uuid.UUID]bool{}, cyc: newCycle()}, nil
}

// failed logs a failed read or write of the start or of a cycle at level. When ctx has ended (shutdown), the failure
// is the cancellation itself: nothing is logged and the cycle ends quietly. A failure while ctx is alive is logged.
func (t *Tracker) failed(ctx context.Context, level slog.Level, msg string, args ...any) {
	if ctx.Err() != nil {
		return
	}
	t.logger.Log(ctx, level, msg, args...)
}

// Run starts the tracker and runs a cycle every tracker_interval until ctx ends.
func (t *Tracker) Run(ctx context.Context) {
	if err := t.Start(ctx); err != nil {
		t.failed(ctx, slog.LevelError, "tracker start failed; the cycles go on", "error", err.Error())
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
	t.begin()
	defer t.end(ctx)
	if err := t.readTreasury(ctx); err != nil {
		return err
	}

	rows, err := repository.New(t.db).ListReturnsInFlight(ctx, repository.ListReturnsInFlightParams{ChainID: t.chainID(), OperatorAddress: t.operator()})
	if err != nil {
		return fmt.Errorf("list the returns in flight: %s", decision.ErrorDetail(err))
	}
	var errs []error
	for _, r := range rows {
		if t.limited() {
			errs = append(errs, errRateLimited)
			break
		}
		if err := t.restart(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("return %s: %w", r.ID, err))
		}
	}
	// Row 9 for the debits (FR-16, EC-18): every non-final authorization by its on-chain state and receipts, before the
	// first cycle sends anything.
	t.debitPass(ctx)
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
		receipt, err := t.receipt(ctx, common.BytesToHash(r.TxHash))
		if err != nil {
			return err
		}
		if receipt != nil {
			// The refund landed and reverted: the cycle retries it.
			return t.follow(ctx, r)
		}
		count, err := t.nonceCount(ctx)
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
// are logged once per kind at the end of the cycle, except at shutdown (failed); a rate limit ends the cycle at once
// (budget.go). The next cycle tries again.
func (t *Tracker) Cycle(ctx context.Context) {
	t.begin()
	defer t.end(ctx)
	t.debitPass(ctx)
	t.stuckPass(ctx)

	q := repository.New(t.db)
	due, err := q.ListReturnsToSend(ctx, repository.ListReturnsToSendParams{
		DueBefore: t.now().Add(-t.cfg.RetryInterval), PageLimit: pageSize, ChainID: t.chainID(), OperatorAddress: t.operator(),
	})
	if err != nil {
		t.failed(ctx, slog.LevelError, "tracker: list the returns to send failed", "error", decision.ErrorDetail(err))
	}
	for _, r := range due {
		if t.limited() {
			break
		}
		if !t.due(r.ID) {
			continue
		}
		if err := t.attempt(ctx, r.ID, r.ChainAuthID, r.ChainRefundID, r.TokenAmount); err != nil {
			t.rowFailed(slog.LevelError, "tracker: sending a refund failed", err, "return_row_id", r.ID.String())
		}
	}

	inFlight, err := q.ListReturnsInFlight(ctx, repository.ListReturnsInFlightParams{ChainID: t.chainID(), OperatorAddress: t.operator()})
	if err != nil {
		t.failed(ctx, slog.LevelError, "tracker: list the returns in flight failed", "error", decision.ErrorDetail(err))
	}
	// INCLUDED refunds by descending block: the reorg check of the highest block covers the lower ones (budget.go).
	sort.SliceStable(inFlight, func(i, j int) bool { return blockOf(inFlight[i].BlockNumber) > blockOf(inFlight[j].BlockNumber) })
	for _, r := range inFlight {
		if t.limited() {
			break
		}
		if err := t.follow(ctx, r); err != nil {
			t.rowFailed(slog.LevelWarn, "tracker: following a refund failed", err, "return_row_id", r.ID.String())
		}
	}
	t.updateMetrics(ctx)
}

// attempt sends one refund (UC-2 step 7): the slot is reserved through the operator queue — or the PLANNED slot of an
// earlier attempt that was never sent is used again, so no nonce is left unused — the return SUBMITTED with
// attempts + 1, then the hash stored and the transaction sent. A failure before the send makes it RETRYING. A rate
// limit before the send is a rate limit of the cycle (rules of S2 st9b): the return goes back to its status with
// its attempts, the slot stays PLANNED, the next cycle tries again, and the one WARN line of the cycle is the log.
func (t *Tracker) attempt(ctx context.Context, id uuid.UUID, chainAuthID, chainRefundID []byte, tokenAmount string) error {
	if t.limited() {
		return errRateLimited
	}
	t.mu.Lock()
	t.lastAttempt[id] = t.now()
	t.mu.Unlock()
	var slot operator.Slot
	var skip bool
	var from string
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
		from = r.Status
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
	_, err = t.queue.Send(ctx, slot, t.refundCall(chainAuthID, chainRefundID, tokenAmount), nil)
	t.sent(slot.ID)
	if err = t.check(err); errors.Is(err, errRateLimited) {
		// Nothing was sent and the row does not move: the status and the attempts of before, no delay of the retry.
		t.mu.Lock()
		delete(t.lastAttempt, id)
		t.mu.Unlock()
		if err := t.setStatus(ctx, id, from, -1, returns.StatusSubmitted); err != nil {
			return err
		}
		return errRateLimited
	}
	if err != nil {
		// Nothing was sent: the slot stays PLANNED and is used by the next attempt (step 8, EC-12).
		t.failed(ctx, slog.LevelWarn, "refund not sent; retrying", "return_row_id", id.String(), "nonce", slot.Nonce, "error", err.Error())
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
		receipt, err := t.receipt(ctx, common.BytesToHash(r.TxHash))
		if err != nil {
			return err
		}
		if receipt == nil {
			used, err := t.refundUsed(ctx, r.ChainRefundID, nil)
			if err != nil {
				return err
			}
			if used {
				// Another transaction of this return landed, a replaced hash of the slot among them.
				return t.included(ctx, r, nil)
			}
			return nil // not mined yet: stuck.go replaces it after tracker_interval × 3
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
		if r.BlockNumber == nil {
			// Preconfirmed, found by refundUsed, or the block was cleared by a reorg: only the receipt is read; a sealed
			// one gives the block, from which finality counts (UC-3 row 10, S2 st9b b).
			receipt, err := t.receipt(ctx, common.BytesToHash(r.TxHash))
			if err != nil || receipt == nil || receipt.Status != types.ReceiptStatusSuccessful || receipt.BlockHash == (common.Hash{}) {
				return err
			}
			if err := t.setTx(ctx, r.TxID, txIncluded, receipt); err != nil {
				return err
			}
			n := receipt.BlockNumber.Int64()
			r.BlockNumber, r.BlockHash, r.TxStatus = &n, receipt.BlockHash.Bytes(), txIncluded
		}
		block := uint64(*r.BlockNumber)
		final, ok, err := t.finalNumber(ctx)
		if err != nil {
			return err
		}
		if !ok || block > final {
			// Not final: no chain call for finality; only the shared reorg check (S2 st9b b, c).
			same, err := t.unchanged(ctx, block, r.BlockHash)
			if err != nil || same {
				return err
			}
			return t.refundReorg(ctx, r)
		}
		// The check right before the return is set final reads its block.
		onChain, err := t.blockHash(ctx, block)
		if err != nil {
			return err
		}
		if onChain != common.BytesToHash(r.BlockHash) {
			return t.refundReorg(ctx, r)
		}
		// At the final block, which is at or above the block of the refund: the controller has code there.
		used, err := t.refundUsed(ctx, r.ChainRefundID, new(big.Int).SetUint64(final))
		if err != nil || !used {
			// A refund that disappeared from the chain is the reorg handling above.
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

// blockOf orders rows by their stored block; a row without one goes last.
func blockOf(n *int64) int64 {
	if n == nil {
		return -1
	}
	return *n
}

// due reports whether a return listed as due was last attempted by this process at least return_retry_interval ago:
// an attempt that failed before its send reuses an old PLANNED slot, whose created_at would make it due at once.
func (t *Tracker) due(id uuid.UUID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.lastAttempt[id]
	return !ok || !t.now().Before(last.Add(t.cfg.RetryInterval))
}

// refundReorg handles an INCLUDED refund whose stored block is no longer on the chain (ADR-10): the receipt is read
// again; without one, refundUsed decides: still used → INCLUDED without a block; not used → sent again.
func (t *Tracker) refundReorg(ctx context.Context, r repository.ListReturnsInFlightRow) error {
	t.logger.WarnContext(ctx, "reorg: the block of an included refund changed", "return_row_id", r.ID.String(),
		"block_number", *r.BlockNumber)
	receipt, err := t.receipt(ctx, common.BytesToHash(r.TxHash))
	if err != nil {
		return err
	}
	if receipt != nil && receipt.Status == types.ReceiptStatusSuccessful {
		return t.setBlock(ctx, r.TxID, receipt)
	}
	used, err := t.refundUsed(ctx, r.ChainRefundID, nil)
	if err != nil {
		return err
	}
	if used {
		return dbErr(repository.New(t.db).SetOperatorTxBlock(ctx, repository.SetOperatorTxBlockParams{ID: r.TxID}))
	}
	if err := t.setStatus(ctx, r.ID, returns.StatusRetrying, 0, returns.StatusIncluded); err != nil {
		return err
	}
	return t.attempt(ctx, r.ID, r.ChainAuthID, r.ChainRefundID, r.TokenAmount)
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

// refundUsed reads refundUsed(refundId) of the controller at block; nil is the latest block.
func (t *Tracker) refundUsed(ctx context.Context, refundID []byte, block *big.Int) (bool, error) {
	if t.limited() {
		return false, errRateLimited
	}
	opts, cancel := t.opts(ctx, block)
	used, err := t.controller().RefundUsed(opts, [32]byte(refundID))
	cancel()
	if err != nil {
		return false, t.rpcErr("refundUsed", err)
	}
	return used, nil
}

// readTreasury reads treasury() of the controller and keeps the address. Start reads it; while it is unset — the read
// of the start failed, by a rate limit among others — each metric update reads it again. Refunds do not need it.
func (t *Tracker) readTreasury(ctx context.Context) error {
	if t.limited() {
		return errRateLimited
	}
	opts, cancel := t.opts(ctx, nil)
	treasury, err := t.controller().Treasury(opts)
	cancel()
	if err != nil {
		return t.rpcErr("treasury()", err)
	}
	t.treasury = treasury
	return nil
}

func (t *Tracker) controller() *bindings.CardSpendControllerCaller {
	c, _ := bindings.NewCardSpendControllerCaller(t.cfg.Controller, t.queue.Reader().Endpoint().Client)
	return c
}

// opts are the options of one contract call at block: the call is bounded by rpc_read_timeout, as every other read
// (SRS — Card Spend §3.1). The caller calls cancel when the call has returned.
func (t *Tracker) opts(ctx context.Context, block *big.Int) (*bind.CallOpts, context.CancelFunc) {
	cctx, cancel := context.WithTimeout(ctx, t.queue.CallTimeout())
	return &bind.CallOpts{Context: cctx, BlockNumber: block}, cancel
}

// chainID and operator scope every query of the tracker: rows of another chain or another operator key are never
// touched (the database may hold them after a key rotation, or from another deployment).
func (t *Tracker) chainID() *int64 {
	id := int64(t.queue.ChainID())
	return &id
}

func (t *Tracker) operator() []byte { return t.queue.Address().Bytes() }

// CloseOpenReturns is the exception of FR-14 for a DEBIT_LOST authorization (UC-3 row 3): its open returns close as
// NOTHING_TO_RETURN — no tokens were debited — and returned_amount drops by their amounts. Called by the debit side
// of the tracker (st6b) in the transaction that sets DEBIT_LOST.
func CloseOpenReturns(ctx context.Context, q *repository.Queries, authorizationID uuid.UUID) (int64, error) {
	return q.CloseOpenReturns(ctx, authorizationID)
}
