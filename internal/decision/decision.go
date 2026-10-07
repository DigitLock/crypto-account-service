// Package decision is the decision engine of card-auth: UC-1 steps 3 to 9 of SRS — Card Spend and the hand-off
// of an authorization that passed every check to the Debit step (steps 10 to 13). It runs only in card-auth;
// packages of server must not import it (ADR-3).
//
// Every decline is stored before it is answered: status DECLINED, decline_reason, decided_at and one
// authorization_events row per status change. Amounts and rates are exact decimals (quote.go), never floats.
package decision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/crs"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Authorization states (SRS — Card Spend §2.3.1).
const (
	StatusReceived          = "RECEIVED"
	StatusDebitSubmitted    = "DEBIT_SUBMITTED"
	StatusApproved          = "APPROVED"
	StatusDebitConfirmed    = "DEBIT_CONFIRMED"
	StatusDeclined          = "DECLINED"
	StatusTimedOut          = "TIMED_OUT"
	StatusLateDebit         = "LATE_DEBIT"
	StatusLateDebitRefunded = "LATE_DEBIT_REFUNDED"
	StatusDebitLost         = "DEBIT_LOST"
)

// Decisions (SRS — Card Spend §2.1.2).
const (
	Approved = "APPROVED"
	Declined = "DECLINED"
)

// Decline reasons (SRS — Card Spend §2.1.2).
const (
	ReasonCardNotFound          = "CARD_NOT_FOUND"
	ReasonCardFrozen            = "CARD_FROZEN"
	ReasonProgramPaused         = "PROGRAM_PAUSED"
	ReasonCurrencyNotSupported  = "CURRENCY_NOT_SUPPORTED"
	ReasonRateUnavailable       = "RATE_UNAVAILABLE"
	ReasonLimitExceeded         = "LIMIT_EXCEEDED"
	ReasonInsufficientFunds     = "INSUFFICIENT_FUNDS"
	ReasonInsufficientAllowance = "INSUFFICIENT_ALLOWANCE"
	ReasonChainUnavailable      = "CHAIN_UNAVAILABLE"
	ReasonDebitReverted         = "DEBIT_REVERTED"
	ReasonTimeout               = "TIMEOUT"
	ReasonReversedBeforeAuth    = "REVERSED_BEFORE_AUTH"
	ReasonInternalError         = "INTERNAL_ERROR"
)

// cardActive is the status of a card that may spend (SRS — Core §2.4 cards).
const cardActive = "ACTIVE"

// ErrAuthIDConflict: the auth_id is known with a different normalized body (EC-2, 409 AUTH_ID_CONFLICT).
var ErrAuthIDConflict = errors.New("auth_id is known with a different request")

// Request is a validated authorization request of a tenant (SRS — Card Spend §2.1.1, §2.1.2).
type Request struct {
	TenantID uuid.UUID
	AuthID   string
	CardRef  string
	Amount   string // a plain decimal without trailing zeros, greater than 0
	Currency string
	Merchant []byte // as received; nil when absent
	Hash     []byte // request_hash: SHA-256 of the normalized request
}

// Decision is the answer of an authorization (SRS — Card Spend §2.1.2). Token, TokenAmount, the quote and
// TxHash are set only for an approval; Rate is empty for USD.
type Decision struct {
	Decision      string
	Status        string
	DeclineReason string
	Token         string
	TokenAmount   string
	Rate          string
	BufferBps     *int32
	TxHash        string
}

// Result is a Decision and whether this request made it. A repeated request answered from the stored decision
// or by waiting for another request is not Fresh: it is not counted again in the metrics.
type Result struct {
	Decision
	Fresh bool
}

// Checked is an authorization that passed steps 5 to 9, handed to the Debit step.
type Checked struct {
	ID          uuid.UUID // authorizations.id
	TenantID    uuid.UUID
	AuthID      string
	ChainAuthID common.Hash
	CardID      uuid.UUID
	Wallet      common.Address
	ChainID     uint64
	Token       string
	TokenAmount *big.Int
	Rate        string // empty for USD
	BufferBps   *int32 // nil for USD
	ReceivedAt  time.Time
	DeadlineAt  time.Time
}

// Debit runs steps 10 to 13 for a checked authorization, while the engine holds the card lock. It owns every
// status change of the authorization from RECEIVED on and returns the decision it stored. ctx ends at the
// decision deadline. An error before the authorization left RECEIVED makes the engine decline it as
// INTERNAL_ERROR.
type Debit interface {
	Debit(ctx context.Context, a Checked) (Decision, error)
}

// Rates returns rate_decimal of a currency to USD (package crs).
type Rates interface {
	Rate(ctx context.Context, currency string) (string, error)
}

// ChainReader reads the state of step 9 (package chain).
type ChainReader interface {
	Read(ctx context.Context, wallet common.Address) (chain.State, error)
}

// DB is what the engine needs from the database, role cas_card_auth. *pgxpool.Pool satisfies it.
type DB interface {
	repository.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Config holds the values of SRS — Card Spend §3.1 the engine uses.
type Config struct {
	DecisionDeadline time.Duration
	QuoteBufferBPS   int
	TokenDecimals    uint8
	Token            string // symbol of the funding token, read at start (D-10)
	ChainID          uint64
	MinSendWindow    time.Duration // min_send_window: step 9a (D-20)
}

// Engine decides authorizations.
type Engine struct {
	db     DB
	rates  Rates // nil: CRS_ADDRESS unset, every non-USD authorization is RATE_UNAVAILABLE
	chain  ChainReader
	debit  Debit
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
	locks  *cardLocks
	calls  *calls
}

// New returns an engine. now is the clock of received_at, decided_at and the UTC day of step 8; the deadlines
// of the request run on real time.
func New(db DB, rates Rates, reader ChainReader, debit Debit, cfg Config, logger *slog.Logger, now func() time.Time) *Engine {
	return &Engine{
		db: db, rates: rates, chain: reader, debit: debit, cfg: cfg, logger: logger, now: now,
		locks: newCardLocks(), calls: newCalls(),
	}
}

// ChainAuthID is the authId of the contract (D-16): keccak256 of the 16 bytes of the tenant UUID followed by the
// UTF-8 bytes of auth_id. The prefix has a fixed length, so the same auth_id of two tenants never gives the same
// input.
func ChainAuthID(tenantID uuid.UUID, authID string) common.Hash {
	return crypto.Keccak256Hash(tenantID[:], []byte(authID))
}

// ChainRefundID is the refundId of the contract (D-16): keccak256 of the 16 bytes of the tenant UUID followed by
// the UTF-8 bytes of return_id.
func ChainRefundID(tenantID uuid.UUID, returnID string) common.Hash {
	return crypto.Keccak256Hash(tenantID[:], []byte(returnID))
}

// FinishReserve is the part of a decision deadline kept for storing the decision: the checks, and the wait for
// the inclusion signal, end this much earlier, so the answer is never later than deadline_at.
func FinishReserve(decisionDeadline time.Duration) time.Duration {
	return min(50*time.Millisecond, decisionDeadline/10)
}

func (e *Engine) finishReserve() time.Duration { return FinishReserve(e.cfg.DecisionDeadline) }

// Authorize runs steps 3 to 9 for a validated request received at start (real time) and returns the decision.
// The only error is ErrAuthIDConflict. Any other failure is a decline, INTERNAL_ERROR or TIMEOUT. The result is
// never later than start + decision_deadline, up to the time a cancelled call takes to return.
func (e *Engine) Authorize(ctx context.Context, req Request, start time.Time) (Result, error) {
	// The decision does not depend on the connection of the processor: it is stored either way.
	ctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), start.Add(e.cfg.DecisionDeadline))
	defer cancel()

	k := key{req.TenantID, req.AuthID}
	cl := e.calls.join(k)
	defer e.calls.leave(k, cl)

	// Step 3.
	q := repository.New(e.db)
	row, err := q.GetDecisionState(ctx, repository.GetDecisionStateParams{TenantID: req.TenantID, AuthID: req.AuthID})
	if err == nil {
		return e.repeated(ctx, req, row, cl)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return e.unstored(ctx, req, "step 3: read the authorization", err), nil
	}

	// Step 4.
	receivedAt := e.now()
	id, inserted, err := e.insert(ctx, req, receivedAt)
	if err != nil {
		return e.unstored(ctx, req, "step 4: insert the authorization", err), nil
	}
	if !inserted {
		// A concurrent request with the same auth_id won the unique key: this one is a repeated request.
		row, err := q.GetDecisionState(ctx, repository.GetDecisionStateParams{TenantID: req.TenantID, AuthID: req.AuthID})
		if err != nil {
			return e.unstored(ctx, req, "step 3: read the authorization after a concurrent insert", err), nil
		}
		return e.repeated(ctx, req, row, cl)
	}

	d := e.decide(ctx, req, id, receivedAt)
	cl.publish(d)
	return Result{Decision: d, Fresh: true}, nil
}

// repeated answers a request whose auth_id is stored: tombstone, conflict, stored decision, or the decision of
// the request deciding it now.
func (e *Engine) repeated(ctx context.Context, req Request, row repository.GetDecisionStateRow, cl *call) (Result, error) {
	if row.RequestHash == nil {
		// A tombstone of a reversal that came first (UC-2 step 3, EC-14): no lock, no read.
		return Result{Decision: declined(row.Status, ReasonReversedBeforeAuth), Fresh: true}, nil
	}
	if !bytes.Equal(row.RequestHash, req.Hash) {
		return Result{}, ErrAuthIDConflict
	}
	if row.Status != StatusReceived && row.Status != StatusDebitSubmitted {
		return Result{Decision: stored(row)}, nil
	}
	if d, ok := cl.wait(ctx); ok {
		return Result{Decision: d}, nil
	}
	// This request's own deadline: the authorization is still being decided, or was left by a stopped process.
	return Result{Decision: inProgressTimeout(row.Status)}, nil
}

// insert stores the request as RECEIVED with its first event. inserted is false when the auth_id exists.
func (e *Engine) insert(ctx context.Context, req Request, receivedAt time.Time) (id uuid.UUID, inserted bool, err error) {
	deadlineAt := receivedAt.Add(e.cfg.DecisionDeadline)
	chainAuthID := ChainAuthID(req.TenantID, req.AuthID)
	currency := req.Currency
	err = pgx.BeginFunc(ctx, e.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		var err error
		id, err = q.InsertReceivedAuthorization(ctx, repository.InsertReceivedAuthorizationParams{
			TenantID: req.TenantID, AuthID: req.AuthID, ChainAuthID: chainAuthID.Bytes(), RequestHash: req.Hash,
			FiatAmount: req.Amount, FiatCurrency: &currency, Merchant: req.Merchant,
			ReceivedAt: receivedAt, DeadlineAt: &deadlineAt,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		inserted = true
		return q.InsertAuthorizationEvent(ctx, repository.InsertAuthorizationEventParams{
			AuthorizationID: id, ToStatus: StatusReceived, CreatedAt: receivedAt,
		})
	})
	return id, inserted, err
}

// facts are the values of steps 6 and 7 stored with the authorization; nil is not known yet.
type facts struct {
	cardID      *uuid.UUID
	wallet      []byte
	chainID     *int64
	rate        *string
	bufferBps   *int32
	token       *string
	tokenAmount *string
}

func (f facts) params(id uuid.UUID) repository.SetAuthorizationFactsParams {
	return repository.SetAuthorizationFactsParams{
		ID: id, CardID: f.cardID, WalletAddress: f.wallet, ChainID: f.chainID, Rate: f.rate,
		BufferBps: f.bufferBps, Token: f.token, TokenAmount: f.tokenAmount,
	}
}

// decide runs steps 5 to 9 for the authorization id and hands it to the Debit step when every check passes.
func (e *Engine) decide(ctx context.Context, req Request, id uuid.UUID, receivedAt time.Time) Decision {
	deadline, _ := ctx.Deadline()
	stepCtx, cancel := context.WithDeadline(ctx, deadline.Add(-e.finishReserve()))
	defer cancel()

	var f facts
	log := e.logger.With("tenant_id", req.TenantID.String(), "auth_id", req.AuthID)
	decline := func(reason string) Decision { return e.decline(ctx, log, id, f, reason) }
	// failed is the decline of an unexpected failure of a step: TIMEOUT when the deadline ended it.
	failed := func(step string, err error) Decision {
		if stepCtx.Err() != nil {
			return decline(ReasonTimeout)
		}
		log.ErrorContext(ctx, "authorization failed", "step", step, "error", ErrorDetail(err))
		return decline(ReasonInternalError)
	}

	// Step 5: one in-flight authorization per card, until the decision.
	release, ok := e.locks.acquire(stepCtx, key{req.TenantID, req.CardRef})
	if !ok {
		return decline(ReasonTimeout)
	}
	defer release()

	// Step 6.
	q := repository.New(e.db)
	card, err := q.GetCardForDecision(stepCtx, repository.GetCardForDecisionParams{TenantID: req.TenantID, CardRef: req.CardRef})
	if errors.Is(err, pgx.ErrNoRows) {
		return decline(ReasonCardNotFound)
	}
	if err != nil {
		return failed("step 6: read the card", err)
	}
	f.cardID = &card.ID
	if !common.IsHexAddress(card.WalletAddress) {
		return failed("step 6: read the card", errors.New("the wallet address of the card's connection is malformed"))
	}
	wallet := common.HexToAddress(card.WalletAddress)
	f.wallet, f.chainID = wallet.Bytes(), &card.ChainID
	if card.ChainID < 0 || uint64(card.ChainID) != e.cfg.ChainID {
		return failed("step 6: read the card", fmt.Errorf("the wallet of the card is on chain %d, card-auth serves chain %d", card.ChainID, e.cfg.ChainID))
	}
	if card.Status != cardActive {
		return decline(ReasonCardFrozen)
	}

	// Step 7.
	var rate string
	var bufferBps *int32
	if req.Currency != crs.USD {
		if e.rates == nil {
			log.WarnContext(ctx, "no rate source: CRS_ADDRESS is unset")
			return decline(ReasonRateUnavailable)
		}
		raw, err := e.rates.Rate(stepCtx, req.Currency)
		switch {
		case errors.Is(err, crs.ErrNotFound):
			return decline(ReasonCurrencyNotSupported)
		case err != nil && stepCtx.Err() != nil:
			return decline(ReasonTimeout)
		case err != nil:
			log.WarnContext(ctx, "rate unavailable", "currency", req.Currency, "error", err.Error())
			return decline(ReasonRateUnavailable)
		}
		if rate, err = ParseRate(raw); err != nil {
			log.WarnContext(ctx, "rate unavailable", "currency", req.Currency, "error", err.Error())
			return decline(ReasonRateUnavailable)
		}
		bps := int32(e.cfg.QuoteBufferBPS)
		bufferBps = &bps
	}
	amount, err := TokenAmount(req.Amount, rate, e.cfg.QuoteBufferBPS, e.cfg.TokenDecimals)
	if err != nil {
		return failed("step 7: quote", err)
	}
	amountText, token := amount.String(), e.cfg.Token
	f.tokenAmount, f.token, f.bufferBps = &amountText, &token, bufferBps
	if rate != "" {
		f.rate = &rate
	}
	if n, err := q.SetAuthorizationFacts(stepCtx, f.params(id)); err != nil || n != 1 {
		if err == nil {
			err = errors.New("the authorization is no longer RECEIVED")
		}
		return failed("step 7: store the quote", err)
	}

	// Step 8: the card daily limit over the UTC day of received_at.
	day := receivedAt.UTC()
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	spentText, err := q.GetCardDaySpend(stepCtx, repository.GetCardDaySpendParams{
		CardID: &card.ID, DayStart: dayStart, DayEnd: dayStart.AddDate(0, 0, 1),
	})
	if err != nil {
		return failed("step 8: read the day's spend", err)
	}
	spent, ok1 := new(big.Int).SetString(spentText, 10)
	limit, ok2 := new(big.Int).SetString(card.DailyLimit, 10)
	if !ok1 || !ok2 {
		return failed("step 8: read the day's spend", errors.New("a stored amount is not an integer"))
	}
	if spent.Add(spent, amount).Cmp(limit) > 0 {
		return decline(ReasonLimitExceeded)
	}

	// Step 9.
	st, err := e.chain.Read(stepCtx, wallet)
	if err != nil {
		if stepCtx.Err() != nil {
			return decline(ReasonTimeout)
		}
		log.WarnContext(ctx, "chain unavailable", "error", err.Error())
		return decline(ReasonChainUnavailable)
	}
	switch {
	case st.Paused:
		return decline(ReasonProgramPaused)
	case st.Balance.Cmp(amount) < 0:
		return decline(ReasonInsufficientFunds)
	case st.Allowance.Cmp(amount) < 0:
		return decline(ReasonInsufficientAllowance)
	case st.RemainingDailyLimit.Cmp(amount) < 0:
		return decline(ReasonLimitExceeded)
	}

	// Step 9a (D-20): no debit is sent without the time to see its signal. Nothing is reserved or sent.
	if deadline, _ := ctx.Deadline(); time.Until(deadline) < e.cfg.MinSendWindow {
		log.InfoContext(ctx, "less than min_send_window left before step 10", "left", time.Until(deadline).String())
		return decline(ReasonTimeout)
	}

	// Steps 10 to 13.
	d, err := e.debit.Debit(ctx, Checked{
		ID: id, TenantID: req.TenantID, AuthID: req.AuthID, ChainAuthID: ChainAuthID(req.TenantID, req.AuthID),
		CardID: card.ID, Wallet: wallet, ChainID: e.cfg.ChainID, Token: token, TokenAmount: new(big.Int).Set(amount),
		Rate: rate, BufferBps: bufferBps, ReceivedAt: receivedAt, DeadlineAt: receivedAt.Add(e.cfg.DecisionDeadline),
	})
	if err != nil {
		log.ErrorContext(ctx, "authorization failed", "step", "debit", "error", err.Error())
		return decline(ReasonInternalError)
	}
	return d
}

// OutcomeWriteTimeout bounds the write of an outcome. The write is detached from the request deadline (D-21): the
// answer keeps its deadline, the outcome is stored even when the answer has already gone out.
const OutcomeWriteTimeout = 2 * time.Second

// decline stores RECEIVED → DECLINED with the reason, the known facts and the event, and returns the answer. The
// write runs in its own context; when the deadline comes first the decline is answered and the write goes on.
// When the authorization is no longer RECEIVED, the stored state is answered; when storing fails before the
// deadline, INTERNAL_ERROR. No tokens moved either way.
func (e *Engine) decline(ctx context.Context, log *slog.Logger, id uuid.UUID, f facts, reason string) Decision {
	done := make(chan Decision, 1)
	go func() {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), OutcomeWriteTimeout)
		defer cancel()
		done <- e.storeDecline(wctx, log, id, f, reason)
	}()
	select {
	case d := <-done:
		return d
	case <-ctx.Done():
		log.WarnContext(ctx, "decline answered at the deadline before it was stored", "reason", reason)
		return declined(StatusDeclined, reason)
	}
}

func (e *Engine) storeDecline(ctx context.Context, log *slog.Logger, id uuid.UUID, f facts, reason string) Decision {
	decidedAt := e.now()
	var changed bool
	err := pgx.BeginFunc(ctx, e.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if _, err := q.SetAuthorizationFacts(ctx, f.params(id)); err != nil {
			return err
		}
		r := reason
		n, err := q.DeclineReceivedAuthorization(ctx, repository.DeclineReceivedAuthorizationParams{
			ID: id, DeclineReason: &r, DecidedAt: &decidedAt,
		})
		if err != nil || n == 0 {
			return err
		}
		changed = true
		from := StatusReceived
		return q.InsertAuthorizationEvent(ctx, repository.InsertAuthorizationEventParams{
			AuthorizationID: id, FromStatus: &from, ToStatus: StatusDeclined, Reason: &r, CreatedAt: decidedAt,
		})
	})
	if err != nil {
		log.ErrorContext(ctx, "storing the decline failed", "reason", reason, "error", ErrorDetail(err))
		return declined(StatusDeclined, ReasonInternalError)
	}
	if !changed {
		return e.current(ctx, log, id)
	}
	log.InfoContext(ctx, "authorization declined", "reason", reason)
	return declined(StatusDeclined, reason)
}

// current answers the stored state of an authorization that left RECEIVED without this engine.
func (e *Engine) current(ctx context.Context, log *slog.Logger, id uuid.UUID) Decision {
	row, err := repository.New(e.db).GetDecisionStateByID(ctx, id)
	if err != nil {
		log.ErrorContext(ctx, "reading the authorization failed", "error", ErrorDetail(err))
		return declined(StatusDeclined, ReasonInternalError)
	}
	if row.Status == StatusReceived || row.Status == StatusDebitSubmitted {
		return inProgressTimeout(row.Status)
	}
	return stored(repository.GetDecisionStateRow(row))
}

// unstored answers a failure of step 3 or 4: nothing is stored.
func (e *Engine) unstored(ctx context.Context, req Request, step string, err error) Result {
	if ctx.Err() != nil {
		return Result{Decision: declined(StatusDeclined, ReasonTimeout), Fresh: true}
	}
	e.logger.ErrorContext(ctx, "authorization failed", "tenant_id", req.TenantID.String(), "auth_id", req.AuthID,
		"step", step, "error", ErrorDetail(err))
	return Result{Decision: declined(StatusDeclined, ReasonInternalError), Fresh: true}
}

func declined(status, reason string) Decision {
	return Decision{Decision: Declined, Status: status, DeclineReason: reason}
}

// inProgressTimeout is the answer at the deadline for an authorization still being decided: no debit yet is a
// plain decline, a sent debit is TIMED_OUT (UC-1 steps 5 and 13).
func inProgressTimeout(status string) Decision {
	if status == StatusDebitSubmitted {
		return declined(StatusTimedOut, ReasonTimeout)
	}
	return declined(StatusDeclined, ReasonTimeout)
}

// stored is the answer of a decided authorization.
func stored(row repository.GetDecisionStateRow) Decision {
	switch row.Status {
	case StatusApproved, StatusDebitConfirmed, StatusDebitLost:
		d := Decision{Decision: Approved, Status: row.Status, TokenAmount: row.TokenAmount, Rate: row.Rate,
			BufferBps: row.BufferBps, TxHash: row.TxHash}
		if row.Token != nil {
			d.Token = *row.Token
		}
		return d
	}
	d := Decision{Decision: Declined, Status: row.Status}
	if row.DeclineReason != nil {
		d.DeclineReason = *row.DeclineReason
	}
	return d
}

// ErrorDetail describes an error without a host, a user or a connection string: the message of a PostgreSQL error
// names none; a failed connection, which names them, becomes a fixed text.
func ErrorDetail(err error) string {
	var (
		pgErr   *pgconn.PgError
		connErr *pgconn.ConnectError
		netErr  *net.OpError
	)
	switch {
	case errors.As(err, &pgErr):
		return fmt.Sprintf("database: %s (SQLSTATE %s)", pgErr.Message, pgErr.Code)
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &connErr), errors.As(err, &netErr):
		return "database: the connection failed"
	}
	return err.Error()
}
