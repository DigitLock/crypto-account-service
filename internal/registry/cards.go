package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Statuses of a card and audit actions of the card registry (SRS — Core §2.4 cards, audit_log).
const (
	CardActive           = "ACTIVE"
	CardFrozen           = "FROZEN"
	ActionCardRegistered = "CARD_REGISTERED"
	ActionCardUpdated    = "CARD_UPDATED"

	codeForeignKeyViolation = "23503"
)

// Errors of the card registry. The API maps them to gRPC statuses.
var (
	// ErrCardNotFound: no such card in the tenant. Another tenant's card is not found too.
	ErrCardNotFound = errors.New("no such card")
	// ErrCardExists: the card_ref is registered with different data.
	ErrCardExists = errors.New("the card is already registered with different data")
	// ErrConnectionNotUsable: the connection is not a wallet, not ACTIVE or DEGRADED, or of another owner (EC-113).
	ErrConnectionNotUsable = errors.New("the connection is not an ACTIVE wallet of this owner")
	// ErrConnectionHasCards: cards are bound to the connection (EC-114).
	ErrConnectionHasCards = errors.New("cards are bound to the connection")
	// ErrAuthorizationNotFound: no such authorization in the tenant.
	ErrAuthorizationNotFound = errors.New("no such authorization")
)

// Card is a card as the API returns it (SRS — Core §2.1.5). DailyLimit is a base-unit integer string.
type Card struct {
	CardRef       string
	OwnerRef      string
	ConnectionID  uuid.UUID
	WalletAddress string
	Status        string
	DailyLimit    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RegisterInput is a validated RegisterCard request of a tenant. DailyLimit has no leading zeros.
type RegisterInput struct {
	TenantID     uuid.UUID
	CredentialID uuid.UUID
	CardRef      string
	OwnerRef     string
	ConnectionID uuid.UUID
	DailyLimit   string
}

// CardPosition is a place in the order created_at, card_ref of the cards of a tenant.
type CardPosition struct {
	CreatedAt time.Time
	CardRef   string
}

// Cards manages the card registry on the pool of cas_server (SRS — Core UC-103) and reads the
// authorizations, which only card-auth writes.
type Cards struct {
	db  DB
	now func() time.Time
}

// NewCards returns Cards. now is the clock; the logic never calls time.Now itself.
func NewCards(db DB, now func() time.Time) *Cards {
	return &Cards{db: db, now: now}
}

// Register binds a card_ref to a wallet connection (SRS — Core §2.1.5). The same request again returns the
// existing card; other data for a known card_ref is ErrCardExists. The card and its audit row are written in
// one transaction.
func (c *Cards) Register(ctx context.Context, in RegisterInput) (Card, error) {
	var out Card
	err := pgx.BeginFunc(ctx, c.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		existing, err := q.GetCard(ctx, repository.GetCardParams{TenantID: in.TenantID, CardRef: in.CardRef})
		if err == nil {
			out, err = sameCard(in, existing)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read the card: %w", err)
		}

		conn, err := q.GetConnection(ctx, repository.GetConnectionParams{ID: in.ConnectionID, TenantID: in.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionNotFound
		}
		if err != nil {
			return fmt.Errorf("read the connection: %w", err)
		}
		// DEGRADED is a label only (SRS — Core §2.3.1): the wallet is usable; CREDENTIALS_INVALID is not (D-12).
		usable := conn.Status == ConnectionActive || conn.Status == ConnectionDegraded
		if conn.SourceKind != connector.KindEVM || !usable || conn.OwnerRef != in.OwnerRef {
			return ErrConnectionNotUsable
		}

		now := c.now().UTC().Truncate(time.Microsecond)
		_, err = q.InsertCard(ctx, repository.InsertCardParams{
			TenantID: in.TenantID, CardRef: in.CardRef, OwnerRef: in.OwnerRef, ConnectionID: in.ConnectionID,
			DailyLimit: in.DailyLimit, Now: now,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Registered by a concurrent call after the first read.
			existing, err := q.GetCard(ctx, repository.GetCardParams{TenantID: in.TenantID, CardRef: in.CardRef})
			if err != nil {
				return fmt.Errorf("read the card: %w", err)
			}
			out, err = sameCard(in, existing)
			return err
		case isForeignKeyViolation(err):
			// The connection was deleted by a concurrent call.
			return ErrConnectionNotFound
		case err != nil:
			return fmt.Errorf("insert the card: %w", err)
		}

		details := map[string]string{
			"owner_ref": in.OwnerRef, "connection_id": in.ConnectionID.String(), "daily_limit": in.DailyLimit,
		}
		if err := c.audit(ctx, q, in.TenantID, in.CredentialID, ActionCardRegistered, in.CardRef, details); err != nil {
			return err
		}
		row, err := q.GetCard(ctx, repository.GetCardParams{TenantID: in.TenantID, CardRef: in.CardRef})
		if err != nil {
			return fmt.Errorf("read the card: %w", err)
		}
		out = toCard(row)
		return nil
	})
	return out, err
}

// sameCard returns the stored card when the request carries its data, else ErrCardExists.
func sameCard(in RegisterInput, row repository.GetCardRow) (Card, error) {
	if row.OwnerRef != in.OwnerRef || row.ConnectionID != in.ConnectionID || row.DailyLimit != in.DailyLimit {
		return Card{}, ErrCardExists
	}
	return toCard(row), nil
}

// Update changes the daily limit and the status of a card; nil leaves a field as it is. A value equal to the
// stored one changes nothing and writes no audit row; a change sets updated_at and writes CARD_UPDATED with
// the changed fields, in one transaction. dailyLimit has no leading zeros.
func (c *Cards) Update(ctx context.Context, tenantID, credentialID uuid.UUID, cardRef string, dailyLimit, status *string) (Card, error) {
	var out Card
	err := pgx.BeginFunc(ctx, c.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := q.LockCard(ctx, repository.LockCardParams{TenantID: tenantID, CardRef: cardRef})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCardNotFound
		}
		if err != nil {
			return fmt.Errorf("lock the card: %w", err)
		}
		next := repository.UpdateCardParams{ID: cur.ID, DailyLimit: cur.DailyLimit, Status: cur.Status}
		changed := map[string]string{}
		if dailyLimit != nil && *dailyLimit != cur.DailyLimit {
			next.DailyLimit = *dailyLimit
			changed["daily_limit"] = *dailyLimit
		}
		if status != nil && *status != cur.Status {
			next.Status = *status
			changed["status"] = *status
		}
		if len(changed) > 0 {
			next.Now = c.now().UTC().Truncate(time.Microsecond)
			if err := q.UpdateCard(ctx, next); err != nil {
				return fmt.Errorf("update the card: %w", err)
			}
			if err := c.audit(ctx, q, tenantID, credentialID, ActionCardUpdated, cardRef, changed); err != nil {
				return err
			}
		}
		row, err := q.GetCard(ctx, repository.GetCardParams{TenantID: tenantID, CardRef: cardRef})
		if err != nil {
			return fmt.Errorf("read the card: %w", err)
		}
		out = toCard(row)
		return nil
	})
	return out, err
}

// Get returns a card of the tenant.
func (c *Cards) Get(ctx context.Context, tenantID uuid.UUID, cardRef string) (Card, error) {
	row, err := repository.New(c.db).GetCard(ctx, repository.GetCardParams{TenantID: tenantID, CardRef: cardRef})
	if errors.Is(err, pgx.ErrNoRows) {
		return Card{}, ErrCardNotFound
	}
	if err != nil {
		return Card{}, fmt.Errorf("read the card: %w", err)
	}
	return toCard(row), nil
}

// List returns up to limit cards of the tenant after the position, in the order created_at, card_ref, and
// whether more follow. ownerRef filters exactly when it is not empty.
func (c *Cards) List(ctx context.Context, tenantID uuid.UUID, ownerRef string, after CardPosition, limit int32) ([]Card, bool, error) {
	params := repository.ListCardsPageParams{
		TenantID: tenantID, AfterCreatedAt: after.CreatedAt, AfterCardRef: after.CardRef, PageLimit: limit + 1,
	}
	if ownerRef != "" {
		params.OwnerRef = &ownerRef
	}
	rows, err := repository.New(c.db).ListCardsPage(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list the cards: %w", err)
	}
	more := len(rows) > int(limit)
	if more {
		rows = rows[:limit]
	}
	out := make([]Card, 0, len(rows))
	for _, r := range rows {
		out = append(out, toCard(repository.GetCardRow(r)))
	}
	return out, more, nil
}

func (c *Cards) audit(ctx context.Context, q *repository.Queries, tenantID, credentialID uuid.UUID, action, objectID string, details any) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if err := q.InsertAuditLog(ctx, repository.InsertAuditLogParams{
		TenantID: tenantID, CredentialID: &credentialID, Action: action, ObjectID: objectID, Details: raw,
	}); err != nil {
		return fmt.Errorf("insert the audit row: %w", err)
	}
	return nil
}

func toCard(r repository.GetCardRow) Card {
	return Card{
		CardRef: r.CardRef, OwnerRef: r.OwnerRef, ConnectionID: r.ConnectionID, WalletAddress: r.WalletAddress,
		Status: r.Status, DailyLimit: r.DailyLimit, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == codeForeignKeyViolation
}

// Authorization is an authorization as the API returns it (SRS — Card Spend §2.1.4). A value the
// authorization does not have is empty: a tombstone has no amount, currency, token or quote.
type Authorization struct {
	AuthID         string
	Status         string
	DeclineReason  string
	Amount         string
	Currency       string
	Token          string
	TokenAmount    string
	Rate           string
	BufferBps      *int32 // nil: no quote
	DebitedAmount  string
	ReturnedAmount string
	CardRef        string
	ReceivedAt     time.Time
	DecidedAt      *time.Time
	// TxHash is the debit transaction hash, 0x and 64 hexadecimal characters; empty before the first send.
	TxHash  string
	Returns []Return
	History []StatusChange
}

// Return is a return of an authorization.
type Return struct {
	ReturnID    string
	Type        string
	Status      string
	TokenAmount string
	CreatedAt   time.Time
	// TxHash is the refund transaction hash; empty before the first send and for NOTHING_TO_RETURN.
	TxHash string
}

// StatusChange is one record of the status history.
type StatusChange struct {
	Status string
	Reason string
	At     time.Time
}

// AuthorizationFilter are the filters of ListAuthorizations, combined with AND; a zero value does not filter.
type AuthorizationFilter struct {
	CardRef, OwnerRef, Status string
	From, To                  *time.Time
}

// AuthorizationPosition is a place in the order received_at descending, auth_id.
type AuthorizationPosition struct {
	ReceivedAt time.Time
	AuthID     string
}

// Authorization returns an authorization of the tenant with its returns, in created_at order, and its history,
// in event order. It reads only: card-auth writes the authorizations.
func (c *Cards) Authorization(ctx context.Context, tenantID uuid.UUID, authID string) (Authorization, error) {
	q := repository.New(c.db)
	row, err := q.GetAuthorization(ctx, repository.GetAuthorizationParams{TenantID: tenantID, AuthID: authID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Authorization{}, ErrAuthorizationNotFound
	}
	if err != nil {
		return Authorization{}, fmt.Errorf("read the authorization: %w", err)
	}
	out := toAuthorization(row)
	returns, err := q.ListAuthorizationReturns(ctx, row.ID)
	if err != nil {
		return Authorization{}, fmt.Errorf("read the returns: %w", err)
	}
	out.Returns = make([]Return, 0, len(returns))
	for _, r := range returns {
		out.Returns = append(out.Returns, Return(r))
	}
	events, err := q.ListAuthorizationEvents(ctx, row.ID)
	if err != nil {
		return Authorization{}, fmt.Errorf("read the history: %w", err)
	}
	out.History = make([]StatusChange, 0, len(events))
	for _, e := range events {
		sc := StatusChange{Status: e.ToStatus, At: e.CreatedAt}
		if e.Reason != nil {
			sc.Reason = *e.Reason
		}
		out.History = append(out.History, sc)
	}
	return out, nil
}

// Authorizations returns up to limit authorizations of the tenant after the position, in the order
// received_at descending, auth_id, without returns and history, and whether more follow. after nil: the
// first page.
func (c *Cards) Authorizations(ctx context.Context, tenantID uuid.UUID, f AuthorizationFilter, after *AuthorizationPosition, limit int32) ([]Authorization, bool, error) {
	params := repository.ListAuthorizationsPageParams{
		TenantID: tenantID, ReceivedFrom: f.From, ReceivedTo: f.To, PageLimit: limit + 1,
	}
	if after != nil {
		params.AfterReceivedAt = &after.ReceivedAt
		params.AfterAuthID = &after.AuthID
	}
	for _, p := range []struct {
		dst **string
		v   string
	}{{&params.CardRef, f.CardRef}, {&params.OwnerRef, f.OwnerRef}, {&params.Status, f.Status}} {
		if p.v != "" {
			v := p.v
			*p.dst = &v
		}
	}
	rows, err := repository.New(c.db).ListAuthorizationsPage(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list the authorizations: %w", err)
	}
	more := len(rows) > int(limit)
	if more {
		rows = rows[:limit]
	}
	out := make([]Authorization, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAuthorization(repository.GetAuthorizationRow(r)))
	}
	return out, more, nil
}

func toAuthorization(r repository.GetAuthorizationRow) Authorization {
	out := Authorization{
		AuthID: r.AuthID, Status: r.Status, Amount: r.Amount, TokenAmount: r.TokenAmount, Rate: r.Rate,
		BufferBps: r.BufferBps, DebitedAmount: r.DebitedAmount, ReturnedAmount: r.ReturnedAmount,
		ReceivedAt: r.ReceivedAt, DecidedAt: r.DecidedAt, TxHash: r.TxHash,
	}
	for _, p := range []struct {
		dst *string
		v   *string
	}{{&out.DeclineReason, r.DeclineReason}, {&out.Currency, r.FiatCurrency}, {&out.Token, r.Token}, {&out.CardRef, r.CardRef}} {
		if p.v != nil {
			*p.dst = *p.v
		}
	}
	return out
}
