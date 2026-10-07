// Phase 3 of docs/test-plan-c1.md: schema, constraints, rights of cas_server, seed.
// T308 is in internal/health.
package migrations_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
	"github.com/DigitLock/crypto-account-service/migrations"
)

var coreTables = []string{
	"api_credentials", "asset_aliases", "audit_log", "balance_snapshots", "connections", "ledger_entries",
	"snapshot_balances", "sources", "sync_cursors", "tenants",
}

const (
	codeUniqueViolation       = "23505"
	codeCheckViolation        = "23514"
	codeInsufficientPrivilege = "42501"
)

var ctx = context.Background()

// tables returns the tables of the schema public, schema_migrations excluded.
func tables(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// schema describes columns, constraints, indexes and grants of the schema public.
func schema(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT format('column %s.%s %s %s null=%s default=%s identity=%s', table_name, column_name, ordinal_position,
			udt_name, is_nullable, coalesce(column_default, '-'), coalesce(identity_generation, '-'))
		FROM information_schema.columns WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		UNION ALL
		SELECT format('constraint %s.%s %s', c.conrelid::regclass, c.conname, pg_get_constraintdef(c.oid))
		FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public'
			AND c.conrelid <> 'schema_migrations'::regclass
		UNION ALL
		SELECT format('index %s', indexdef) FROM pg_indexes
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		UNION ALL
		SELECT format('grant %s %s %s', table_name, grantee, privilege_type) FROM information_schema.role_table_grants
		WHERE table_schema = 'public' AND grantee = 'cas_server'
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func assertVersion(t *testing.T, m *migrate.Migrate, want uint) {
	t.Helper()
	v, dirty, err := m.Version()
	if err != nil || v != want || dirty {
		t.Fatalf("schema version = %d dirty=%v err=%v, want %d clean", v, dirty, err, want)
	}
}

// pgCode returns the SQLSTATE of err, or "" when err is nil or not a PostgreSQL error.
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// expectCode runs stmt and fails unless PostgreSQL refuses it with code.
func expectCode(t *testing.T, pool *pgxpool.Pool, code, stmt string, args ...any) {
	t.Helper()
	_, err := pool.Exec(ctx, stmt, args...)
	if got := pgCode(err); got != code {
		t.Errorf("%s\n  got error %v (SQLSTATE %q), want SQLSTATE %s", stmt, err, got, code)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, stmt string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func mustQueryRow(t *testing.T, pool *pgxpool.Pool, dest any, stmt string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(ctx, stmt, args...).Scan(dest); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// fixture holds the rows the tests build on.
type fixture struct {
	tenantID, credentialID, connectionID, snapshotID string
	sourceID                                         int16
}

// newTenant inserts a tenant and a credential as the owner role, which cas_server cannot do.
func newTenant(t *testing.T, owner *pgxpool.Pool, name string) fixture {
	t.Helper()
	var f fixture
	mustQueryRow(t, owner, &f.tenantID, `INSERT INTO tenants (name) VALUES ($1) RETURNING id`, name)
	mustQueryRow(t, owner, &f.credentialID, `INSERT INTO api_credentials (tenant_id, kind, key_id, secret_hash)
		VALUES ($1, 'SERVICE_TOKEN', $2, '\x00') RETURNING id`, f.tenantID, name+"-key")
	mustQueryRow(t, owner, &f.sourceID, `SELECT id FROM sources WHERE code = 'anvil'`)
	return f
}

// addConnectionData inserts a connection with a cursor, a snapshot with a balance and a ledger entry.
func addConnectionData(t *testing.T, pool *pgxpool.Pool, f *fixture, account string) {
	t.Helper()
	mustQueryRow(t, pool, &f.connectionID, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account)
		VALUES ($1, 'owner-1', $2, $3) RETURNING id`, f.tenantID, f.sourceID, account)
	mustExec(t, pool, `INSERT INTO sync_cursors (connection_id, stream, mode, next_run_at)
		VALUES ($1, 'logs', 'BACKFILL', now())`, f.connectionID)
	mustQueryRow(t, pool, &f.snapshotID, `INSERT INTO balance_snapshots (connection_id, taken_at)
		VALUES ($1, now()) RETURNING id`, f.connectionID)
	mustExec(t, pool, `INSERT INTO snapshot_balances (snapshot_id, account_type, native_asset, asset, free, locked)
		VALUES ($1, 'WALLET', '0xtoken', 'USDC', 70.133613, 0)`, f.snapshotID)
	mustExec(t, pool, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id, type,
		direction, asset, native_asset, amount, occurred_at, raw)
		VALUES ($1, $2, 'logs', 'tx:1', 'SINGLE', 'logs:tx:1', 'CARD_DEBIT', 'OUT', 'USDC', '0xtoken', 29.866387,
		now(), '{}')`, f.tenantID, f.connectionID)
}

// C1-T301 — Req: §2.4
func TestT301_ApplyOnCleanDatabase(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	m := testdb.Migrator(t)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down: %v", err)
	}
	if got := tables(t, owner); len(got) != 0 {
		t.Fatalf("tables after down to zero: %v, want none", got)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	assertVersion(t, m, migrations.Latest())
	// The tables of C1. The card tables of S2 are checked by S2-T301.
	got := tables(t, owner)
	for _, table := range coreTables {
		if !slices.Contains(got, table) {
			t.Errorf("tables = %v, want %v among them", got, coreTables)
			break
		}
	}
}

// C1-T302 — Req: package st4
func TestT302_RollBackAndApplyAgain(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	m := testdb.Migrator(t)
	first := schema(t, owner)

	if err := m.Down(); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if got := tables(t, owner); len(got) != 0 {
		t.Fatalf("tables after down to zero: %v, want none", got)
	}
	var grants int
	mustQueryRow(t, owner, &grants, `SELECT count(*) FROM information_schema.role_table_grants
		WHERE table_schema = 'public' AND grantee = 'cas_server'`)
	if grants != 0 {
		t.Errorf("cas_server keeps %d grants after down to zero", grants)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	assertVersion(t, m, migrations.Latest())
	if second := schema(t, owner); !slices.Equal(first, second) {
		t.Errorf("schema after the second up differs:\nfirst:  %v\nsecond: %v", first, second)
	}
}

// C1-T303 — Req: §2.4, FR-104, FR-106
func TestT303_Uniqueness(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	f := newTenant(t, owner, "tenant-a")
	addConnectionData(t, owner, &f, "0xaccount")

	expectCode(t, owner, codeUniqueViolation, `INSERT INTO tenants (name) VALUES ('tenant-a')`)
	expectCode(t, owner, codeUniqueViolation, `INSERT INTO api_credentials (tenant_id, kind, key_id, secret_hash)
		VALUES ($1, 'SERVICE_TOKEN', 'tenant-a-key', '\x01')`, f.tenantID)
	expectCode(t, owner, codeUniqueViolation, `INSERT INTO sources (code, kind) VALUES ('anvil', 'EVM')`)
	expectCode(t, owner, codeUniqueViolation, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account)
		VALUES ($1, 'owner-2', $2, '0xaccount')`, f.tenantID, f.sourceID)
	expectCode(t, owner, codeUniqueViolation, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id,
		leg, group_id, type, direction, asset, native_asset, amount, occurred_at, raw)
		VALUES ($1, $2, 'logs', 'tx:1', 'SINGLE', 'other', 'DEPOSIT', 'IN', 'USDC', '0xtoken', 1, now(), '{}')`,
		f.tenantID, f.connectionID)

	// The same account in another tenant, and another leg of the same record, are accepted.
	g := newTenant(t, owner, "tenant-b")
	mustExec(t, owner, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account)
		VALUES ($1, 'owner-1', $2, '0xaccount')`, g.tenantID, g.sourceID)
	mustExec(t, owner, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id, type,
		direction, asset, native_asset, amount, occurred_at, raw)
		VALUES ($1, $2, 'logs', 'tx:1', 'FEE', 'logs:tx:1', 'FEE', 'OUT', 'ETH', 'ETH', 0.0001, now(), '{}')`,
		f.tenantID, f.connectionID)
}

// C1-T304 — Req: §2.4, handoff §4
func TestT304_ValueConstraints(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	f := newTenant(t, owner, "tenant-a")
	addConnectionData(t, owner, &f, "0xaccount")

	n := 0
	entry := func(column, value string) string {
		n++
		values := map[string]string{
			"leg": "'SINGLE'", "type": "'DEPOSIT'", "direction": "'IN'", "amount": "1",
		}
		values[column] = value
		return fmt.Sprintf(`INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id,
			type, direction, asset, native_asset, amount, occurred_at, raw)
			VALUES ($1, $2, 'logs', 'tx:%d', %s, 'g', %s, %s, 'USDC', '0xtoken', %s, now(), '{}')`,
			n, values["leg"], values["type"], values["direction"], values["amount"])
	}
	for _, c := range []struct{ column, value string }{
		{"amount", "0"}, {"amount", "-1"},
		{"leg", "'OTHER'"}, {"type", "'AIRDROP'"}, {"direction", "'SIDEWAYS'"},
	} {
		expectCode(t, owner, codeCheckViolation, entry(c.column, c.value), f.tenantID, f.connectionID)
	}

	balance := `INSERT INTO snapshot_balances (snapshot_id, account_type, native_asset, asset, free, locked)
		VALUES ($1, $2, $3, 'USDC', $4, $5)`
	expectCode(t, owner, codeCheckViolation, balance, f.snapshotID, "SPOT", "a", "-1", "0")
	expectCode(t, owner, codeCheckViolation, balance, f.snapshotID, "SPOT", "b", "0", "-0.000000000000000001")
	expectCode(t, owner, codeCheckViolation, balance, f.snapshotID, "MARGIN", "c", "0", "0")

	for _, stmt := range []string{
		`INSERT INTO tenants (name, status) VALUES ('tenant-x', 'SUSPENDED')`,
		`INSERT INTO sources (code, kind) VALUES ('x-source', 'BANK')`,
		`UPDATE connections SET status = 'PAUSED'`,
		`UPDATE connections SET credentials_enc = '\x01'`,
		`UPDATE connections SET kek_version = 1`,
		`UPDATE sync_cursors SET mode = 'REPLAY'`,
		`UPDATE sync_cursors SET consecutive_failures = -1`,
	} {
		expectCode(t, owner, codeCheckViolation, stmt)
	}
	expectCode(t, owner, codeCheckViolation, `INSERT INTO api_credentials (tenant_id, kind, key_id, secret_hash)
		VALUES ($1, 'PASSWORD', 'x-key', '\x00')`, f.tenantID)
	expectCode(t, owner, codeCheckViolation, `INSERT INTO audit_log (tenant_id, action, object_id)
		VALUES ($1, 'CONNECTION_RENAMED', 'x')`, f.tenantID)

	// Both credential columns set together are accepted.
	mustExec(t, owner, `UPDATE connections SET credentials_enc = '\x01', kek_version = 1`)
}

// C1-T305 — Req: §3.2, FR-111
func TestT305_RightsOfCasServer(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	server := testdb.OpenServer(t)
	f := newTenant(t, owner, "tenant-a")

	t.Run("granted statements work", func(t *testing.T) {
		addConnectionData(t, server, &f, "0xaccount")
		mustExec(t, server, `UPDATE connections SET label = 'Card wallet' WHERE id = $1`, f.connectionID)
		mustExec(t, server, `UPDATE sync_cursors SET consecutive_failures = 1 WHERE connection_id = $1`, f.connectionID)
		// No RETURNING: cas_server has no SELECT on audit_log.
		mustExec(t, server, `INSERT INTO audit_log (tenant_id, credential_id, action, object_id)
			VALUES ($1, $2, 'CONNECTION_CREATED', $3)`, f.tenantID, f.credentialID, f.connectionID)
		for _, table := range []string{
			"tenants", "api_credentials", "sources", "asset_aliases", "connections", "sync_cursors",
			"balance_snapshots", "snapshot_balances", "ledger_entries", "schema_migrations",
		} {
			mustExec(t, server, "SELECT * FROM "+table+" LIMIT 1")
		}
		mustExec(t, server, `DELETE FROM connections WHERE id = $1`, f.connectionID)
	})

	t.Run("refused statements", func(t *testing.T) {
		addConnectionData(t, owner, &f, "0xaccount-2")
		refused := []string{
			// DDL.
			`CREATE TABLE t305 (x INT)`,
			`ALTER TABLE ledger_entries ADD COLUMN x INT`,
			`CREATE INDEX ON ledger_entries (asset)`,
			`DROP TABLE audit_log`,
			`TRUNCATE ledger_entries`,
		}
		for table, column := range map[string]string{
			"ledger_entries":    "asset",
			"balance_snapshots": "taken_at",
			"snapshot_balances": "asset",
		} {
			refused = append(refused,
				`UPDATE `+table+` SET `+column+` = `+column,
				`DELETE FROM `+table,
			)
		}
		refused = slices.Concat(refused, []string{
			`UPDATE audit_log SET object_id = 'x'`,
			`DELETE FROM audit_log`,
			`SELECT * FROM audit_log`,
			`INSERT INTO tenants (name) VALUES ('tenant-x')`,
			`INSERT INTO api_credentials (tenant_id, kind, key_id, secret_hash)
				SELECT id, 'SERVICE_TOKEN', 'x-key', '\x00' FROM tenants LIMIT 1`,
			`INSERT INTO sources (code, kind) VALUES ('x-source', 'EVM')`,
			`INSERT INTO asset_aliases (source_id, native_asset, asset) SELECT id, '0xtoken', 'USDC' FROM sources LIMIT 1`,
		})

		for _, stmt := range refused {
			expectCode(t, server, codeInsufficientPrivilege, stmt)
		}
	})
}

// C1-T306 — Req: §3.2, FR-118
func TestT306_CascadeUnderCasServer(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	server := testdb.OpenServer(t)
	f := newTenant(t, owner, "tenant-a")
	addConnectionData(t, server, &f, "0xaccount")
	mustExec(t, server, `INSERT INTO audit_log (tenant_id, action, object_id) VALUES ($1, 'CONNECTION_DELETED', $2)`,
		f.tenantID, f.connectionID)

	mustExec(t, server, `DELETE FROM connections WHERE id = $1`, f.connectionID)

	for table, where := range map[string]string{
		"connections":       "id = $1",
		"sync_cursors":      "connection_id = $1",
		"balance_snapshots": "connection_id = $1",
		"ledger_entries":    "connection_id = $1",
	} {
		var n int
		mustQueryRow(t, owner, &n, "SELECT count(*) FROM "+table+" WHERE "+where, f.connectionID)
		if n != 0 {
			t.Errorf("%s keeps %d rows of the deleted connection", table, n)
		}
	}
	var balances int
	mustQueryRow(t, owner, &balances, `SELECT count(*) FROM snapshot_balances WHERE snapshot_id = $1`, f.snapshotID)
	if balances != 0 {
		t.Errorf("snapshot_balances keeps %d rows of the deleted connection", balances)
	}
	var audit int
	mustQueryRow(t, owner, &audit, `SELECT count(*) FROM audit_log WHERE object_id = $1`, f.connectionID)
	if audit != 1 {
		t.Errorf("audit_log rows of the deleted connection = %d, want 1", audit)
	}
}

// C1-T307 — Req: SRS — EVM §2.4, §2.4
func TestT307_Seed(t *testing.T) {
	testdb.Open(t)
	testdb.Clean(t)
	q := repository.New(testdb.OpenServer(t))

	sources, err := q.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"anvil": 31337, "base-sepolia": 84532}
	got := map[string]float64{}
	for _, s := range sources {
		var config map[string]any
		if err := json.Unmarshal(s.Config, &config); err != nil {
			t.Fatalf("%s config: %v", s.Code, err)
		}
		if s.Kind != "EVM" || !s.Enabled || len(config) != 1 {
			t.Errorf("%s: kind %s, enabled %v, config %v; want EVM, enabled, chain_id only", s.Code, s.Kind, s.Enabled, config)
		}
		chainID, _ := config["chain_id"].(float64)
		got[s.Code] = chainID
	}
	if !maps.Equal(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}

	if _, err := q.GetSourceByCode(ctx, "anvil"); err != nil {
		t.Errorf("GetSourceByCode(anvil): %v", err)
	}
	for _, code := range []string{"fake", "binance"} {
		if _, err := q.GetSourceByCode(ctx, code); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetSourceByCode(%s) = %v, want no rows", code, err)
		}
	}
}
