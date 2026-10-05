// Package testdb gives integration tests the test database (docs/test-plan-c1.md §1).
// TEST_DATABASE_URL is the owner role: it runs the migrations and cleans tables.
// TEST_DATABASE_URL_SERVER is the role cas_server: it is used by the code under test.
// TEST_DATABASE_URL_CARD_AUTH is the role cas_card_auth: it is used by card-auth under test
// (docs/test-plan-s2.md §1).
package testdb

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the database/sql driver "pgx"

	"github.com/DigitLock/crypto-account-service/migrations"
)

// URL returns TEST_DATABASE_URL. When it is empty the test is skipped locally
// and fails in CI, where the variable CI is set.
func URL(t testing.TB) string {
	t.Helper()
	return envURL(t, "TEST_DATABASE_URL")
}

// ServerURL returns TEST_DATABASE_URL_SERVER with the same rule as URL.
func ServerURL(t testing.TB) string {
	t.Helper()
	return envURL(t, "TEST_DATABASE_URL_SERVER")
}

// CardAuthURL returns TEST_DATABASE_URL_CARD_AUTH with the same rule as URL.
func CardAuthURL(t testing.TB) string {
	t.Helper()
	return envURL(t, "TEST_DATABASE_URL_CARD_AUTH")
}

func envURL(t testing.TB, name string) string {
	t.Helper()
	u := os.Getenv(name)
	if u == "" {
		if os.Getenv("CI") != "" {
			t.Fatal(name + " is not set; it is required in CI")
		}
		t.Skip(name + " is not set")
	}
	return u
}

// Open returns a pool of the owner role on a schema at migrations.Latest().
func Open(t testing.TB) *pgxpool.Pool {
	t.Helper()
	m := Migrator(t)
	if err := m.Migrate(migrations.Latest()); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate the test database to version %d: %v", migrations.Latest(), err)
	}
	return newPool(t, URL(t))
}

// OpenServer returns a pool of the role cas_server. It does not migrate: call Open first.
func OpenServer(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return newPool(t, ServerURL(t))
}

// OpenCardAuth returns a pool of the role cas_card_auth. It does not migrate: call Open first.
func OpenCardAuth(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return newPool(t, CardAuthURL(t))
}

// Migrator returns a golang-migrate instance on TEST_DATABASE_URL with the embedded migrations.
func Migrator(t testing.TB) *migrate.Migrate {
	t.Helper()
	db, err := sql.Open("pgx", URL(t))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		_ = db.Close()
		t.Fatalf("migration driver: %v", err)
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

// Clean empties the tables of the test database and keeps the seeded sources.
func Clean(t testing.TB) {
	t.Helper()
	pool := newPool(t, URL(t))
	for _, stmt := range []string{
		`TRUNCATE tenants, api_credentials, connections, sync_cursors, balance_snapshots, snapshot_balances,
			ledger_entries, audit_log RESTART IDENTITY CASCADE`,
		`DELETE FROM asset_aliases`,
		`DELETE FROM sources WHERE code NOT IN ('anvil', 'base-sepolia')`,
		// Every setup inserts the source fake again; without this the SMALLINT identity of sources.id
		// grows with every run of the suite until it overflows.
		`SELECT setval(pg_get_serial_sequence('sources', 'id'), COALESCE(MAX(id), 1), MAX(id) IS NOT NULL) FROM sources`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("clean the test database: %v", err)
		}
	}
}

func newPool(t testing.TB, connString string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), connString)
	if err != nil {
		// The error is not printed: it may quote the connection string.
		t.Fatal("create a pool on the test database: invalid connection string")
	}
	t.Cleanup(pool.Close)
	return pool
}
