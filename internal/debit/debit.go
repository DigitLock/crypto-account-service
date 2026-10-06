// Package debit is the Debit step of card-auth: UC-1 steps 10 to 13 of SRS — Card Spend. It reserves the
// operator nonce and stores the intent before anything is sent (FR-5), signs and sends debit through the operator
// queue, waits for the inclusion signal until the deadline, and stores APPROVED, DECLINED / DEBIT_REVERTED or
// TIMED_OUT.
//
// It runs only in card-auth; packages of server must not import it (ADR-3). Amounts are *big.Int, never floats.
package debit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/operator"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Statuses of operator_txs (SRS — Card Spend §2.4).
const (
	txIncluded = "INCLUDED"
	txReverted = "REVERTED"
)

// DB is what the step needs from the database, role cas_card_auth. *pgxpool.Pool satisfies it.
type DB interface {
	repository.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Config holds the values of SRS — Card Spend §3.1 the step uses.
type Config struct {
	Controller       common.Address
	DecisionDeadline time.Duration // decision_deadline: the wait ends FinishReserve before it
	DebitValidity    time.Duration // debit_validity: validUntil = received_at + it, floored to a second
	GasLimit         uint64        // debit_gas_limit
	PollInterval     time.Duration // receipt_poll_interval
}

// Metrics are the signal metrics of SRS — Card Spend §2.5.1.
type Metrics struct {
	signals *prometheus.CounterVec
}

// NewMetrics registers inclusion_signals_total in reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{signals: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "inclusion_signals_total",
		Help: "Inclusion signals that decided a debit, by source: subscription or polling.",
	}, []string{"source"})}
	m.signals.WithLabelValues(SourcePolling)
	reg.MustRegister(m.signals)
	return m
}

// Step is the Debit step of the decision engine.
type Step struct {
	db      DB
	queue   *operator.Queue
	signals *Signals
	metrics *Metrics
	cfg     Config
	logger  *slog.Logger
	now     func() time.Time
	abi     *abi.ABI
}

var _ decision.Debit = (*Step)(nil)

// New returns the step. now is the clock of the stored times, as the engine's.
func New(db DB, queue *operator.Queue, signals *Signals, metrics *Metrics, cfg Config, logger *slog.Logger, now func() time.Time) (*Step, error) {
	a, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("debit: controller ABI: %w", err)
	}
	return &Step{db: db, queue: queue, signals: signals, metrics: metrics, cfg: cfg, logger: logger, now: now, abi: a}, nil
}

// ValidUntil is the on-chain expiry of the debit of an authorization received at receivedAt: received_at +
// debit_validity, rounded down to a whole second (FR-6, ADR-12).
func ValidUntil(receivedAt time.Time, validity time.Duration) time.Time {
	return time.Unix(receivedAt.Add(validity).Unix(), 0).UTC()
}

// Debit runs steps 10 to 13. An error is returned only while the authorization is still RECEIVED: the engine
// declines it as INTERNAL_ERROR.
func (s *Step) Debit(ctx context.Context, a decision.Checked) (decision.Decision, error) {
	log := s.logger.With("tenant_id", a.TenantID.String(), "auth_id", a.AuthID)
	validUntil := ValidUntil(a.ReceivedAt, s.cfg.DebitValidity)

	// Step 10.
	slot, err := s.reserve(ctx, a, validUntil)
	txID, nonce := slot.ID, slot.Nonce
	if err != nil {
		return decision.Decision{}, fmt.Errorf("step 10: %s", decision.ErrorDetail(err))
	}

	deadline, _ := ctx.Deadline()
	waitCtx, cancel := context.WithDeadline(ctx, deadline.Add(-decision.FinishReserve(s.cfg.DecisionDeadline)))
	defer cancel()
	// Registered before the send, so no signal can come before the wait.
	signals, stop := s.signals.wait(a.ChainAuthID)
	defer stop()

	// Step 11.
	hash, err := s.send(waitCtx, a, slot, validUntil)
	if err != nil {
		// Nothing was sent: the slot stays PLANNED for the tracker (UC-3 row 9). The outcome is not decided yet,
		// so the answer is the deadline's.
		log.ErrorContext(ctx, "debit not sent", "nonce", nonce, "error", err.Error())
		return s.finish(ctx, log, a, txID, timedOut)
	}

	// Step 12.
	go s.poll(waitCtx, a.ChainAuthID, hash)
	for {
		select {
		case <-waitCtx.Done():
			// Step 13.
			log.InfoContext(ctx, "no inclusion signal by the deadline", "tx_hash", hash.Hex())
			return s.finish(ctx, log, a, txID, timedOut)
		case sig := <-signals:
			receipt := sig.Receipt
			if receipt == nil {
				if receipt = s.receipt(waitCtx, hash); receipt == nil {
					// A signal without a readable receipt decides nothing; polling goes on.
					continue
				}
			}
			s.metrics.signals.WithLabelValues(sig.Source).Inc()
			o := outcome{status: decision.StatusApproved, txStatus: txIncluded, receipt: receipt, hash: hash}
			if receipt.Status != types.ReceiptStatusSuccessful {
				o = outcome{status: decision.StatusDeclined, reason: decision.ReasonDebitReverted, txStatus: txReverted, receipt: receipt}
			}
			return s.finish(ctx, log, a, txID, o)
		}
	}
}

// reserve is step 10 in one transaction: the next nonce of the operator queue, the slot PLANNED, the authorization
// DEBIT_SUBMITTED with its valid_until, the event. No chain call inside.
func (s *Step) reserve(ctx context.Context, a decision.Checked, validUntil time.Time) (operator.Slot, error) {
	var slot operator.Slot
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		id := a.ID
		var err error
		if slot, err = s.queue.Reserve(ctx, q, operator.PurposeDebit, &id, nil); err != nil {
			return err
		}
		n, err := q.SubmitAuthorization(ctx, repository.SubmitAuthorizationParams{ID: a.ID, ValidUntil: &validUntil})
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("the authorization is no longer RECEIVED")
		}
		from := decision.StatusReceived
		return q.InsertAuthorizationEvent(ctx, repository.InsertAuthorizationEventParams{
			AuthorizationID: a.ID, FromStatus: &from, ToStatus: decision.StatusDebitSubmitted, CreatedAt: s.now(),
		})
	})
	return slot, err
}

// send is step 11 through the operator queue: an error only before the hash is stored, nothing sent.
func (s *Step) send(ctx context.Context, a decision.Checked, slot operator.Slot, validUntil time.Time) (common.Hash, error) {
	data, err := s.abi.Pack("debit", a.Wallet, a.TokenAmount, [32]byte(a.ChainAuthID), uint64(validUntil.Unix()))
	if err != nil {
		return common.Hash{}, fmt.Errorf("calldata: %w", err)
	}
	return s.queue.Send(ctx, slot, operator.Call{Purpose: operator.PurposeDebit, To: s.cfg.Controller, Data: data, Gas: s.cfg.GasLimit}, nil)
}

// poll asks for the receipt now and every PollInterval until it has one or ctx ends, and delivers it as a
// polling signal. A preconfirmed receipt (zero block hash) counts (UC-3 row 10).
func (s *Step) poll(ctx context.Context, authID, hash common.Hash) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if r := s.receipt(ctx, hash); r != nil {
			s.signals.Deliver(authID, Signal{Source: SourcePolling, Receipt: r})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// receipt reads the receipt of hash; nil when there is none yet or the read fails.
func (s *Step) receipt(ctx context.Context, hash common.Hash) *types.Receipt {
	return s.queue.Receipt(ctx, hash)
}

// outcome is the end of steps 12 and 13.
type outcome struct {
	status, reason string
	txStatus       string         // empty: operator_txs unchanged (TIMED_OUT keeps SENT or PLANNED)
	receipt        *types.Receipt // of INCLUDED and REVERTED
	hash           common.Hash
}

// timedOut is the outcome of the deadline (step 13).
var timedOut = outcome{status: decision.StatusTimedOut, reason: decision.ReasonTimeout}

// gate orders the write of an outcome against the answer at the deadline (D-21). Once the answer has gone out as
// TIMED_OUT, only TIMED_OUT may be committed: a debit that is then found included becomes a late debit for the
// tracker, never an approval the processor did not see.
type gate struct {
	mu        sync.Mutex
	late      bool     // the deadline's answer has gone out
	committed *outcome // the outcome stored
}

// finish stores the outcome and answers it. The write runs in its own context, detached from the request deadline
// with its own timeout (D-21): the answer keeps its deadline, the outcome is stored even after the answer. When
// the deadline comes first, the answer is TIMED_OUT and so is the stored outcome. When the write fails, the row
// stays DEBIT_SUBMITTED for the tracker (UC-3) and the answer is TIMED_OUT.
func (s *Step) finish(ctx context.Context, log *slog.Logger, a decision.Checked, txID uuid.UUID, o outcome) (decision.Decision, error) {
	g := &gate{}
	type result struct {
		o   outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), decision.OutcomeWriteTimeout)
		defer cancel()
		stored, err := s.store(wctx, a, txID, o, g)
		if err != nil {
			log.ErrorContext(wctx, "storing the outcome failed; the row stays DEBIT_SUBMITTED for the tracker",
				"status", stored.status, "error", err.Error())
		}
		done <- result{stored, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return s.answer(ctx, log, a, timedOut), nil
		}
		return s.answer(ctx, log, a, r.o), nil
	case <-ctx.Done():
	}
	g.mu.Lock()
	g.late = true
	committed := g.committed
	g.mu.Unlock()
	if committed != nil {
		return s.answer(ctx, log, a, *committed), nil
	}
	log.WarnContext(ctx, "outcome answered at the deadline before it was stored", "status", o.status)
	return s.answer(ctx, log, a, timedOut), nil
}

// store writes the outcome in one transaction and returns the outcome it committed: o, or TIMED_OUT when the
// answer of the deadline went out first.
func (s *Step) store(ctx context.Context, a decision.Checked, txID uuid.UUID, o outcome, g *gate) (outcome, error) {
	for {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return o, errors.New(decision.ErrorDetail(err))
		}
		if err := s.write(ctx, tx, a, txID, o); err != nil {
			_ = tx.Rollback(ctx)
			return o, errors.New(decision.ErrorDetail(err))
		}
		g.mu.Lock()
		if g.late && o.status != decision.StatusTimedOut {
			g.mu.Unlock()
			_ = tx.Rollback(ctx)
			o = timedOut
			continue
		}
		err = tx.Commit(ctx)
		if err == nil {
			g.committed = &o
		}
		g.mu.Unlock()
		if err != nil {
			return o, errors.New(decision.ErrorDetail(err))
		}
		return o, nil
	}
}

// write is the statements of an outcome.
func (s *Step) write(ctx context.Context, tx pgx.Tx, a decision.Checked, txID uuid.UUID, o outcome) error {
	q := repository.New(tx)
	decidedAt := s.now()
	var reason *string
	if o.reason != "" {
		reason = &o.reason
	}
	n, err := q.FinishSubmittedAuthorization(ctx, repository.FinishSubmittedAuthorizationParams{
		ID: a.ID, Status: o.status, DeclineReason: reason, Debited: o.status == decision.StatusApproved, DecidedAt: &decidedAt,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("the authorization is no longer DEBIT_SUBMITTED")
	}
	if o.txStatus != "" {
		p := repository.SetOperatorTxOutcomeParams{ID: txID, Status: o.txStatus}
		// Block number and hash only from a sealed receipt: a preconfirmed one carries a zero hash.
		if o.receipt.BlockHash != (common.Hash{}) && o.receipt.BlockNumber != nil && o.receipt.BlockNumber.IsInt64() {
			number := o.receipt.BlockNumber.Int64()
			p.BlockNumber, p.BlockHash = &number, o.receipt.BlockHash.Bytes()
		}
		if err := q.SetOperatorTxOutcome(ctx, p); err != nil {
			return err
		}
	}
	from := decision.StatusDebitSubmitted
	return q.InsertAuthorizationEvent(ctx, repository.InsertAuthorizationEventParams{
		AuthorizationID: a.ID, FromStatus: &from, ToStatus: o.status, Reason: reason, CreatedAt: decidedAt,
	})
}

// answer is the decision of an outcome.
func (s *Step) answer(ctx context.Context, log *slog.Logger, a decision.Checked, o outcome) decision.Decision {
	if o.status == decision.StatusApproved {
		log.InfoContext(ctx, "authorization approved", "tx_hash", o.hash.Hex())
		return decision.Decision{
			Decision: decision.Approved, Status: decision.StatusApproved, Token: a.Token,
			TokenAmount: a.TokenAmount.String(), Rate: a.Rate, BufferBps: a.BufferBps, TxHash: o.hash.Hex(),
		}
	}
	log.InfoContext(ctx, "authorization declined", "status", o.status, "reason", o.reason)
	return decision.Decision{Decision: decision.Declined, Status: o.status, DeclineReason: o.reason}
}
