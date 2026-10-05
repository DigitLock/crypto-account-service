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
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Status and permission values of a connection (SRS — Core §2.3.1, EC-103).
const (
	ConnectionActive        = "ACTIVE"
	PermissionUnverified    = "UNVERIFIED"
	ActionConnectionCreated = "CONNECTION_CREATED"

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
	now        func() time.Time
}

// NewConnections returns Connections. now is the clock; the logic never calls time.Now itself.
func NewConnections(db DB, v vault.Vault, connectors *connector.Set, now func() time.Time) *Connections {
	return &Connections{db: db, vault: v, connectors: connectors, now: now}
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

	// Steps 3 and 5: the account check.
	info, err := conn.CheckAccount(ctx, src, in.Credentials)
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
	case errors.Is(err, connector.ErrUnreachable), errors.As(err, &rateLimit):
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
