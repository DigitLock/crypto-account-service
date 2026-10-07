// Package processorapi is the processor API of card-auth (SRS — Card Spend §2.1, api/openapi/card-auth.yaml),
// written by hand (D-7): POST /v1/authorizations and GET /v1/authorizations/{auth_id}. Every response has a body
// and a code of the OpenAPI; tests validate them against it (S2-T203).
package processorapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/auth"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/returns"
)

// Error codes of SRS — Card Spend §2.1.1.
const (
	codeUnauthenticated = "UNAUTHENTICATED"
	codeInvalidRequest  = "INVALID_REQUEST"
	codeAuthIDConflict  = "AUTH_ID_CONFLICT"
	codeNotFound        = "NOT_FOUND"
	codeInternal        = "INTERNAL"

	codeReturnIDConflict   = "RETURN_ID_CONFLICT"
	codeInProgress         = "AUTHORIZATION_IN_PROGRESS"
	codeReturnExceedsDebit = "RETURN_EXCEEDS_DEBIT"
)

const tenantActive = "ACTIVE"

// Lengths of a Basic pair (SRS — Core UC-105 row 6): 12 and 64 lower-case hexadecimal characters.
const (
	usernameLen = 12
	passwordLen = 64
)

// dummyHash is compared when the username is unknown, so an unknown username costs what a wrong password costs.
var dummyHash = auth.Hash(make([]byte, passwordLen/2))

// Authorizer decides an authorization (package decision).
type Authorizer interface {
	Authorize(ctx context.Context, req decision.Request, start time.Time) (decision.Result, error)
}

// Reader reads an authorization of a tenant with its returns and history (registry.Cards).
type Reader interface {
	Authorization(ctx context.Context, tenantID uuid.UUID, authID string) (registry.Authorization, error)
}

// Returner accepts returns (package returns).
type Returner interface {
	Accept(ctx context.Context, req returns.Request) (returns.Result, error)
}

// Deps are the parts of the API.
type Deps struct {
	DB      repository.DBTX // role cas_card_auth: the processor credentials
	Engine  Authorizer
	Returns Returner
	Reads   Reader
	Metrics *Metrics
	Logger  *slog.Logger
}

type api struct{ Deps }

// NewHandler returns the routes of the processor API. Any other route is 404 without a body of the contract.
func NewHandler(d Deps) http.Handler {
	a := &api{d}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/authorizations", a.authorize)
	mux.HandleFunc("GET /v1/authorizations/{auth_id}", a.getAuthorization)
	mux.HandleFunc("POST /v1/authorizations/{auth_id}/returns", a.createReturn)
	return mux
}

// errAuthUnavailable: the credentials could not be read; the request is neither accepted nor refused.
var errAuthUnavailable = errors.New("the processor credentials cannot be read")

// authenticate resolves the tenant of the Basic pair (UC-1 step 1, SRS — Core UC-105). Every refusal is the
// same 401; only a failure to read the credentials is errAuthUnavailable.
func (a *api) authenticate(r *http.Request) (uuid.UUID, error) {
	username, password, ok := r.BasicAuth()
	if !ok || len(username) != usernameLen || len(password) != passwordLen || !lowerHex(username) || !lowerHex(password) {
		return uuid.Nil, errUnauthenticated
	}
	secret, err := hex.DecodeString(password)
	if err != nil {
		return uuid.Nil, errUnauthenticated
	}
	cred, err := repository.New(a.DB).GetProcessorCredentialByKeyID(r.Context(), username)
	if errors.Is(err, pgx.ErrNoRows) {
		auth.Verify(secret, dummyHash)
		return uuid.Nil, errUnauthenticated
	}
	if err != nil {
		a.Logger.ErrorContext(r.Context(), "reading the processor credentials failed", "error", decision.ErrorDetail(err))
		return uuid.Nil, errAuthUnavailable
	}
	if !auth.Verify(secret, cred.SecretHash) || cred.RevokedAt != nil || cred.TenantStatus != tenantActive {
		return uuid.Nil, errUnauthenticated
	}
	return cred.TenantID, nil
}

var errUnauthenticated = errors.New("unauthenticated")

func lowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// authorize is POST /v1/authorizations (SRS — Card Spend §2.1.2, UC-1).
func (a *api) authorize(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	tenantID, authErr := a.authenticate(r)
	if errors.Is(authErr, errUnauthenticated) {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "unauthenticated")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, codeInvalidRequest, fmt.Sprintf("the body must be a JSON object of at most %d bytes", MaxBody))
		return
	}
	req, err := parseAuthorize(body)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, codeInvalidRequest, err.Error())
		return
	}

	// From here the processor always gets a decision (§2.1.1).
	var res decision.Result
	defer func() {
		if p := recover(); p != nil {
			a.Logger.ErrorContext(r.Context(), "authorization failed", "auth_id", req.AuthID, "panic", fmt.Sprint(p))
			res = decision.Result{Decision: decision.Decision{
				Decision: decision.Declined, Status: decision.StatusDeclined, DeclineReason: decision.ReasonInternalError,
			}, Fresh: true}
			writeDecision(w, req.AuthID, res.Decision)
			a.Metrics.observe(start, res)
		}
	}()
	if authErr != nil {
		res = decision.Result{Decision: decision.Decision{
			Decision: decision.Declined, Status: decision.StatusDeclined, DeclineReason: decision.ReasonInternalError,
		}, Fresh: true}
	} else {
		res, err = a.Engine.Authorize(r.Context(), decision.Request{
			TenantID: tenantID, AuthID: req.AuthID, CardRef: req.CardRef, Amount: req.Amount, Currency: req.Currency,
			Merchant: req.Merchant, Hash: req.Hash,
		}, start)
		if errors.Is(err, decision.ErrAuthIDConflict) {
			writeError(w, http.StatusConflict, codeAuthIDConflict, "auth_id is known with a different request")
			return
		}
	}
	writeDecision(w, req.AuthID, res.Decision)
	a.Metrics.observe(start, res)
}

// returnResponse is the body of §2.1.3.
type returnResponse struct {
	ReturnID    string `json:"return_id"`
	AuthID      string `json:"auth_id"`
	Status      string `json:"status"`
	TokenAmount string `json:"token_amount"`
}

// createReturn is POST /v1/authorizations/{auth_id}/returns (SRS — Card Spend §2.1.3, UC-2 steps 1 to 6).
func (a *api) createReturn(w http.ResponseWriter, r *http.Request) {
	tenantID, err := a.authenticate(r)
	switch {
	case errors.Is(err, errUnauthenticated):
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "unauthenticated")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, codeInvalidRequest, fmt.Sprintf("the body must be a JSON object of at most %d bytes", MaxBody))
		return
	}
	authID := r.PathValue("auth_id")
	req, err := parseReturn(body, authID)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, codeInvalidRequest, err.Error())
		return
	}
	res, err := a.Returns.Accept(r.Context(), returns.Request{
		TenantID: tenantID, AuthID: authID, ReturnID: req.ReturnID, Type: req.Type, Amount: req.Amount, Hash: req.Hash,
	})
	switch {
	case errors.Is(err, returns.ErrReturnIDConflict):
		writeError(w, http.StatusConflict, codeReturnIDConflict, "return_id is known with a different request")
	case errors.Is(err, returns.ErrInProgress):
		writeError(w, http.StatusConflict, codeInProgress, "the authorization is still being decided")
	case errors.Is(err, returns.ErrExceedsDebit):
		writeError(w, http.StatusUnprocessableEntity, codeReturnExceedsDebit, "amount is above the part not yet returned")
	case err != nil:
		a.Logger.ErrorContext(r.Context(), "accepting the return failed", "auth_id", authID, "return_id", req.ReturnID,
			"error", decision.ErrorDetail(err))
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
	default:
		writeJSON(w, http.StatusOK, returnResponse{ReturnID: res.ReturnID, AuthID: res.AuthID, Status: res.Status, TokenAmount: res.TokenAmount})
	}
}

// authorizeResponse is the body of §2.1.2.
type authorizeResponse struct {
	AuthID        string     `json:"auth_id"`
	Decision      string     `json:"decision"`
	Status        string     `json:"status"`
	DeclineReason string     `json:"decline_reason,omitempty"`
	Token         string     `json:"token,omitempty"`
	TokenAmount   string     `json:"token_amount,omitempty"`
	Quote         *quoteBody `json:"quote,omitempty"`
	TxHash        string     `json:"tx_hash,omitempty"`
}

// quoteBody is the quote of a response; rate is absent for USD.
type quoteBody struct {
	Rate      string `json:"rate,omitempty"`
	BufferBps int32  `json:"buffer_bps"`
}

func newQuote(rate string, bufferBps *int32) *quoteBody {
	if bufferBps == nil {
		return nil
	}
	return &quoteBody{Rate: decision.TrimDecimal(rate), BufferBps: *bufferBps}
}

func writeDecision(w http.ResponseWriter, authID string, d decision.Decision) {
	body := authorizeResponse{AuthID: authID, Decision: d.Decision, Status: d.Status}
	if d.Decision == decision.Approved {
		body.Token, body.TokenAmount, body.Quote, body.TxHash = d.Token, d.TokenAmount, newQuote(d.Rate, d.BufferBps), d.TxHash
	} else {
		body.DeclineReason = d.DeclineReason
	}
	writeJSON(w, http.StatusOK, body)
}

// authorizationBody is the body of §2.1.4.
type authorizationBody struct {
	AuthID         string         `json:"auth_id"`
	Status         string         `json:"status"`
	DeclineReason  string         `json:"decline_reason,omitempty"`
	Amount         string         `json:"amount,omitempty"`
	Currency       string         `json:"currency,omitempty"`
	Token          string         `json:"token,omitempty"`
	TokenAmount    string         `json:"token_amount,omitempty"`
	Quote          *quoteBody     `json:"quote,omitempty"`
	DebitedAmount  string         `json:"debited_amount"`
	ReturnedAmount string         `json:"returned_amount"`
	TxHash         string         `json:"tx_hash,omitempty"`
	Returns        []returnBody   `json:"returns"`
	History        []statusChange `json:"history"`
}

type returnBody struct {
	ReturnID    string `json:"return_id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	TokenAmount string `json:"token_amount"`
	TxHash      string `json:"tx_hash,omitempty"`
}

type statusChange struct {
	Status string `json:"status"`
	At     string `json:"at"`
}

// timeFormat is RFC 3339 in UTC with milliseconds, as the examples of §2.1.4.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// getAuthorization is GET /v1/authorizations/{auth_id} (SRS — Card Spend §2.1.4).
func (a *api) getAuthorization(w http.ResponseWriter, r *http.Request) {
	tenantID, err := a.authenticate(r)
	switch {
	case errors.Is(err, errUnauthenticated):
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "unauthenticated")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	notFound := func() { writeError(w, http.StatusNotFound, codeNotFound, "authorization not found") }
	authID := r.PathValue("auth_id")
	if !validProcessorID(authID) {
		notFound()
		return
	}
	au, err := a.Reads.Authorization(r.Context(), tenantID, authID)
	if errors.Is(err, registry.ErrAuthorizationNotFound) {
		notFound()
		return
	}
	if err != nil {
		a.Logger.ErrorContext(r.Context(), "reading the authorization failed", "auth_id", authID, "error", decision.ErrorDetail(err))
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	body := authorizationBody{
		AuthID: au.AuthID, Status: au.Status, Amount: au.Amount, Currency: au.Currency, TokenAmount: au.TokenAmount,
		Quote: newQuote(au.Rate, au.BufferBps), DebitedAmount: au.DebitedAmount, ReturnedAmount: au.ReturnedAmount,
		TxHash: au.TxHash, Returns: make([]returnBody, 0, len(au.Returns)), History: make([]statusChange, 0, len(au.History)),
	}
	if au.Status == decision.StatusDeclined || au.Status == decision.StatusTimedOut {
		body.DeclineReason = au.DeclineReason
	}
	if au.TokenAmount != "" {
		body.Token = au.Token
	}
	for _, rt := range au.Returns {
		body.Returns = append(body.Returns, returnBody{
			ReturnID: rt.ReturnID, Type: rt.Type, Status: rt.Status, TokenAmount: rt.TokenAmount, TxHash: rt.TxHash,
		})
	}
	for _, h := range au.History {
		body.History = append(body.History, statusChange{Status: h.Status, At: h.At.UTC().Format(timeFormat)})
	}
	writeJSON(w, http.StatusOK, body)
}

// errorBody is the error body of §2.1.1.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
