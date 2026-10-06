// Package debit is the Debit step of card-auth: UC-1 steps 10 to 13 of SRS — Card Spend. It reserves the
// operator nonce and stores the intent before anything is sent (FR-5), signs and sends debit, waits for the
// inclusion signal until the deadline, and stores APPROVED, DECLINED / DEBIT_REVERTED or TIMED_OUT.
//
// It holds the operator signer: only card-auth uses it; packages of server must not import it (ADR-3). Fees and
// amounts are *big.Int, never floats.
package debit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/signer"
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
	ChainID          uint64
	Controller       common.Address
	DecisionDeadline time.Duration // decision_deadline: the wait ends FinishReserve before it
	DebitValidity    time.Duration // debit_validity: validUntil = received_at + it, floored to a second
	GasLimit         uint64        // debit_gas_limit
	PollInterval     time.Duration // receipt_poll_interval
	CallTimeout      time.Duration // one fee read, send or receipt poll: rpc_read_timeout
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
	reader  *chain.Reader
	signer  signer.Signer
	signals *Signals
	metrics *Metrics
	cfg     Config
	logger  *slog.Logger
	now     func() time.Time
	abi     *abi.ABI
}

var _ decision.Debit = (*Step)(nil)

// New returns the step. now is the clock of the stored times, as the engine's.
func New(db DB, reader *chain.Reader, s signer.Signer, signals *Signals, metrics *Metrics, cfg Config, logger *slog.Logger, now func() time.Time) (*Step, error) {
	a, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("debit: controller ABI: %w", err)
	}
	return &Step{db: db, reader: reader, signer: s, signals: signals, metrics: metrics, cfg: cfg, logger: logger, now: now, abi: a}, nil
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
	txID, nonce, err := s.reserve(ctx, a, validUntil)
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
	hash, err := s.send(waitCtx, log, a, txID, nonce, validUntil)
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

// reserve is step 10 in one transaction: the operator's nonce row locked, the slot PLANNED, the authorization
// DEBIT_SUBMITTED with its valid_until, the event. No chain call inside.
func (s *Step) reserve(ctx context.Context, a decision.Checked, validUntil time.Time) (txID uuid.UUID, nonce uint64, err error) {
	chainID := int64(s.cfg.ChainID)
	operator := s.signer.Address().Bytes()
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		next, err := q.LockOperatorAccount(ctx, repository.LockOperatorAccountParams{ChainID: chainID, Address: operator})
		if err != nil {
			return fmt.Errorf("lock the operator account: %w", err)
		}
		if err := q.SetOperatorNextNonce(ctx, repository.SetOperatorNextNonceParams{
			ChainID: chainID, Address: operator, NextNonce: next + 1,
		}); err != nil {
			return err
		}
		id := a.ID
		if txID, err = q.InsertPlannedDebit(ctx, repository.InsertPlannedDebitParams{
			ChainID: chainID, OperatorAddress: operator, Nonce: next, AuthorizationID: &id,
		}); err != nil {
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
		nonce = uint64(next)
		return q.InsertAuthorizationEvent(ctx, repository.InsertAuthorizationEventParams{
			AuthorizationID: a.ID, FromStatus: &from, ToStatus: decision.StatusDebitSubmitted, CreatedAt: s.now(),
		})
	})
	return txID, nonce, err
}

// pendingBlock is the part of the pending block the fee needs.
type pendingBlock struct {
	BaseFee *hexutil.Big `json:"baseFeePerGas"`
}

// send is step 11: fees, calldata, signature; the hash stored as SENT; then eth_sendRawTransaction. An error is
// returned only before the hash is stored. Any error of the send itself counts as sent (EC-15).
func (s *Step) send(ctx context.Context, log *slog.Logger, a decision.Checked, txID uuid.UUID, nonce uint64, validUntil time.Time) (common.Hash, error) {
	ep := s.reader.Endpoint()

	// Fees (§3.2, ADR-10): the tip of eth_maxPriorityFeePerGas; maxFeePerGas = 2 × base fee of the pending
	// block + the tip. One batch, no eth_estimateGas: the gas limit is fixed.
	var tip hexutil.Big
	var block *pendingBlock
	batch := []rpc.BatchElem{
		{Method: "eth_maxPriorityFeePerGas", Result: &tip},
		{Method: "eth_getBlockByNumber", Args: []any{chain.PendingTag, false}, Result: &block},
	}
	fctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	err := ep.Client.Client().BatchCallContext(fctx, batch)
	cancel()
	for _, b := range batch {
		err = errors.Join(err, b.Error)
	}
	if err != nil {
		return common.Hash{}, fmt.Errorf("fees: %s: %s", ep.Name, s.reader.Describe(err, s.cfg.CallTimeout))
	}
	if block == nil || block.BaseFee == nil {
		return common.Hash{}, fmt.Errorf("fees: %s: the pending block has no base fee", ep.Name)
	}
	tipCap := (*big.Int)(&tip)
	feeCap := new(big.Int).Mul((*big.Int)(block.BaseFee), big.NewInt(2))
	feeCap.Add(feeCap, tipCap)

	data, err := s.abi.Pack("debit", a.Wallet, a.TokenAmount, [32]byte(a.ChainAuthID), uint64(validUntil.Unix()))
	if err != nil {
		return common.Hash{}, fmt.Errorf("calldata: %w", err)
	}
	controller := s.cfg.Controller
	chainID := new(big.Int).SetUint64(s.cfg.ChainID)
	signed, err := s.signer.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: tipCap, GasFeeCap: feeCap, Gas: s.cfg.GasLimit,
		To: &controller, Value: new(big.Int), Data: data,
	}), chainID)
	if err != nil {
		return common.Hash{}, err
	}
	hash := signed.Hash()

	// The hash is stored before the send: a restart finds every hash that may be on the network.
	n, err := repository.New(s.db).MarkOperatorTxSent(ctx, repository.MarkOperatorTxSentParams{ID: txID, TxHash: hash.Bytes()})
	if err != nil || n != 1 {
		if err == nil {
			err = errors.New("the slot is no longer PLANNED")
		}
		return common.Hash{}, fmt.Errorf("store the hash: %s", decision.ErrorDetail(err))
	}

	sctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	err = ep.Client.SendTransaction(sctx, signed)
	cancel()
	if err != nil {
		// EC-15: the transaction may be on the network. Step 12 or the tracker resolves it.
		log.WarnContext(ctx, "debit send failed; treated as sent", "endpoint", ep.Name, "nonce", nonce,
			"tx_hash", hash.Hex(), "error", s.reader.Describe(err, s.cfg.CallTimeout))
	} else {
		log.DebugContext(ctx, "debit sent", "endpoint", ep.Name, "nonce", nonce, "tx_hash", hash.Hex())
	}
	return hash, nil
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
	ep := s.reader.Endpoint()
	rctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	r, err := ep.Client.TransactionReceipt(rctx, hash)
	if err != nil {
		if !errors.Is(err, ethereum.NotFound) && ctx.Err() == nil {
			s.logger.DebugContext(ctx, "receipt poll failed", "endpoint", ep.Name, "tx_hash", hash.Hex(),
				"error", s.reader.Describe(err, s.cfg.CallTimeout))
		}
		return nil
	}
	return r
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
