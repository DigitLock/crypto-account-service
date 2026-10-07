// Phase 3 of docs/test-plan-s2.md: the card tables and the rights of both roles on them. The names carry the
// S2 row and differ from the C1 rows of schema_test.go.
package migrations_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/testdb"
	"github.com/DigitLock/crypto-account-service/migrations"
)

var cardTables = []string{"authorization_events", "authorizations", "cards", "operator_accounts", "operator_txs", "returns"}

// c1Version is the last migration of C1 and of S2 st2: the schema before the card tables.
const c1Version = 4

// cardFixture holds one row of each card table, inserted as the owner role.
type cardFixture struct {
	fixture
	cardID, authorizationID, returnID string
}

// bytesOf returns n bytes of value b: chain IDs, hashes and addresses of the fixtures.
func bytesOf(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func newCardFixture(t *testing.T, owner *pgxpool.Pool, tenant string) cardFixture {
	t.Helper()
	f := cardFixture{fixture: newTenant(t, owner, tenant)}
	// The byte values differ per tenant: the chain IDs and the operator are unique across tenants.
	seed := tenant[len(tenant)-1]
	mustQueryRow(t, owner, &f.connectionID, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account)
		VALUES ($1, 'owner-1', $2, $3) RETURNING id`, f.tenantID, f.sourceID, "0xwallet-"+tenant)
	mustQueryRow(t, owner, &f.cardID, `INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, daily_limit)
		VALUES ($1, 'card-1', 'owner-1', $2, 200000000) RETURNING id`, f.tenantID, f.connectionID)
	mustQueryRow(t, owner, &f.authorizationID, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, card_id,
		fiat_amount, fiat_currency, rate, buffer_bps, token, token_amount, wallet_address, chain_id, status, received_at)
		VALUES ($1, 'auth-1', $2, $3, 25.40, 'EUR', 1.1642, 100, 'USDC', 29866387, $4, 31337, 'RECEIVED', now())
		RETURNING id`, f.tenantID, bytesOf(seed, 32), f.cardID, bytesOf(2, 20))
	mustExec(t, owner, `INSERT INTO authorization_events (authorization_id, to_status) VALUES ($1, 'RECEIVED')`,
		f.authorizationID)
	mustQueryRow(t, owner, &f.returnID, `INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id,
		type, token_amount, status) VALUES ($1, $2, 'rv-1', $3, 'REVERSAL', 100, 'ACCEPTED') RETURNING id`,
		f.tenantID, f.authorizationID, bytesOf(seed+1, 32))
	mustExec(t, owner, `INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose, authorization_id, status)
		VALUES (31337, $1, 0, 'DEBIT', $2, 'PLANNED')`, bytesOf(seed, 20), f.authorizationID)
	mustExec(t, owner, `INSERT INTO operator_accounts (chain_id, address, next_nonce) VALUES (31337, $1, 1)`, bytesOf(seed, 20))
	return f
}

// grantsOf returns the table and column privileges of a role in the schema public.
func grantsOf(t *testing.T, pool *pgxpool.Pool, role string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT format('table %s %s', table_name, privilege_type) FROM information_schema.role_table_grants
		WHERE table_schema = 'public' AND grantee = $1
		UNION ALL
		SELECT format('column %s.%s %s', table_name, column_name, privilege_type) FROM information_schema.column_privileges
		WHERE table_schema = 'public' AND grantee = $1
			AND (table_name, privilege_type) NOT IN (
				SELECT table_name, privilege_type FROM information_schema.role_table_grants
				WHERE table_schema = 'public' AND grantee = $1)
		ORDER BY 1`, role)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// S2-T301 — Req: SRS — Card Spend §2.4, SRS — Core §2.4. Up from empty, down to the version of C1, up again.
func TestT301_CardTablesUpDownUp(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	m := testdb.Migrator(t)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down: %v", err)
	}
	if err := m.Migrate(c1Version); err != nil {
		t.Fatalf("migrate up to %d: %v", c1Version, err)
	}
	beforeSchema, beforeTables := schema(t, owner), tables(t, owner)
	beforeCardAuth := grantsOf(t, owner, "cas_card_auth")

	if err := m.Up(); err != nil {
		t.Fatalf("migrate up from empty: %v", err)
	}
	assertVersion(t, m, migrations.Latest())
	want := slices.Sorted(slices.Values(append(slices.Clone(coreTables), cardTables...)))
	if got := tables(t, owner); !slices.Equal(got, want) {
		t.Errorf("tables = %v, want %v", got, want)
	}

	if err := m.Migrate(c1Version); err != nil {
		t.Fatalf("migrate down to %d: %v", c1Version, err)
	}
	if got := tables(t, owner); !slices.Equal(got, beforeTables) || !slices.Equal(got, coreTables) {
		t.Errorf("tables after down = %v, want exactly the ten of C1 %v", got, coreTables)
	}
	if got := schema(t, owner); !slices.Equal(got, beforeSchema) {
		t.Errorf("the schema of C1 changed after up and down:\nbefore %v\nafter  %v", beforeSchema, got)
	}
	if got := grantsOf(t, owner, "cas_card_auth"); !slices.Equal(got, beforeCardAuth) ||
		!slices.Equal(got, []string{"table schema_migrations SELECT"}) {
		t.Errorf("cas_card_auth after down = %v, want only SELECT on schema_migrations", got)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	assertVersion(t, m, migrations.Latest())
	if got := tables(t, owner); !slices.Equal(got, want) {
		t.Errorf("tables after the second up = %v, want %v", got, want)
	}
	// Every card table is usable after the second up.
	newCardFixture(t, owner, "tenant-a")
}

// S2-T302 — Req: SRS — Card Spend §2.4
func TestT302_CardUniqueness(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	newCardFixture(t, owner, "tenant-a")

	for name, stmt := range map[string]string{
		"cards (tenant_id, card_ref)": `INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, daily_limit)
			SELECT tenant_id, card_ref, owner_ref, connection_id, 1 FROM cards`,
		"authorizations (tenant_id, auth_id)": `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, status, received_at)
			SELECT tenant_id, auth_id, '\x` + string(bytes.Repeat([]byte("09"), 32)) + `', 'RECEIVED', now() FROM authorizations`,
		"authorizations.chain_auth_id": `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, status, received_at)
			SELECT tenant_id, 'auth-2', chain_auth_id, 'RECEIVED', now() FROM authorizations`,
		"returns (tenant_id, return_id)": `INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id, type,
			token_amount, status)
			SELECT tenant_id, authorization_id, return_id, '\x` + string(bytes.Repeat([]byte("0a"), 32)) + `', 'REFUND', 1,
			'ACCEPTED' FROM returns`,
		"returns.chain_refund_id": `INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id, type,
			token_amount, status)
			SELECT tenant_id, authorization_id, 'rv-2', chain_refund_id, 'REFUND', 1, 'ACCEPTED' FROM returns`,
		"operator_txs (chain_id, operator_address, nonce)": `INSERT INTO operator_txs (chain_id, operator_address, nonce,
			purpose, status) SELECT chain_id, operator_address, nonce, 'REFUND', 'PLANNED' FROM operator_txs`,
		"operator_accounts (chain_id, address)": `INSERT INTO operator_accounts (chain_id, address, next_nonce)
			SELECT chain_id, address, 5 FROM operator_accounts`,
	} {
		t.Run(name, func(t *testing.T) { expectCode(t, owner, codeUniqueViolation, stmt) })
	}

	t.Run("another tenant may use the same card_ref, auth_id and return_id", func(t *testing.T) {
		newCardFixture(t, owner, "tenant-b")
		var n int
		mustQueryRow(t, owner, &n, `SELECT count(DISTINCT tenant_id) FROM authorizations WHERE auth_id = 'auth-1'`)
		if n != 2 {
			t.Errorf("auth-1 in %d tenants, want 2", n)
		}
	})
}

// S2-T303 — Req: SRS — Card Spend §2.4, handoff §4
func TestT303_CardValueConstraints(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	newCardFixture(t, owner, "tenant-a")

	checks := map[string]string{
		"card status":             `UPDATE cards SET status = 'CLOSED'`,
		"negative daily_limit":    `UPDATE cards SET daily_limit = -1`,
		"authorization status":    `UPDATE authorizations SET status = 'PENDING'`,
		"decline_reason":          `UPDATE authorizations SET decline_reason = 'NO_REASON'`,
		"negative token_amount":   `UPDATE authorizations SET token_amount = -1`,
		"negative debited":        `UPDATE authorizations SET debited_amount = -1`,
		"negative returned":       `UPDATE authorizations SET returned_amount = -1`,
		"zero fiat_amount":        `UPDATE authorizations SET fiat_amount = 0`,
		"zero rate":               `UPDATE authorizations SET rate = 0`,
		"negative buffer_bps":     `UPDATE authorizations SET buffer_bps = -1`,
		"chain_auth_id 31 bytes":  `UPDATE authorizations SET chain_auth_id = substring(chain_auth_id FROM 2)`,
		"wallet_address 19 bytes": `UPDATE authorizations SET wallet_address = substring(wallet_address FROM 2)`,
		"event to_status":         `UPDATE authorization_events SET to_status = 'PENDING'`,
		"event from_status":       `UPDATE authorization_events SET from_status = 'PENDING'`,
		"return type":             `UPDATE returns SET type = 'CHARGEBACK'`,
		"return status":           `UPDATE returns SET status = 'DONE'`,
		"negative return amount":  `UPDATE returns SET token_amount = -1`,
		"negative attempts":       `UPDATE returns SET attempts = -1`,
		"chain_refund_id 31":      `UPDATE returns SET chain_refund_id = substring(chain_refund_id FROM 2)`,
		"purpose":                 `UPDATE operator_txs SET purpose = 'TRANSFER'`,
		"operator tx status":      `UPDATE operator_txs SET status = 'DROPPED'`,
		"operator_address 19":     `UPDATE operator_txs SET operator_address = substring(operator_address FROM 2)`,
		"tx_hash 31 bytes":        `UPDATE operator_txs SET tx_hash = '\x` + string(bytes.Repeat([]byte("01"), 31)) + `'`,
		"block_hash 33 bytes":     `UPDATE operator_txs SET block_hash = '\x` + string(bytes.Repeat([]byte("01"), 33)) + `'`,
		"negative nonce":          `UPDATE operator_txs SET nonce = -1`,
		"negative next_nonce":     `UPDATE operator_accounts SET next_nonce = -1`,
		"account address 21":      `UPDATE operator_accounts SET address = address || '\x01'::bytea`,
	}
	for name, stmt := range checks {
		t.Run(name, func(t *testing.T) { expectCode(t, owner, codeCheckViolation, stmt) })
	}

	t.Run("nullable values may be absent", func(t *testing.T) {
		mustExec(t, owner, `UPDATE authorizations SET fiat_amount = NULL, rate = NULL, wallet_address = NULL,
			decline_reason = NULL, token_amount = NULL, buffer_bps = NULL`)
		mustExec(t, owner, `UPDATE operator_txs SET tx_hash = NULL, block_hash = NULL`)
	})

	t.Run("replaced_hashes defaults to empty", func(t *testing.T) {
		var n int
		mustQueryRow(t, owner, &n, `SELECT cardinality(replaced_hashes) FROM operator_txs`)
		if n != 0 {
			t.Errorf("replaced_hashes has %d items, want none", n)
		}
	})

	t.Run("a connection with a card is not deleted", func(t *testing.T) {
		expectCode(t, owner, "23503", `DELETE FROM connections`)
	})
}

// S2-T304 — Req: SRS — Core §3.2, ADR-3
func TestT304_RightsOfCasCardAuthRefused(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	cardAuth := testdb.OpenCardAuth(t)
	newCardFixture(t, owner, "tenant-a")

	refused := []string{
		// DDL.
		`CREATE TABLE t304 (x INT)`,
		`ALTER TABLE authorizations ADD COLUMN x INT`,
		`DROP TABLE operator_txs`,
		`TRUNCATE authorization_events`,
		// No exchange secret.
		`SELECT credentials_enc FROM connections`,
		`SELECT * FROM connections`,
		// Append-only history.
		`UPDATE authorization_events SET reason = 'x'`,
		`DELETE FROM authorization_events`,
		// The registry and the credentials are read-only.
		`INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, daily_limit)
			SELECT tenant_id, 'card-x', owner_ref, connection_id, 1 FROM cards`,
		`UPDATE cards SET status = 'FROZEN'`,
		`DELETE FROM cards`,
		`INSERT INTO tenants (name) VALUES ('tenant-x')`,
		`UPDATE tenants SET status = 'DISABLED'`,
		`DELETE FROM tenants`,
		`INSERT INTO api_credentials (tenant_id, kind, key_id, secret_hash)
			SELECT id, 'PROCESSOR_BASIC', 'x-key', '\x00' FROM tenants LIMIT 1`,
		`UPDATE api_credentials SET revoked_at = now()`,
		`DELETE FROM api_credentials`,
		// No DELETE on a card table.
		`DELETE FROM authorizations`,
		`DELETE FROM returns`,
		`DELETE FROM operator_txs`,
		`DELETE FROM operator_accounts`,
		// Nothing of the sync data.
		`SELECT * FROM ledger_entries`,
		`SELECT * FROM audit_log`,
		`UPDATE connections SET label = 'x'`,
	}
	for _, stmt := range refused {
		expectCode(t, cardAuth, codeInsufficientPrivilege, stmt)
	}
}

// S2-T305 — Req: SRS — Core §3.2
func TestT305_RightsOfCasCardAuthAllowed(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	cardAuth := testdb.OpenCardAuth(t)
	f := newCardFixture(t, owner, "tenant-a")

	for _, stmt := range []string{
		`SELECT * FROM cards`,
		`SELECT id, tenant_id, owner_ref, source_id, external_account, label, status, kek_version, key_fingerprint,
			permissions, permissions_checked_at, last_manual_sync_at, created_at FROM connections`,
		`SELECT * FROM tenants`,
		`SELECT * FROM api_credentials`,
		`SELECT * FROM sources`,
		`SELECT * FROM schema_migrations`,
		`SELECT * FROM authorizations`,
		`SELECT * FROM authorization_events`,
		`SELECT * FROM returns`,
		`SELECT * FROM operator_txs`,
		`SELECT * FROM operator_accounts`,
	} {
		mustExec(t, cardAuth, stmt)
	}

	var authID, returnID string
	mustQueryRow(t, cardAuth, &authID, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, status, received_at)
		VALUES ($1, 'auth-2', $2, 'RECEIVED', now()) RETURNING id`, f.tenantID, bytesOf(5, 32))
	mustExec(t, cardAuth, `UPDATE authorizations SET status = 'DECLINED', decline_reason = 'TIMEOUT' WHERE id = $1`, authID)
	mustExec(t, cardAuth, `INSERT INTO authorization_events (authorization_id, from_status, to_status, reason)
		VALUES ($1, 'RECEIVED', 'DECLINED', 'TIMEOUT')`, authID)
	mustQueryRow(t, cardAuth, &returnID, `INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id,
		type, token_amount, status) VALUES ($1, $2, 'rv-2', $3, 'REFUND', 0, 'NOTHING_TO_RETURN') RETURNING id`,
		f.tenantID, authID, bytesOf(6, 32))
	mustExec(t, cardAuth, `UPDATE returns SET attempts = 1 WHERE id = $1`, returnID)
	mustExec(t, cardAuth, `INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose, return_row_id, status)
		VALUES (31337, $1, 1, 'REFUND', $2, 'PLANNED')`, bytesOf('a', 20), returnID)
	mustExec(t, cardAuth, `UPDATE operator_txs SET status = 'SENT', tx_hash = $1,
		replaced_hashes = replaced_hashes || $1::bytea WHERE nonce = 1`, bytesOf(7, 32))
	mustExec(t, cardAuth, `INSERT INTO operator_accounts (chain_id, address, next_nonce) VALUES (84532, $1, 0)`, bytesOf('a', 20))
	mustExec(t, cardAuth, `UPDATE operator_accounts SET next_nonce = next_nonce + 1`)
	// No RETURNING: cas_card_auth has no SELECT on audit_log.
	mustExec(t, cardAuth, `INSERT INTO audit_log (tenant_id, action, object_id) VALUES ($1, 'CREDENTIAL_ISSUED', 'x')`,
		f.tenantID)
}

// S2-T306 — Req: SRS — Core §3.2, owner's decision D-11
func TestT306_RightsOfCasServerOnCardTables(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	server := testdb.OpenServer(t)
	f := newCardFixture(t, owner, "tenant-a")

	t.Run("reads and the card registry", func(t *testing.T) {
		for _, stmt := range []string{
			`SELECT * FROM authorizations`, `SELECT * FROM authorization_events`, `SELECT * FROM returns`,
			`SELECT * FROM cards`,
			// D-11: the transaction hashes of the reads.
			`SELECT authorization_id, return_row_id, purpose, status, tx_hash, created_at FROM operator_txs`,
		} {
			mustExec(t, server, stmt)
		}
		mustExec(t, server, `INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, daily_limit)
			VALUES ($1, 'card-2', 'owner-1', $2, 5)`, f.tenantID, f.connectionID)
		mustExec(t, server, `UPDATE cards SET status = 'FROZEN', updated_at = now() WHERE card_ref = 'card-2'`)
	})

	t.Run("writes of card-auth refused", func(t *testing.T) {
		for _, stmt := range []string{
			`INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, status, received_at)
				SELECT tenant_id, 'auth-x', '\x` + string(bytes.Repeat([]byte("0b"), 32)) + `', 'RECEIVED', now() FROM cards LIMIT 1`,
			`UPDATE authorizations SET status = 'DECLINED'`,
			`DELETE FROM authorizations`,
			`INSERT INTO authorization_events (authorization_id, to_status) SELECT id, 'RECEIVED' FROM authorizations`,
			`UPDATE authorization_events SET reason = 'x'`,
			`DELETE FROM authorization_events`,
			`INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id, type, token_amount, status)
				SELECT tenant_id, id, 'rv-x', chain_auth_id, 'REFUND', 1, 'ACCEPTED' FROM authorizations`,
			`UPDATE returns SET status = 'CONFIRMED'`,
			`DELETE FROM returns`,
			`DELETE FROM cards`,
			`SELECT * FROM operator_txs`,
			`SELECT nonce FROM operator_txs`,
			`SELECT operator_address FROM operator_txs`,
			`SELECT replaced_hashes FROM operator_txs`,
			`UPDATE operator_txs SET status = 'CONFIRMED'`,
			`DELETE FROM operator_txs`,
			`INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose, status)
				VALUES (31337, '\x00', 9, 'DEBIT', 'PLANNED')`,
			`SELECT * FROM operator_accounts`,
			`UPDATE operator_accounts SET next_nonce = 0`,
		} {
			expectCode(t, server, codeInsufficientPrivilege, stmt)
		}
	})
}
