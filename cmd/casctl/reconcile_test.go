package main

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "0x" + hex.EncodeToString(b)
}

func storedRunIDs(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text FROM reconciliation_runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// S3-T616 — Req: SRS — Core UC-105 row 10, EC-123; S3 D-8, S3 D-24. casctl reconcile prints one line per stored run
// with the tenant, the run ID and the mismatches by type, and exits 0 with mismatches. Refused with a message and
// nothing stored: an unknown source, a source of kind EXCHANGE, a source without treasury_connection (EC-123), a
// treasury never indexed. The rules on Anvil are shown in internal/reconcile; here the treasury, its cursor and its
// entries are rows written by the test.
func TestT616_Reconcile(t *testing.T) {
	pool := setup(t)
	keepAnvilConfig(t, pool)
	mustCasctl(t, "source", "add-fake")
	mustCasctl(t, "tenant", "create", "cas-platform")
	mustCasctl(t, "tenant", "create", "tenant-a")
	_, address := randomAddress(t)
	treasury := insertConnection(t, pool, "cas-platform", "anvil", address)
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
	// A's APPROVED authorization without a Debited (MISSING_DEBIT) and a Debited of no authorization (UNKNOWN_DEBIT).
	exec(`INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, token_amount, chain_id, status, received_at,
		valid_until) SELECT id, 'a1', decode($1, 'hex'), 8000000, 31337, 'APPROVED', '2026-10-07T09:00:00Z',
		'2026-10-07T09:00:04Z' FROM tenants WHERE name = 'tenant-a'`, randomHex(t, 32)[2:])
	tx, unknown := randomHex(t, 32), randomHex(t, 32)
	exec(`INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id, type, direction,
		asset, native_asset, amount, occurred_at, raw) SELECT tenant_id, id, 'logs', $2 || ':1', 'SINGLE', 'logs:' || $2 || ':1',
		'CARD_DEBIT', 'IN', 'USDC', '0x00000000000000000000000000000000000A6160', 3, '2026-10-07T09:30:00Z',
		jsonb_build_object('transactionHash', $2::text, 'event', 'Debited',
			'args', jsonb_build_object('authId', $3::text, 'amount', '3000000'))
		FROM connections WHERE id = $1`, treasury, tx, unknown)

	refused := func(name, message string, args ...string) {
		t.Helper()
		r := casctl(t, append([]string{"reconcile"}, args...)...)
		if r.code == 0 || !strings.Contains(r.stderr, message) || r.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want an error with %q", name, r.code, r.stdout, r.stderr, message)
		}
		if ids := storedRunIDs(t, pool); len(ids) != 0 {
			t.Errorf("%s: %d runs stored, want none", name, len(ids))
		}
	}
	refused("unknown source", "no source with this code", "polygon")
	refused("source of kind EXCHANGE", "not of kind EVM", "fake")
	refused("no treasury_connection (EC-123)", "no treasury_connection", "anvil")
	refused("no source", "accepts 1 arg")
	mustCasctl(t, "source", "set-treasury", "anvil", treasury)
	refused("treasury without a logs cursor", "no stored logs yet", "anvil")
	exec(`INSERT INTO sync_cursors (connection_id, stream, mode, cursor, next_run_at) VALUES ($1, 'logs', 'BACKFILL', '{}', now())`, treasury)
	refused("treasury never indexed", "no stored logs yet", "anvil")

	exec(`UPDATE sync_cursors SET mode = 'INCREMENTAL',
		cursor = '{"next_block": 120, "last_hash": "0x01", "last_time": "2026-10-07T10:00:00Z"}' WHERE connection_id = $1`, treasury)
	r := mustCasctl(t, "reconcile", "anvil")
	line := regexp.MustCompile(`^Tenant (\S+), run ([0-9a-f-]{36}): (.+)$`)
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	want := [][2]string{{"tenant-a", "MISSING_DEBIT 1"}, {"cas-platform", "UNKNOWN_DEBIT 1"}}
	var printed []string
	if len(lines) != len(want) {
		t.Fatalf("stdout %q, want %d lines", r.stdout, len(want))
	}
	for i, l := range lines {
		m := line.FindStringSubmatch(l)
		if m == nil || m[1] != want[i][0] || m[3] != want[i][1] {
			t.Errorf("line %q, want tenant %s with %s", l, want[i][0], want[i][1])
			continue
		}
		printed = append(printed, m[2])
	}
	stored := storedRunIDs(t, pool)
	if len(stored) != 2 || !(strings.Contains(r.stdout, stored[0]) && strings.Contains(r.stdout, stored[1])) {
		t.Errorf("stored runs %v, printed %v", stored, printed)
	}

	// The second run of the same state: two more rows; mismatches persist (runs are cumulative).
	r = mustCasctl(t, "reconcile", "anvil")
	if strings.Count(r.stdout, "\n") != 2 || len(storedRunIDs(t, pool)) != 4 {
		t.Errorf("second run: stdout %q, %d stored runs", r.stdout, len(storedRunIDs(t, pool)))
	}

	// Without findings: both lines print "no mismatches".
	exec(`DELETE FROM ledger_entries`)
	exec(`UPDATE authorizations SET status = 'DECLINED'`)
	r = mustCasctl(t, "reconcile", "anvil")
	if strings.Count(r.stdout, ": no mismatches\n") != 2 || strings.Count(r.stdout, "\n") != 2 {
		t.Errorf("stdout %q, want two lines with no mismatches", r.stdout)
	}

	// treasury_connection unset again: refused, nothing more stored.
	exec(`UPDATE sources SET config = config - 'treasury_connection' WHERE code = 'anvil'`)
	before := len(storedRunIDs(t, pool))
	r = casctl(t, "reconcile", "anvil")
	if r.code == 0 || !strings.Contains(r.stderr, "no treasury_connection") || len(storedRunIDs(t, pool)) != before {
		t.Errorf("treasury unset: exit %d, stderr %q", r.code, r.stderr)
	}
}
