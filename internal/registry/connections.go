package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Status and permission values of a connection (SRS — Core §2.3.1, EC-103).
const (
	ConnectionActive        = "ACTIVE"
	PermissionUnverified    = "UNVERIFIED"
	ActionConnectionCreated = "CONNECTION_CREATED"
	ActionConnectionDeleted = "CONNECTION_DELETED"

	connectionAccountKey = "connections_account_key"
	fingerprintRunes     = 4
)

// Errors of Create. The API maps them to gRPC statuses; any other error is an internal failure.
var (
	ErrSourceNotFound = errors.New("no source with this code")
	ErrSourceDisabled = errors.New("the source is not available")
	ErrKeyInvalid     = errors.New("the source rejects the key")
	ErrUnavailable    = errors.New("the source cannot be reached or answers with a rate limit")
	ErrAlreadyExists  = errors.New("this account is already connected")
	// ErrConnectionNotFound: no such connection in the tenant. Another tenant's connection is not found too.
	ErrConnectionNotFound = errors.New("no such connection")
)

// InvalidArgumentError carries a message that is safe to return to the caller.
type InvalidArgumentError struct{ Message string }

func (e *InvalidArgumentError) Error() string { return e.Message }

// KeyNotReadOnlyError names the permissions beyond reading.
type KeyNotReadOnlyError struct{ Permissions []string }

func (e *KeyNotReadOnlyError) Error() string {
	return fmt.Sprintf("the key is not read-only: %v", e.Permissions)
}

// DB is what Connections needs from the database: queries and transactions. *pgxpool.Pool satisfies it.
type DB interface {
	repository.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Connection is a connection as the API returns it (SRS — Core §2.1.2).
type Connection struct {
	ID             uuid.UUID
	Source         string
	SourceKind     string
	OwnerRef       string
	Label          string
	Status         string
	KeyFingerprint string
	Permissions    []string
	CreatedAt      time.Time
	// WalletAddress is the EIP-55 address of an EVM wallet; empty for an exchange.
	WalletAddress string
}

// CreateInput is a validated CreateConnection request of a tenant.
type CreateInput struct {
	TenantID     uuid.UUID
	CredentialID uuid.UUID
	OwnerRef     string
	Source       string
	Label        string
	Credentials  connector.Credentials
}

// Connections manages connections on the pool of cas_server (SRS — Core UC-101).
type Connections struct {
	db         DB
	vault      vault.Vault
	connectors *connector.Set
	limiters   *limiter.Set
	now        func() time.Time
	checkWait  time.Duration
}

// NewConnections returns Connections. limiters is the set of the process, shared with the engine; the key
// check of Create waits for the limiter of its source at most checkWait. now is the clock; the logic never
// calls time.Now itself.
func NewConnections(db DB, v vault.Vault, connectors *connector.Set, limiters *limiter.Set, now func() time.Time, checkWait time.Duration) *Connections {
	return &Connections{db: db, vault: v, connectors: connectors, limiters: limiters, now: now, checkWait: checkWait}
}

// Sources returns the available sources, ordered by code.
func (c *Connections) Sources(ctx context.Context) ([]connector.Source, error) {
	rows, err := repository.New(c.db).ListSources(ctx)
	if err != nil {
		return nil, err
	}
	var available []connector.Source
	for _, row := range rows {
		if src := toSource(row); c.connectors.Available(src) {
			available = append(available, src)
		}
	}
	return available, nil
}

// Create runs UC-101 steps 2 to 9.
func (c *Connections) Create(ctx context.Context, in CreateInput) (Connection, error) {
	q := repository.New(c.db)

	// Step 2: the source exists and is available.
	row, err := q.GetSourceByCode(ctx, in.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ErrSourceNotFound
	}
	if err != nil {
		return Connection{}, fmt.Errorf("read the source: %w", err)
	}
	src := toSource(row)
	if !c.connectors.Available(src) {
		return Connection{}, ErrSourceDisabled
	}
	conn, _ := c.connectors.For(src)

	key := in.Credentials.ExchangeKey
	switch {
	case src.Kind == connector.KindEVM && key != nil:
		return Connection{}, &InvalidArgumentError{Message: "source " + src.Code + " is an EVM network: send wallet, not exchange_key"}
	case src.Kind == connector.KindExchange && key == nil:
		return Connection{}, &InvalidArgumentError{Message: "source " + src.Code + " is an exchange: send exchange_key, not wallet"}
	}

	// Steps 3 and 5: the account check, through the limiter of the source with a bounded wait.
	lim, err := c.limiters.For(src, conn)
	if err != nil {
		return Connection{}, fmt.Errorf("the limiter of the source: %w", err)
	}
	info, err := conn.CheckAccount(ctx, src, in.Credentials, lim.WithMaxWait(c.checkWait))
	if err != nil {
		return Connection{}, checkError(err)
	}
	if info.Identity == "" {
		return Connection{}, errors.New("the connector returned no account identity")
	}

	now := c.now().UTC().Truncate(time.Microsecond)
	id := uuid.New()
	params := repository.InsertConnectionParams{
		ID:              id,
		TenantID:        in.TenantID,
		OwnerRef:        in.OwnerRef,
		SourceID:        row.ID,
		ExternalAccount: info.Identity,
		Status:          ConnectionActive,
		CreatedAt:       now,
	}
	if in.Label != "" {
		params.Label = &in.Label
	}
	details := map[string]any{"source": src.Code, "owner_ref": in.OwnerRef}

	if key != nil {
		// Step 4: read-only keys only.
		perms, err := permissions(conn.Capabilities(), info.Permissions)
		if err != nil {
			return Connection{}, err
		}
		// Step 7: encrypt key and secret; keep the fingerprint.
		plaintext, err := json.Marshal(struct {
			APIKey    string `json:"api_key"`
			APISecret string `json:"api_secret"`
		}{key.APIKey.Value(), key.APISecret.Value()})
		if err != nil {
			return Connection{}, err
		}
		block, kekVersion, err := c.vault.Encrypt(id, plaintext)
		clear(plaintext)
		if err != nil {
			return Connection{}, fmt.Errorf("encrypt the key: %w", err)
		}
		fingerprint := lastRunes(key.APIKey.Value(), fingerprintRunes)
		params.CredentialsEnc = block
		params.KekVersion = &kekVersion
		params.KeyFingerprint = &fingerprint
		params.Permissions = perms
		params.PermissionsCheckedAt = &now
		details["permissions"] = perms
	}

	streams, err := conn.Streams(ctx, src, info)
	if err != nil {
		return Connection{}, fmt.Errorf("declare the streams: %w", err)
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return Connection{}, err
	}

	// Steps 6, 8 and 9: one transaction; the unique constraint decides on a second connection.
	err = pgx.BeginFunc(ctx, c.db, func(tx pgx.Tx) error {
		qtx := repository.New(tx)
		if err := qtx.InsertConnection(ctx, params); err != nil {
			if isUniqueViolationOf(err, connectionAccountKey) {
				return ErrAlreadyExists
			}
			return fmt.Errorf("insert the connection: %w", err)
		}
		for _, s := range streams {
			if err := qtx.InsertSyncCursor(ctx, repository.InsertSyncCursorParams{
				ConnectionID: id, Stream: s.Name, Mode: string(s.FirstMode), Cursor: s.FirstCursor, NextRunAt: now,
			}); err != nil {
				return fmt.Errorf("insert the cursor of %s: %w", s.Name, err)
			}
		}
		credentialID := in.CredentialID
		if err := qtx.InsertAuditLog(ctx, repository.InsertAuditLogParams{
			TenantID: in.TenantID, CredentialID: &credentialID, Action: ActionConnectionCreated,
			ObjectID: id.String(), Details: detailsJSON,
		}); err != nil {
			return fmt.Errorf("insert the audit row: %w", err)
		}
		return nil
	})
	if err != nil {
		return Connection{}, err
	}

	out := Connection{
		ID: id, Source: src.Code, SourceKind: src.Kind, OwnerRef: in.OwnerRef, Label: in.Label,
		Status: ConnectionActive, Permissions: params.Permissions, CreatedAt: now,
	}
	if params.KeyFingerprint != nil {
		out.KeyFingerprint = *params.KeyFingerprint
	}
	if src.Kind == connector.KindEVM {
		out.WalletAddress = info.Identity
	}
	return out, nil
}

// checkError maps a failed account check (UC-101 step 3).
func checkError(err error) error {
	var notReadOnly *connector.KeyNotReadOnlyError
	var rateLimit *connector.RateLimitError
	var invalid *connector.InvalidInputError
	switch {
	case errors.Is(err, connector.ErrKeyRejected):
		return ErrKeyInvalid
	case errors.As(err, &notReadOnly):
		return &KeyNotReadOnlyError{Permissions: notReadOnly.Permissions}
	case errors.Is(err, connector.ErrUnreachable), errors.As(err, &rateLimit), errors.Is(err, limiter.ErrNoBudget):
		return ErrUnavailable
	case errors.As(err, &invalid):
		return &InvalidArgumentError{Message: invalid.Message}
	default:
		return fmt.Errorf("account check: %w", err)
	}
}

// permissions applies UC-101 step 4 and EC-103 to the permissions the connector returned.
func permissions(caps connector.Capabilities, reported []string) ([]string, error) {
	if !caps.PermissionsReadable {
		return []string{PermissionUnverified}, nil
	}
	if len(reported) == 0 {
		return nil, errors.New("the connector reads permissions but returned none")
	}
	var beyond []string
	for _, p := range reported {
		if p != connector.PermissionRead && !slices.Contains(beyond, p) {
			beyond = append(beyond, p)
		}
	}
	if len(beyond) > 0 {
		return nil, &KeyNotReadOnlyError{Permissions: beyond}
	}
	return []string{connector.PermissionRead}, nil
}

func toSource(row repository.Source) connector.Source {
	return connector.Source{Code: row.Code, Kind: row.Kind, Enabled: row.Enabled, Config: row.Config}
}

// lastRunes returns the last n characters of s.
func lastRunes(s string, n int) string {
	count := utf8.RuneCountInString(s)
	if count <= n {
		return s
	}
	i := 0
	for range count - n {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[i:]
}

func isUniqueViolationOf(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation && pgErr.ConstraintName == constraint
}

// StreamHealth is the health of one stream of a connection; the cursor is not part of it.
type StreamHealth struct {
	Stream              string
	Mode                string
	NextRunAt           time.Time
	LastSuccessAt       *time.Time
	LastError           string
	ConsecutiveFailures int32
}

// Position is a place in the order created_at, id of the connections of a tenant.
type Position struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// Get returns a connection of the tenant with the health of its streams, ordered by stream.
// The availability of its source does not matter.
func (c *Connections) Get(ctx context.Context, tenantID, id uuid.UUID) (Connection, []StreamHealth, error) {
	q := repository.New(c.db)
	row, err := q.GetConnection(ctx, repository.GetConnectionParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, nil, ErrConnectionNotFound
	}
	if err != nil {
		return Connection{}, nil, fmt.Errorf("read the connection: %w", err)
	}
	rows, err := q.ListStreamHealth(ctx, repository.ListStreamHealthParams{ConnectionID: id, TenantID: tenantID})
	if err != nil {
		return Connection{}, nil, fmt.Errorf("read the stream health: %w", err)
	}
	streams := make([]StreamHealth, 0, len(rows))
	for _, r := range rows {
		h := StreamHealth{
			Stream: r.Stream, Mode: r.Mode, NextRunAt: r.NextRunAt, LastSuccessAt: r.LastSuccessAt,
			ConsecutiveFailures: r.ConsecutiveFailures,
		}
		if r.LastError != nil {
			h.LastError = *r.LastError
		}
		streams = append(streams, h)
	}
	return toConnection(repository.ListConnectionsPageRow(row)), streams, nil
}

// List returns up to limit connections of the tenant after the position, in the order created_at, id,
// and whether more follow. ownerRef filters exactly when it is not empty. One query per page.
func (c *Connections) List(ctx context.Context, tenantID uuid.UUID, ownerRef string, after Position, limit int32) ([]Connection, bool, error) {
	params := repository.ListConnectionsPageParams{
		TenantID: tenantID, AfterCreatedAt: after.CreatedAt, AfterID: after.ID, PageLimit: limit + 1,
	}
	if ownerRef != "" {
		params.OwnerRef = &ownerRef
	}
	rows, err := repository.New(c.db).ListConnectionsPage(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list the connections: %w", err)
	}
	more := len(rows) > int(limit)
	if more {
		rows = rows[:limit]
	}
	out := make([]Connection, 0, len(rows))
	for _, r := range rows {
		out = append(out, toConnection(r))
	}
	return out, more, nil
}

// Delete deletes a connection of the tenant with its secret, cursors, snapshots and ledger entries, and
// writes the audit row, in one transaction (UC-104 step 2). The card check of step 1 comes with S2.
func (c *Connections) Delete(ctx context.Context, tenantID, credentialID, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, c.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		row, err := q.GetConnection(ctx, repository.GetConnectionParams{ID: id, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionNotFound
		}
		if err != nil {
			return fmt.Errorf("read the connection: %w", err)
		}
		n, err := q.DeleteConnection(ctx, repository.DeleteConnectionParams{ID: id, TenantID: tenantID})
		if err != nil {
			return fmt.Errorf("delete the connection: %w", err)
		}
		if n == 0 {
			// Deleted by a concurrent call.
			return ErrConnectionNotFound
		}
		details, err := json.Marshal(map[string]string{"source": row.SourceCode, "owner_ref": row.OwnerRef})
		if err != nil {
			return err
		}
		if err := q.InsertAuditLog(ctx, repository.InsertAuditLogParams{
			TenantID: tenantID, CredentialID: &credentialID, Action: ActionConnectionDeleted,
			ObjectID: id.String(), Details: details,
		}); err != nil {
			return fmt.Errorf("insert the audit row: %w", err)
		}
		return nil
	})
}

func toConnection(r repository.ListConnectionsPageRow) Connection {
	out := Connection{
		ID: r.ID, Source: r.SourceCode, SourceKind: r.SourceKind, OwnerRef: r.OwnerRef, Status: r.Status,
		Permissions: r.Permissions, CreatedAt: r.CreatedAt,
	}
	if r.Label != nil {
		out.Label = *r.Label
	}
	if r.KeyFingerprint != nil {
		out.KeyFingerprint = *r.KeyFingerprint
	}
	if r.SourceKind == connector.KindEVM {
		out.WalletAddress = r.ExternalAccount
	}
	return out
}
