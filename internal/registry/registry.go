// Package registry manages tenants, service tokens and processor credentials (SRS — Core UC-105).
// Every change and its audit row are written in one transaction (FR-121). Audit rows of these
// operations have no credential_id: they are made by the operator through casctl.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/auth"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Statuses of a tenant and kinds of a credential (SRS — Core §2.4).
const (
	TenantActive   = "ACTIVE"
	TenantDisabled = "DISABLED"
	KindService    = "SERVICE_TOKEN"
	KindProcessor  = "PROCESSOR_BASIC"
)

// Audit actions of this package (SRS — Core §2.4 audit_log).
const (
	ActionTenantCreated     = "TENANT_CREATED"
	ActionTenantDisabled    = "TENANT_DISABLED"
	ActionTenantEnabled     = "TENANT_ENABLED"
	ActionCredentialIssued  = "CREDENTIAL_ISSUED"
	ActionCredentialRevoked = "CREDENTIAL_REVOKED"
)

var (
	ErrInvalidName    = errors.New("tenant name must be 1 to 64 characters of a-z, 0-9, - and _, the first one a letter or a digit")
	ErrTenantExists   = errors.New("a tenant with this name exists")
	ErrTenantNotFound = errors.New("no tenant with this name")
	ErrTokenNotFound  = errors.New("no service token with this key_id")
	ErrPairNotFound   = errors.New("no processor credential with this username")
)

const codeUniqueViolation = "23505"

// tenantName is the rule of a tenant name (SRS — Core UC-105, row 1).
var tenantName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Tenant is a tenant as listed.
type Tenant struct {
	ID        uuid.UUID
	Name      string
	Status    string
	CreatedAt time.Time
}

// Token is a service token as listed: no secret and no hash.
type Token struct {
	KeyID     string
	Tenant    string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// IssuedToken is returned once by IssueToken. Value is the only copy of the token.
type IssuedToken struct {
	Value  string
	KeyID  string
	Tenant string
}

// IssuedPair is returned once by IssueProcessorCredential. Password is the only copy of the password.
type IssuedPair struct {
	Username string
	Password string
	Tenant   string
}

// Registry works on a pool of the owner role: cas_server cannot write tenants or credentials.
type Registry struct {
	pool *pgxpool.Pool
}

// New returns a Registry on pool.
func New(pool *pgxpool.Pool) *Registry {
	return &Registry{pool: pool}
}

// CreateTenant creates an ACTIVE tenant. A name against the rule or an existing name is refused
// and nothing is stored (EC-120). The name is used as given.
func (r *Registry) CreateTenant(ctx context.Context, name string) (Tenant, error) {
	if !tenantName.MatchString(name) {
		return Tenant{}, ErrInvalidName
	}
	var t Tenant
	err := r.inTx(ctx, func(q *repository.Queries) error {
		row, err := q.CreateTenant(ctx, name)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrTenantExists
			}
			return err
		}
		t = Tenant(row)
		return audit(ctx, q, row.ID, ActionTenantCreated, row.ID.String(), map[string]string{"name": name})
	})
	return t, err
}

// DisableTenant sets a tenant DISABLED. It reports false and writes nothing when it already is.
func (r *Registry) DisableTenant(ctx context.Context, name string) (bool, error) {
	return r.setStatus(ctx, name, TenantDisabled, ActionTenantDisabled)
}

// EnableTenant sets a tenant ACTIVE. It reports false and writes nothing when it already is.
func (r *Registry) EnableTenant(ctx context.Context, name string) (bool, error) {
	return r.setStatus(ctx, name, TenantActive, ActionTenantEnabled)
}

func (r *Registry) setStatus(ctx context.Context, name, status, action string) (bool, error) {
	var changed bool
	err := r.inTx(ctx, func(q *repository.Queries) error {
		t, err := q.GetTenantByName(ctx, name)
		if err != nil {
			return notFound(err, ErrTenantNotFound)
		}
		n, err := q.SetTenantStatus(ctx, repository.SetTenantStatusParams{ID: t.ID, Status: status})
		if err != nil || n == 0 {
			return err
		}
		changed = true
		return audit(ctx, q, t.ID, action, t.ID.String(), map[string]string{"status": status})
	})
	return changed, err
}

// IssueToken creates a service token for a tenant. The token is returned once and never stored.
// A tenant may hold several valid tokens.
func (r *Registry) IssueToken(ctx context.Context, tenantName string) (IssuedToken, error) {
	value, keyID, hash, err := auth.New()
	if err != nil {
		return IssuedToken{}, fmt.Errorf("generate a token: %w", err)
	}
	err = r.inTx(ctx, func(q *repository.Queries) error {
		t, err := q.GetTenantByName(ctx, tenantName)
		if err != nil {
			return notFound(err, ErrTenantNotFound)
		}
		if _, err := q.CreateCredential(ctx, repository.CreateCredentialParams{
			TenantID: t.ID, Kind: KindService, KeyID: keyID, SecretHash: hash,
		}); err != nil {
			return err
		}
		return audit(ctx, q, t.ID, ActionCredentialIssued, keyID, map[string]string{"kind": KindService})
	})
	if err != nil {
		return IssuedToken{}, err
	}
	return IssuedToken{Value: value, KeyID: keyID, Tenant: tenantName}, nil
}

// RevokeToken revokes a service token. It reports false and writes nothing when it already is revoked.
func (r *Registry) RevokeToken(ctx context.Context, keyID string) (bool, error) {
	var changed bool
	err := r.inTx(ctx, func(q *repository.Queries) error {
		row, err := q.RevokeServiceToken(ctx, keyID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown, or revoked already.
			if _, err := q.GetServiceTokenByKeyID(ctx, keyID); err != nil {
				return notFound(err, ErrTokenNotFound)
			}
			return nil
		}
		if err != nil {
			return err
		}
		changed = true
		return audit(ctx, q, row.TenantID, ActionCredentialRevoked, keyID, nil)
	})
	return changed, err
}

// IssueProcessorCredential creates a Basic pair of a tenant for the processor API of card-auth (UC-105 row 6).
// The password is returned once and never stored.
func (r *Registry) IssueProcessorCredential(ctx context.Context, tenantName string) (IssuedPair, error) {
	username, password, hash, err := auth.NewBasic()
	if err != nil {
		return IssuedPair{}, fmt.Errorf("generate a processor credential: %w", err)
	}
	err = r.inTx(ctx, func(q *repository.Queries) error {
		t, err := q.GetTenantByName(ctx, tenantName)
		if err != nil {
			return notFound(err, ErrTenantNotFound)
		}
		if _, err := q.CreateCredential(ctx, repository.CreateCredentialParams{
			TenantID: t.ID, Kind: KindProcessor, KeyID: username, SecretHash: hash,
		}); err != nil {
			return err
		}
		return audit(ctx, q, t.ID, ActionCredentialIssued, username, map[string]string{"kind": KindProcessor})
	})
	if err != nil {
		return IssuedPair{}, err
	}
	return IssuedPair{Username: username, Password: password, Tenant: tenantName}, nil
}

// RevokeProcessorCredential revokes a processor credential (UC-105 row 7). It reports false and writes
// nothing when it already is revoked.
func (r *Registry) RevokeProcessorCredential(ctx context.Context, username string) (bool, error) {
	var changed bool
	err := r.inTx(ctx, func(q *repository.Queries) error {
		row, err := q.RevokeProcessorCredential(ctx, username)
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown, or revoked already.
			if _, err := q.GetProcessorCredentialByKeyID(ctx, username); err != nil {
				return notFound(err, ErrPairNotFound)
			}
			return nil
		}
		if err != nil {
			return err
		}
		changed = true
		return audit(ctx, q, row.TenantID, ActionCredentialRevoked, username, map[string]string{"kind": KindProcessor})
	})
	return changed, err
}

// ListProcessorCredentials returns the processor credentials of a tenant, or of all tenants when tenantName
// is empty. No password and no hash.
func (r *Registry) ListProcessorCredentials(ctx context.Context, tenantName string) ([]Token, error) {
	q := repository.New(r.pool)
	var tenantID *uuid.UUID
	if tenantName != "" {
		t, err := q.GetTenantByName(ctx, tenantName)
		if err != nil {
			return nil, notFound(err, ErrTenantNotFound)
		}
		tenantID = &t.ID
	}
	rows, err := q.ListProcessorCredentials(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(rows))
	for _, row := range rows {
		out = append(out, Token{KeyID: row.KeyID, Tenant: row.TenantName, CreatedAt: row.CreatedAt, RevokedAt: row.RevokedAt})
	}
	return out, nil
}

// ListTenants returns all tenants by name.
func (r *Registry) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := repository.New(r.pool).ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	tenants := make([]Tenant, 0, len(rows))
	for _, row := range rows {
		tenants = append(tenants, Tenant(row))
	}
	return tenants, nil
}

// ListTokens returns the service tokens of a tenant, or of all tenants when tenantName is empty.
func (r *Registry) ListTokens(ctx context.Context, tenantName string) ([]Token, error) {
	q := repository.New(r.pool)
	var tenantID *uuid.UUID
	if tenantName != "" {
		t, err := q.GetTenantByName(ctx, tenantName)
		if err != nil {
			return nil, notFound(err, ErrTenantNotFound)
		}
		tenantID = &t.ID
	}
	rows, err := q.ListServiceTokens(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	tokens := make([]Token, 0, len(rows))
	for _, row := range rows {
		tokens = append(tokens, Token{
			KeyID: row.KeyID, Tenant: row.TenantName, CreatedAt: row.CreatedAt, RevokedAt: row.RevokedAt,
		})
	}
	return tokens, nil
}

func (r *Registry) inTx(ctx context.Context, fn func(q *repository.Queries) error) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(repository.New(tx))
	})
}

func audit(ctx context.Context, q *repository.Queries, tenantID uuid.UUID, action, objectID string, details any) error {
	var raw []byte
	if details != nil {
		var err error
		if raw, err = json.Marshal(details); err != nil {
			return err
		}
	}
	return q.InsertAuditLog(ctx, repository.InsertAuditLogParams{
		TenantID: tenantID, Action: action, ObjectID: objectID, Details: raw,
	})
}

func notFound(err, sentinel error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return sentinel
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation
}

// FakeSeed reports what AddFakeSource added.
type FakeSeed struct {
	SourceAdded  bool
	AliasesAdded []string
}

// fakeAssets are the assets of the fake connector: native code and canonical asset.
var fakeAssets = [][2]string{{"BTC", "BTC"}, {"USDT", "USDT"}}

// AddFakeSource adds the development seed of the fake connector in one transaction: the source fake, kind
// EXCHANGE, enabled, empty config, and the aliases of its two assets, BTC and USDT. What exists stays as
// it is. No audit row.
func (r *Registry) AddFakeSource(ctx context.Context) (FakeSeed, error) {
	var seed FakeSeed
	err := r.inTx(ctx, func(q *repository.Queries) error {
		seed = FakeSeed{}
		n, err := q.AddFakeSource(ctx)
		if err != nil {
			return err
		}
		seed.SourceAdded = n == 1
		for _, a := range fakeAssets {
			n, err := q.AddFakeAlias(ctx, repository.AddFakeAliasParams{NativeAsset: a[0], Asset: a[1]})
			if err != nil {
				return err
			}
			if n == 1 {
				seed.AliasesAdded = append(seed.AliasesAdded, a[0])
			}
		}
		return nil
	})
	return seed, err
}
