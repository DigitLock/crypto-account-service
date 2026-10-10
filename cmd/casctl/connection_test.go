package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/registry"
)

// inspectAccount and the values below are what the output of inspect must never show.
const (
	inspectAccount     = "100000001"
	inspectFingerprint = "q7Zx"
	inspectAsset       = "TSTX"
	inspectAmount      = "987654.321"
)

// binanceConnection inserts, as the owner role, a binance connection of tenant with a ciphertext, a cursor, a snapshot
// with one balance and its CONNECTION_CREATED audit row with ip_restricted false.
func binanceConnection(t *testing.T, pool *pgxpool.Pool, tenant string) (id, tenantID, credentialID uuid.UUID) {
	t.Helper()
	mustCasctl(t, "tenant", "create", tenant)
	issue(t, tenant)
	if err := pool.QueryRow(ctx, `SELECT t.id, c.id FROM tenants t JOIN api_credentials c ON c.tenant_id = t.id
		WHERE t.name = $1`, tenant).Scan(&tenantID, &credentialID); err != nil {
		t.Fatal(err)
	}
	id = uuid.New()
	for _, stmt := range []string{
		`INSERT INTO connections (id, tenant_id, owner_ref, source_id, external_account, status, credentials_enc, kek_version,
			key_fingerprint, permissions, permissions_checked_at)
			SELECT $1, $2, 'owner-x1', s.id, '` + inspectAccount + `', 'ACTIVE', '\x0102', 1, '` + inspectFingerprint + `',
			'{READ}', now() FROM sources s WHERE s.code = 'binance'`,
		`INSERT INTO sync_cursors (connection_id, stream, mode, next_run_at) VALUES ($1, 'balances', 'INCREMENTAL', now())`,
		`WITH s AS (INSERT INTO balance_snapshots (connection_id, taken_at) VALUES ($1, now()) RETURNING id)
			INSERT INTO snapshot_balances (snapshot_id, account_type, native_asset, asset, free, locked)
			SELECT id, 'SPOT', '` + inspectAsset + `', '` + inspectAsset + `', ` + inspectAmount + `, 0 FROM s`,
		`INSERT INTO audit_log (tenant_id, credential_id, action, object_id, details) VALUES ($2, $3, 'CONNECTION_CREATED',
			$1::text, '{"source": "binance", "owner_ref": "owner-x1", "permissions": ["READ"], "ip_restricted": false}')`,
	} {
		args := []any{id, tenantID, credentialID}
		switch {
		case strings.Contains(stmt, "INSERT INTO connections"):
			args = args[:2]
		case strings.Contains(stmt, "audit_log"):
		default:
			args = args[:1]
		}
		if _, err := pool.Exec(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return id, tenantID, credentialID
}

func assertInspectShowsNothingSecret(t *testing.T, out string) {
	t.Helper()
	for _, s := range []string{inspectAccount, inspectFingerprint, inspectAsset, inspectAmount, "987654"} {
		if strings.Contains(out, s) {
			t.Errorf("inspect shows %q:\n%s", s, out)
		}
	}
}

// X1-T602, X1-T608 — Req: UC-201, EC-213; Core UC-104, FR-118; X1 D-2, X1 D-45, X1 P-7. casctl connection inspect on
// the test database: an active connection; the same ID after DeleteConnection; an audit row with a uid member reported
// as yes. Never the account identity, a uid, an asset, an amount or the fingerprint.
func TestT602_ConnectionInspect(t *testing.T) {
	pool := setup(t)
	id, tenantID, credentialID := binanceConnection(t, pool, "x1-real")

	active := mustCasctl(t, "connection", "inspect", id.String()).stdout
	want := "connection " + id.String() + ": found\nstatus: ACTIVE\npermissions: READ\nip_restricted: false\nciphertext: yes\n" +
		"snapshots: 1\nbalance rows: 1\ncursors: 1\nledger entries: 0\naudit rows: CONNECTION_CREATED 1\nuid in audit details: no\n"
	if active != want {
		t.Errorf("inspect of the active connection:\n%s\nwant:\n%s", active, want)
	}
	assertInspectShowsNothingSecret(t, active)

	conns := registry.NewConnections(pool, nil, connector.NewSet(), limiter.NewSet(limiter.SystemClock{}, nil), time.Now, time.Second, time.Minute)
	if err := conns.Delete(ctx, tenantID, credentialID, id); err != nil {
		t.Fatal(err)
	}
	deleted := mustCasctl(t, "connection", "inspect", id.String()).stdout
	want = "connection " + id.String() + ": not found\nsnapshots: 0\nbalance rows: 0\ncursors: 0\nledger entries: 0\n" +
		"audit rows: CONNECTION_CREATED 1, CONNECTION_DELETED 1\nuid in audit details: no\n"
	if deleted != want {
		t.Errorf("inspect after DeleteConnection:\n%s\nwant:\n%s", deleted, want)
	}
	assertInspectShowsNothingSecret(t, deleted)

	if _, err := pool.Exec(ctx, `INSERT INTO audit_log (tenant_id, action, object_id, details)
		VALUES ($1, 'CONNECTION_DEGRADED', $2, '{"stream": "balances", "nested": {"uid": 354937868}}')`, tenantID, id.String()); err != nil {
		t.Fatal(err)
	}
	withUID := mustCasctl(t, "connection", "inspect", id.String()).stdout
	if !strings.Contains(withUID, "uid in audit details: yes\n") || strings.Contains(withUID, "354937868") {
		t.Errorf("inspect with a uid in the audit details:\n%s", withUID)
	}

	if r := casctl(t, "connection", "inspect", "not-a-uuid"); r.code == 0 || !strings.Contains(r.stderr, "not a UUID") {
		t.Errorf("a malformed ID: exit %d, %q", r.code, r.stderr)
	}
	unknown := mustCasctl(t, "connection", "inspect", uuid.NewString()).stdout
	if !strings.Contains(unknown, ": not found\n") || !strings.Contains(unknown, "audit rows: none\n") {
		t.Errorf("inspect of an unknown ID:\n%s", unknown)
	}
}
