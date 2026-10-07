// Phase 1 of docs/test-plan-s3.md: the source values and the tables of S3, the rights of cas_server on them.
// The names carry the S3 row and differ from the C1 and S2 rows of the other files.
package migrations_test

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
	"github.com/DigitLock/crypto-account-service/migrations"
)

var s3Tables = []string{"balance_checkpoints", "reconciliation_runs"}

// s2Version is the last migration of S2: the schema before S3.
const s2Version = 6

// baseSepoliaMockUSDC is MockUSDC of base-sepolia (SRS — EVM Connector §2.4 Seed data; S3 D-27).
const baseSepoliaMockUSDC = "0x6c0434c821694513FFfd5364D63f27F05d73f3aB"

func sourceConfigs(t *testing.T, pool *pgxpool.Pool) map[string]map[string]any {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT code, config FROM sources WHERE kind = 'EVM'`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for rows.Next() {
		var code string
		var raw []byte
		if err := rows.Scan(&code, &raw); err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := json.Unmarshal(raw, &config); err != nil {
			t.Fatalf("%s config: %v", code, err)
		}
		out[code] = config
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func aliases(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT format('%s %s %s %s', s.code, a.native_asset, a.asset, a.decimals)
		FROM asset_aliases a JOIN sources s ON s.id = a.source_id ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func migrateTo(t *testing.T, m *migrate.Migrate, version uint) {
	t.Helper()
	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	assertVersion(t, m, version)
}

// S3-T103 — Req: SRS — EVM Connector §3.1 "Values per network", §2.4 Seed data; S3 D-17, S3 D-27
func TestT103_SourceValuesAndAlias(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	m := testdb.Migrator(t)
	treasury := `"treasury_connection": "0b0b0b0b-0000-4000-8000-000000000001"`

	// A treasury_connection written before the migration of S3 stays.
	migrateTo(t, m, s2Version)
	t.Cleanup(func() {
		migrateTo(t, m, s2Version)
		mustExec(t, owner, `UPDATE sources SET config = config - 'treasury_connection'`)
		migrateTo(t, m, migrations.Latest())
	})
	before := sourceConfigs(t, owner)
	mustExec(t, owner, `UPDATE sources SET config = config || '{`+treasury+`}' WHERE code IN ('anvil', 'base-sepolia')`)
	beforeAliases := aliases(t, owner)

	migrateTo(t, m, migrations.Latest())
	want := map[string]map[string]any{
		"anvil": {
			"chain_id": 31337.0, "finality_mode": "confirmations", "finality_confirmations": 10.0,
			"treasury_connection": "0b0b0b0b-0000-4000-8000-000000000001",
		},
		"base-sepolia": {
			"chain_id": 84532.0, "finality_mode": "tag", "finality_tag": "finalized",
			"controller_address": "0xF75D58dc6E33487dB994D81D0d870D61Eac45F37", "backfill_floor": 47768907.0,
			"treasury_connection": "0b0b0b0b-0000-4000-8000-000000000001",
		},
	}
	got := sourceConfigs(t, owner)
	for code, w := range want {
		if !maps.Equal(got[code], w) {
			t.Errorf("%s config = %v, want %v", code, got[code], w)
		}
	}
	// backfill_floor is a JSON number, not a string.
	var floorType string
	mustQueryRow(t, owner, &floorType, `SELECT jsonb_typeof(config->'backfill_floor') FROM sources WHERE code = 'base-sepolia'`)
	if floorType != "number" {
		t.Errorf("backfill_floor is a JSON %s, want number", floorType)
	}

	wantAlias := "base-sepolia " + baseSepoliaMockUSDC + " USDC 6"
	if got := aliases(t, owner); !slices.Equal(got, append(slices.Clone(beforeAliases), wantAlias)) {
		t.Errorf("asset_aliases = %v, want %v and %q", got, beforeAliases, wantAlias)
	}
	for _, address := range []string{baseSepoliaMockUSDC, want["base-sepolia"]["controller_address"].(string)} {
		if eip55 := evm.ToEIP55(strings.ToLower(address[2:])); eip55 != address {
			t.Errorf("%s is not in EIP-55 form: %s", address, eip55)
		}
	}

	// Down removes exactly what up added: chain_id and treasury_connection stay.
	migrateTo(t, m, s2Version)
	got = sourceConfigs(t, owner)
	for code := range want {
		w := maps.Clone(before[code])
		w["treasury_connection"] = "0b0b0b0b-0000-4000-8000-000000000001"
		if !maps.Equal(got[code], w) {
			t.Errorf("%s config after down = %v, want %v", code, got[code], w)
		}
	}
	if got := aliases(t, owner); !slices.Equal(got, beforeAliases) {
		t.Errorf("asset_aliases after down = %v, want %v", got, beforeAliases)
	}
}

// S3-T106 — Req: SRS — Core §2.4, §3.2; S3 D-5, S3 D-6, S3 D-7
func TestT106_MigrationsOfS3(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	m := testdb.Migrator(t)

	migrateTo(t, m, s2Version)
	t.Cleanup(func() { migrateTo(t, m, migrations.Latest()) })
	beforeSchema, beforeServer := schema(t, owner), grantsOf(t, owner, "cas_server")
	beforeCardAuth := grantsOf(t, owner, "cas_card_auth")
	for _, table := range s3Tables {
		if slices.Contains(tables(t, owner), table) {
			t.Fatalf("%s exists at version %d", table, s2Version)
		}
	}

	migrateTo(t, m, migrations.Latest())

	t.Run("tables", func(t *testing.T) {
		got := schema(t, owner)
		var s3 []string
		for _, line := range got {
			if strings.Contains(line, "reconciliation_runs") || strings.Contains(line, "balance_checkpoints") {
				s3 = append(s3, line)
			}
		}
		want := []string{
			"column balance_checkpoints.asset 3 text null=NO default=- identity=-",
			"column balance_checkpoints.balance 7 numeric null=NO default=- identity=-",
			"column balance_checkpoints.block_hash 5 text null=YES default=- identity=-",
			"column balance_checkpoints.block_number 4 int8 null=YES default=- identity=-",
			"column balance_checkpoints.checked_at 10 timestamptz null=NO default=now() identity=-",
			"column balance_checkpoints.connection_id 1 uuid null=NO default=- identity=-",
			"column balance_checkpoints.gap 9 numeric null=NO default=- identity=-",
			"column balance_checkpoints.ledger_total 8 numeric null=NO default=- identity=-",
			"column balance_checkpoints.native_asset 2 text null=NO default=- identity=-",
			"column balance_checkpoints.taken_at 6 timestamptz null=NO default=- identity=-",
			"column reconciliation_runs.created_at 9 timestamptz null=NO default=now() identity=-",
			"column reconciliation_runs.id 1 uuid null=NO default=gen_random_uuid() identity=-",
			"column reconciliation_runs.mismatches 8 jsonb null=NO default=- identity=-",
			"column reconciliation_runs.period_from 4 timestamptz null=NO default=- identity=-",
			"column reconciliation_runs.period_to 5 timestamptz null=NO default=- identity=-",
			"column reconciliation_runs.source_id 3 int2 null=NO default=- identity=-",
			"column reconciliation_runs.tenant_id 2 uuid null=NO default=- identity=-",
			"column reconciliation_runs.to_block 6 int8 null=NO default=- identity=-",
			"column reconciliation_runs.totals 7 jsonb null=NO default=- identity=-",
			"constraint balance_checkpoints.balance_checkpoints_balance_check CHECK ((balance >= (0)::numeric))",
			"constraint balance_checkpoints.balance_checkpoints_connection_id_fkey FOREIGN KEY (connection_id) REFERENCES connections(id) ON DELETE CASCADE",
			"constraint balance_checkpoints.balance_checkpoints_pkey PRIMARY KEY (connection_id, native_asset)",
			"constraint reconciliation_runs.reconciliation_runs_pkey PRIMARY KEY (id)",
			"constraint reconciliation_runs.reconciliation_runs_source_id_fkey FOREIGN KEY (source_id) REFERENCES sources(id)",
			"constraint reconciliation_runs.reconciliation_runs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES tenants(id)",
			"grant balance_checkpoints cas_server INSERT",
			"grant balance_checkpoints cas_server SELECT",
			"grant balance_checkpoints cas_server UPDATE",
			"grant reconciliation_runs cas_server INSERT",
			"grant reconciliation_runs cas_server SELECT",
			"index CREATE INDEX reconciliation_runs_tenant_source_created_idx ON public.reconciliation_runs USING btree (tenant_id, source_id, created_at DESC)",
			"index CREATE UNIQUE INDEX balance_checkpoints_pkey ON public.balance_checkpoints USING btree (connection_id, native_asset)",
			"index CREATE UNIQUE INDEX reconciliation_runs_pkey ON public.reconciliation_runs USING btree (id)",
		}
		if !slices.Equal(s3, want) {
			t.Errorf("schema of the S3 tables:\n got  %v\n want %v", s3, want)
		}
		var precision []string
		rows, err := owner.Query(ctx, `SELECT format('%s %s,%s', column_name, numeric_precision, numeric_scale)
			FROM information_schema.columns WHERE table_name = 'balance_checkpoints' AND data_type = 'numeric'
			ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		if precision, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			t.Fatal(err)
		}
		if w := []string{"balance 38,18", "gap 38,18", "ledger_total 38,18"}; !slices.Equal(precision, w) {
			t.Errorf("numeric columns = %v, want %v", precision, w)
		}
	})

	t.Run("rights", func(t *testing.T) {
		server := testdb.OpenServer(t)
		added := []string{
			"column operator_txs.block_number SELECT",
			"table balance_checkpoints INSERT", "table balance_checkpoints SELECT", "table balance_checkpoints UPDATE",
			"table reconciliation_runs INSERT", "table reconciliation_runs SELECT",
		}
		if got, w := grantsOf(t, owner, "cas_server"), slices.Sorted(slices.Values(slices.Concat(beforeServer, added))); !slices.Equal(got, w) {
			t.Errorf("cas_server grants = %v, want %v", got, w)
		}
		if got := grantsOf(t, owner, "cas_card_auth"); !slices.Equal(got, beforeCardAuth) {
			t.Errorf("cas_card_auth grants = %v, want unchanged %v", got, beforeCardAuth)
		}

		f := newCardFixture(t, owner, "tenant-a")
		mustExec(t, server, `INSERT INTO reconciliation_runs (tenant_id, source_id, period_from, period_to, to_block,
			totals, mismatches) VALUES ($1, $2, now(), now(), 41, '{}', '[]')`, f.tenantID, f.sourceID)
		mustExec(t, server, `SELECT id, tenant_id, source_id, to_block, totals, mismatches, created_at
			FROM reconciliation_runs WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1`, f.tenantID)
		mustExec(t, server, `INSERT INTO balance_checkpoints (connection_id, native_asset, asset, block_number,
			block_hash, taken_at, balance, ledger_total, gap)
			VALUES ($1, '0xtoken', 'USDC', 41, '0xhash', now(), 70.133613, 70.133613, 0)`, f.connectionID)
		mustExec(t, server, `UPDATE balance_checkpoints SET balance = 70, ledger_total = 70.133613, gap = -0.133613,
			checked_at = now() WHERE connection_id = $1`, f.connectionID)
		mustExec(t, server, `SELECT * FROM balance_checkpoints`)
		mustExec(t, server, `SELECT authorization_id, return_row_id, purpose, status, tx_hash, block_number, created_at
			FROM operator_txs`)

		for _, stmt := range []string{
			`UPDATE reconciliation_runs SET to_block = 0`,
			`DELETE FROM reconciliation_runs`,
			`TRUNCATE reconciliation_runs`,
			`DELETE FROM balance_checkpoints`,
			`TRUNCATE balance_checkpoints`,
			`SELECT block_hash FROM operator_txs`,
			`SELECT * FROM operator_txs`,
			`UPDATE operator_txs SET block_number = 0`,
			`ALTER TABLE balance_checkpoints ADD COLUMN x INT`,
			`DROP TABLE reconciliation_runs`,
		} {
			expectCode(t, server, codeInsufficientPrivilege, stmt)
		}
		cardAuth := testdb.OpenCardAuth(t)
		for _, stmt := range []string{`SELECT * FROM reconciliation_runs`, `SELECT * FROM balance_checkpoints`} {
			expectCode(t, cardAuth, codeInsufficientPrivilege, stmt)
		}
	})

	t.Run("values", func(t *testing.T) {
		f := newTenant(t, owner, "tenant-b")
		addConnectionData(t, owner, &f, "0xaccount-b")
		checkpoint := `INSERT INTO balance_checkpoints (connection_id, native_asset, asset, taken_at, balance,
			ledger_total, gap) VALUES ($1, $2, 'USDC', now(), $3, $4, $5)`
		expectCode(t, owner, codeCheckViolation, checkpoint, f.connectionID, "0xa", "-0.000000000000000001", "0", "0")
		// A negative ledger total and gap are accepted; block number and hash are optional.
		mustExec(t, owner, checkpoint, f.connectionID, "0xb", "0", "-1.5", "1.5")
		expectCode(t, owner, codeUniqueViolation, checkpoint, f.connectionID, "0xb", "0", "0", "0")
		for _, column := range []string{"tenant_id", "source_id", "period_from", "period_to", "to_block", "totals", "mismatches"} {
			values := map[string]string{
				"tenant_id": "'" + f.tenantID + "'", "source_id": "1", "period_from": "now()", "period_to": "now()",
				"to_block": "1", "totals": "'{}'", "mismatches": "'[]'",
			}
			values[column] = "NULL"
			expectCode(t, owner, "23502", `INSERT INTO reconciliation_runs (tenant_id, source_id, period_from, period_to,
				to_block, totals, mismatches) VALUES (`+values["tenant_id"]+`, `+values["source_id"]+`, `+
				values["period_from"]+`, `+values["period_to"]+`, `+values["to_block"]+`, `+values["totals"]+`, `+
				values["mismatches"]+`)`)
		}

		// The checkpoints of a deleted connection go with it; the runs of the tenant stay.
		mustExec(t, owner, `INSERT INTO reconciliation_runs (tenant_id, source_id, period_from, period_to, to_block,
			totals, mismatches) VALUES ($1, $2, now(), now(), 1, '{}', '[]')`, f.tenantID, f.sourceID)
		mustExec(t, owner, `DELETE FROM connections WHERE id = $1`, f.connectionID)
		var checkpoints, runs int
		mustQueryRow(t, owner, &checkpoints, `SELECT count(*) FROM balance_checkpoints WHERE connection_id = $1`, f.connectionID)
		mustQueryRow(t, owner, &runs, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, f.tenantID)
		if checkpoints != 0 || runs != 1 {
			t.Errorf("after the delete: %d checkpoints, %d runs; want 0 and 1", checkpoints, runs)
		}
	})

	t.Run("down restores version 6", func(t *testing.T) {
		testdb.Clean(t)
		migrateTo(t, m, s2Version)
		if got := schema(t, owner); !slices.Equal(got, beforeSchema) {
			t.Errorf("schema after down differs:\nbefore %v\nafter  %v", beforeSchema, got)
		}
		if got := grantsOf(t, owner, "cas_server"); !slices.Equal(got, beforeServer) {
			t.Errorf("cas_server grants after down = %v, want %v", got, beforeServer)
		}
		migrateTo(t, m, migrations.Latest())
		if got := tables(t, owner); !slices.Contains(got, s3Tables[0]) || !slices.Contains(got, s3Tables[1]) {
			t.Errorf("tables after up again = %v, want %v among them", got, s3Tables)
		}
	})
}
