package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Errors of TriggerSync (SRS — Core §2.1.1 Method catalogue).
var (
	ErrCredentialsInvalid = errors.New("the connection is stopped: its key was rejected or is no longer read-only")
	ErrCooldown           = errors.New("manual sync requested inside the cooldown")
)

// Freshness is a connection in a GetBalances response (SRS — Core §2.1.3).
type Freshness struct {
	ConnectionID uuid.UUID
	Source       string
	// AsOf is taken_at of the newest snapshot; nil before the first.
	AsOf  *time.Time
	Stale bool
}

// Balance is a balance of the newest snapshot of a connection. Free and Locked are plain decimals.
type Balance struct {
	ConnectionID                    uuid.UUID
	AccountType, Asset, NativeAsset string
	Free, Locked                    string
}

// Balances returns the selected connections of the tenant in the order of ListConnections with their
// freshness, and the balances of the newest snapshot of each. Exactly one of ownerRef and connectionID is
// set. It reads the database only, never a connector.
func (c *Connections) Balances(ctx context.Context, tenantID uuid.UUID, ownerRef string, connectionID *uuid.UUID) ([]Freshness, []Balance, error) {
	q := repository.New(c.db)
	params := repository.ListBalanceConnectionsParams{TenantID: tenantID, ConnectionID: connectionID}
	if connectionID == nil {
		params.OwnerRef = &ownerRef
	}
	rows, err := q.ListBalanceConnections(ctx, params)
	if err != nil {
		return nil, nil, fmt.Errorf("read the connections: %w", err)
	}
	if connectionID != nil && len(rows) == 0 {
		return nil, nil, ErrConnectionNotFound
	}
	aliases, err := q.AliasesBySource(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read the aliases: %w", err)
	}

	now := c.now()
	connections := make([]Freshness, 0, len(rows))
	var snapshots []uuid.UUID
	owner := map[uuid.UUID]uuid.UUID{} // snapshot → connection
	for _, r := range rows {
		f := Freshness{ConnectionID: r.ID, Source: r.SourceCode}
		if r.SnapshotID != uuid.Nil {
			takenAt := r.TakenAt
			f.AsOf = &takenAt
			snapshots = append(snapshots, r.SnapshotID)
			owner[r.SnapshotID] = r.ID
		}
		src := connector.Source{Code: r.SourceCode, Kind: r.SourceKind, Config: r.SourceConfig, Aliases: aliases[r.SourceCode]}
		// Stale without a balance stream, when one never succeeded, or when the oldest success is too old.
		f.Stale = r.Streams == 0 || r.Succeeded < r.Streams || now.Sub(r.OldestSuccess) > connector.StaleAfter(src)
		connections = append(connections, f)
	}
	if len(snapshots) == 0 {
		return connections, nil, nil
	}

	balanceRows, err := q.ListSnapshotBalances(ctx, snapshots)
	if err != nil {
		return nil, nil, fmt.Errorf("read the balances: %w", err)
	}
	bySnapshot := map[uuid.UUID][]Balance{}
	for _, b := range balanceRows {
		bySnapshot[b.SnapshotID] = append(bySnapshot[b.SnapshotID], Balance{
			ConnectionID: owner[b.SnapshotID], AccountType: b.AccountType, Asset: b.Asset,
			NativeAsset: b.NativeAsset, Free: b.Free, Locked: b.Locked,
		})
	}
	var balances []Balance
	for _, id := range snapshots { // in the order of the connections
		balances = append(balances, bySnapshot[id]...)
	}
	return connections, balances, nil
}

// LedgerFilter are the filters of ListLedgerEntries, combined with AND; a zero value does not filter.
type LedgerFilter struct {
	OwnerRef     string
	ConnectionID *uuid.UUID
	Types        []string
	From, To     *time.Time
}

// LedgerEntry is a ledger entry as ListLedgerEntries returns it: no raw record. Amount is a plain decimal.
type LedgerEntry struct {
	Seq                        int64
	ConnectionID               uuid.UUID
	Type, Leg, Direction       string
	Asset, NativeAsset, Amount string
	GroupID, ExternalID        string
	OccurredAt                 time.Time
}

// Ledger returns up to limit entries of the tenant above afterSeq in ascending seq, and whether more follow
// under the same filters. One query; an empty page with a connection filter also checks that the connection
// exists in the tenant.
func (c *Connections) Ledger(ctx context.Context, tenantID uuid.UUID, afterSeq int64, f LedgerFilter, limit int32) ([]LedgerEntry, bool, error) {
	q := repository.New(c.db)
	params := repository.ListLedgerPageParams{
		TenantID: tenantID, AfterSeq: afterSeq, ConnectionID: f.ConnectionID, Types: f.Types,
		OccurredFrom: f.From, OccurredTo: f.To, PageLimit: limit + 1,
	}
	if f.OwnerRef != "" {
		params.OwnerRef = &f.OwnerRef
	}
	rows, err := q.ListLedgerPage(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("read the ledger: %w", err)
	}
	if len(rows) == 0 && f.ConnectionID != nil {
		exists, err := q.ConnectionExists(ctx, repository.ConnectionExistsParams{ID: *f.ConnectionID, TenantID: tenantID})
		if err != nil {
			return nil, false, fmt.Errorf("read the connection: %w", err)
		}
		if !exists {
			return nil, false, ErrConnectionNotFound
		}
	}
	more := len(rows) > int(limit)
	if more {
		rows = rows[:limit]
	}
	out := make([]LedgerEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, LedgerEntry(r))
	}
	return out, more, nil
}

// TriggerSync makes every stream of a connection of the tenant due now (FR-120). In one transaction: the
// connection row is locked, then its cursors are updated. CREDENTIALS_INVALID is refused before the cooldown;
// inside the cooldown after the last accepted call the call is refused. No audit row; failure counters stay.
func (c *Connections) TriggerSync(ctx context.Context, tenantID, id uuid.UUID) error {
	now := c.now().UTC().Truncate(time.Microsecond)
	return pgx.BeginFunc(ctx, c.db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		conn, err := q.LockConnectionForTrigger(ctx, repository.LockConnectionForTriggerParams{ID: id, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConnectionNotFound
		}
		if err != nil {
			return fmt.Errorf("lock the connection: %w", err)
		}
		if conn.Status == "CREDENTIALS_INVALID" {
			return ErrCredentialsInvalid
		}
		if conn.LastManualSyncAt != nil && now.Before(conn.LastManualSyncAt.Add(c.triggerCooldown)) {
			return ErrCooldown
		}
		if err := q.SetLastManualSync(ctx, repository.SetLastManualSyncParams{ID: id, At: &now}); err != nil {
			return fmt.Errorf("store the manual sync: %w", err)
		}
		if err := q.MakeStreamsDue(ctx, repository.MakeStreamsDueParams{ConnectionID: id, At: now}); err != nil {
			return fmt.Errorf("make the streams due: %w", err)
		}
		return nil
	})
}
