// Package returns accepts the returns of card-auth: UC-2 steps 2 to 6 of SRS — Card Spend. A return is accepted in
// one database transaction with its authorization row locked, so two returns of one authorization cannot exceed the
// debit together (FR-12). The tracker (package tracker) executes it on chain. Amounts are exact integers, never
// floats.
package returns

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Return states and types (SRS — Card Spend §2.3.2, §2.4).
const (
	StatusAccepted        = "ACCEPTED"
	StatusSubmitted       = "SUBMITTED"
	StatusIncluded        = "INCLUDED"
	StatusConfirmed       = "CONFIRMED"
	StatusRetrying        = "RETRYING"
	StatusNothingToReturn = "NOTHING_TO_RETURN"
	TypeReversal          = "REVERSAL"
	TypeRefund            = "REFUND"
	TypeLateDebit         = "LATE_DEBIT"
)

// fiatScale is the scale of fiat amounts, NUMERIC(18,4).
const fiatScale = 4

// Errors of Accept; the API maps them to its error codes (§2.1.1).
var (
	// ErrReturnIDConflict: the return_id is known with a different normalized request (409 RETURN_ID_CONFLICT).
	ErrReturnIDConflict = errors.New("return_id is known with a different request")
	// ErrInProgress: the authorization is RECEIVED or DEBIT_SUBMITTED (409 AUTHORIZATION_IN_PROGRESS, EC-16).
	ErrInProgress = errors.New("the authorization is still being decided")
	// ErrExceedsDebit: the amount is above the part not yet returned (422 RETURN_EXCEEDS_DEBIT, EC-17).
	ErrExceedsDebit = errors.New("the amount is above the part not yet returned")
)

// errRaced: a concurrent return took the return_id; the request is read again as a repeated one.
var errRaced = errors.New("a concurrent return took the return_id")

// Request is a validated return request of a tenant.
type Request struct {
	TenantID uuid.UUID
	AuthID   string
	ReturnID string
	Type     string
	Amount   string // a plain decimal without trailing zeros; empty: everything not yet returned
	Hash     []byte // request_hash of the normalized request
}

// Result is the answer of a return (§2.1.3).
type Result struct {
	ReturnID    string
	AuthID      string
	Status      string
	TokenAmount string
}

// DB is what the service needs from the database, role cas_card_auth. *pgxpool.Pool satisfies it.
type DB interface {
	repository.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Service accepts returns.
type Service struct {
	db  DB
	now func() time.Time
}

// New returns the service. now is the clock of created_at, received_at and decided_at.
func New(db DB, now func() time.Time) *Service { return &Service{db: db, now: now} }

// Accept runs UC-2 steps 2 to 6. The errors of the API are ErrReturnIDConflict, ErrInProgress and ErrExceedsDebit;
// any other error is an internal failure.
func (s *Service) Accept(ctx context.Context, req Request) (Result, error) {
	for range 2 {
		var res Result
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			var err error
			res, err = s.accept(ctx, repository.New(tx), req)
			return err
		})
		if errors.Is(err, errRaced) {
			continue
		}
		return res, err
	}
	return Result{}, errors.New("the return_id is taken by a concurrent return of another authorization twice")
}

func (s *Service) accept(ctx context.Context, q *repository.Queries, req Request) (Result, error) {
	// Step 2.
	if res, found, err := s.repeated(ctx, q, req); found || err != nil {
		return res, err
	}

	// Step 3, with the lock of steps 4 and 5.
	auth, err := q.LockAuthorizationForReturn(ctx, repository.LockAuthorizationForReturnParams{TenantID: req.TenantID, AuthID: req.AuthID})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.tombstone(ctx, q, req)
	}
	if err != nil {
		return Result{}, err
	}
	// A return with the same return_id may have been accepted while this request waited for the lock.
	if res, found, err := s.repeated(ctx, q, req); found || err != nil {
		return res, err
	}
	return s.forAuthorization(ctx, q, req, auth)
}

// repeated answers a known return_id: the stored result for the same request, ErrReturnIDConflict for another.
func (s *Service) repeated(ctx context.Context, q *repository.Queries, req Request) (Result, bool, error) {
	r, err := q.GetReturnForRequest(ctx, repository.GetReturnForRequestParams{TenantID: req.TenantID, ReturnID: req.ReturnID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(r.RequestHash, req.Hash) {
		return Result{}, true, ErrReturnIDConflict
	}
	return Result{ReturnID: req.ReturnID, AuthID: r.AuthID, Status: r.Status, TokenAmount: r.TokenAmount}, true, nil
}

// tombstone is step 3 for an unknown auth_id (EC-14, FR-15): the tombstone DECLINED / REVERSED_BEFORE_AUTH with
// no card and no body fields, its one event, and the return NOTHING_TO_RETURN.
func (s *Service) tombstone(ctx context.Context, q *repository.Queries, req Request) (Result, error) {
	now := s.now()
	id, err := q.InsertTombstone(ctx, repository.InsertTombstoneParams{
		TenantID: req.TenantID, AuthID: req.AuthID, ChainAuthID: decision.ChainAuthID(req.TenantID, req.AuthID).Bytes(), Now: now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// An authorization with this auth_id was inserted at the same time (UC-1 step 4): the return is for it.
		auth, err := q.LockAuthorizationForReturn(ctx, repository.LockAuthorizationForReturnParams{TenantID: req.TenantID, AuthID: req.AuthID})
		if err != nil {
			return Result{}, err
		}
		return s.forAuthorization(ctx, q, req, auth)
	}
	if err != nil {
		return Result{}, err
	}
	reason := decision.ReasonReversedBeforeAuth
	if err := q.InsertAuthorizationEvent(ctx, repository.InsertAuthorizationEventParams{
		AuthorizationID: id, ToStatus: decision.StatusDeclined, Reason: &reason, CreatedAt: now,
	}); err != nil {
		return Result{}, err
	}
	return s.insert(ctx, q, req, id, StatusNothingToReturn, fiatOf(req), "0")
}

// forAuthorization is steps 4 to 6 for a locked authorization.
func (s *Service) forAuthorization(ctx context.Context, q *repository.Queries, req Request, auth repository.LockAuthorizationForReturnRow) (Result, error) {
	switch auth.Status {
	case decision.StatusReceived, decision.StatusDebitSubmitted:
		return Result{}, ErrInProgress
	case decision.StatusApproved, decision.StatusDebitConfirmed:
	default:
		// DECLINED, TIMED_OUT, LATE_DEBIT, LATE_DEBIT_REFUNDED, DEBIT_LOST: nothing was debited for the processor, or a
		// late debit is returned in full automatically.
		return s.insert(ctx, q, req, auth.ID, StatusNothingToReturn, fiatOf(req), "0")
	}

	// Step 5: integers only. F, A and the returned fiat in 10^-4 units; D the debited base units.
	debited, ok1 := new(big.Int).SetString(auth.DebitedAmount, 10)
	returned, ok2 := new(big.Int).SetString(auth.ReturnedAmount, 10)
	fiat, err := decision.Units(auth.FiatAmount, fiatScale)
	if !ok1 || !ok2 || err != nil || fiat.Sign() <= 0 {
		return Result{}, errors.New("the stored amounts of the authorization are malformed")
	}
	returnedFiatText, err := q.GetReturnedFiat(ctx, auth.ID)
	if err != nil {
		return Result{}, err
	}
	returnedFiat, err := decision.Units(returnedFiatText, fiatScale)
	if err != nil {
		return Result{}, errors.New("the stored amounts of the returns are malformed")
	}
	remainingFiat := new(big.Int).Sub(fiat, returnedFiat)
	remaining := new(big.Int).Sub(debited, returned)

	amount := remainingFiat
	tokens := new(big.Int).Set(remaining)
	if req.Amount != "" {
		if amount, err = decision.Units(req.Amount, fiatScale); err != nil {
			return Result{}, err
		}
		switch amount.Cmp(remainingFiat) {
		case 1:
			return Result{}, ErrExceedsDebit
		case -1:
			// debited × amount / fiat_amount, rounded down. A return that completes the fiat amount keeps the whole
			// remainder (case 0), so the total returned is the debited amount exactly (FR-12).
			tokens = new(big.Int).Quo(new(big.Int).Mul(debited, amount), fiat)
		}
		if tokens.Cmp(remaining) > 0 {
			return Result{}, ErrExceedsDebit
		}
	}
	fiatText := formatUnits(amount, fiatScale)
	if tokens.Sign() == 0 {
		// Nothing is left to return in tokens.
		return s.insert(ctx, q, req, auth.ID, StatusNothingToReturn, &fiatText, "0")
	}

	// Step 6.
	res, err := s.insert(ctx, q, req, auth.ID, StatusAccepted, &fiatText, tokens.String())
	if err != nil {
		return Result{}, err
	}
	if err := q.AddReturnedAmount(ctx, repository.AddReturnedAmountParams{ID: auth.ID, Amount: tokens.String()}); err != nil {
		return Result{}, err
	}
	return res, nil
}

// insert writes the return row and returns its answer.
func (s *Service) insert(ctx context.Context, q *repository.Queries, req Request, authorizationID uuid.UUID, status string, fiat *string, tokens string) (Result, error) {
	_, err := q.InsertReturn(ctx, repository.InsertReturnParams{
		TenantID: req.TenantID, AuthorizationID: authorizationID, ReturnID: req.ReturnID,
		ChainRefundID: decision.ChainRefundID(req.TenantID, req.ReturnID).Bytes(), Type: req.Type,
		RequestHash: req.Hash, FiatAmount: fiat, TokenAmount: tokens, Status: status, CreatedAt: s.now(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, errRaced
	}
	if err != nil {
		return Result{}, err
	}
	return Result{ReturnID: req.ReturnID, AuthID: req.AuthID, Status: status, TokenAmount: tokens}, nil
}

// fiatOf is the fiat amount of a request stored with a NOTHING_TO_RETURN answer: nil when omitted.
func fiatOf(req Request) *string {
	if req.Amount == "" {
		return nil
	}
	a := req.Amount
	return &a
}

// formatUnits writes an integer of 10^-scale units as a plain decimal.
func formatUnits(u *big.Int, scale int) string {
	s := u.String()
	for len(s) <= scale {
		s = "0" + s
	}
	return decision.TrimDecimal(s[:len(s)-scale] + "." + s[len(s)-scale:])
}
