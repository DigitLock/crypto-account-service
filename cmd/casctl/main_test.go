// Phase 4 of docs/test-plan-c1.md, casctl rows: the cobra commands run in process against TEST_DATABASE_URL.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/auth"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

var (
	ctx         = context.Background()
	tokenFormat = regexp.MustCompile(`^cas_[0-9a-f]{12}_[0-9a-f]{64}$`)
	coreTables  = []string{
		"tenants", "api_credentials", "sources", "connections", "asset_aliases", "sync_cursors",
		"balance_snapshots", "snapshot_balances", "ledger_entries", "audit_log",
	}
)

type result struct {
	stdout, stderr string
	code           int
}

// casctl runs one command with CASCTL_DATABASE_URL set to the test database.
func casctl(t *testing.T, args ...string) result {
	t.Helper()
	url := testdb.URL(t)
	var out, errOut bytes.Buffer
	code := run(ctx, args, &out, &errOut, func(name string) string {
		if name == dbURLVar {
			return url
		}
		return ""
	})
	return result{stdout: out.String(), stderr: errOut.String(), code: code}
}

func mustCasctl(t *testing.T, args ...string) result {
	t.Helper()
	r := casctl(t, args...)
	if r.code != 0 {
		t.Fatalf("casctl %v: exit %d, stderr %q", args, r.code, r.stderr)
	}
	return r
}

// setup migrates and empties the test database and returns a pool of the owner role.
func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.Open(t)
	testdb.Clean(t)
	return pool
}

// issue issues a token and returns it with its secret in hexadecimal.
func issue(t *testing.T, tenant string) (token, secretHex string, r result) {
	t.Helper()
	r = mustCasctl(t, "token", "issue", tenant)
	token = strings.TrimSpace(r.stdout)
	if !tokenFormat.MatchString(token) {
		t.Fatalf("token issue printed %d characters, not a token", len(token))
	}
	return token, token[len("cas_")+13:], r
}

// databaseText returns every row of every Core table as text.
func databaseText(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var b strings.Builder
	for _, table := range coreTables {
		rows, err := pool.Query(ctx, "SELECT x::text FROM "+table+" x")
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(strings.Join(lines, "\n"))
	}
	return b.String()
}

func assertAbsent(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s contains a token secret", where)
		}
	}
}

// C1-T401 — Req: UC-105, EC-120
func TestT401_CreateTenant(t *testing.T) {
	pool := setup(t)

	r := mustCasctl(t, "tenant", "create", "tenant-a")
	if !strings.Contains(r.stdout, "tenant-a") {
		t.Errorf("create printed %q", r.stdout)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tenants WHERE name = 'tenant-a'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ACTIVE" {
		t.Errorf("status = %s, want ACTIVE", status)
	}

	r = casctl(t, "tenant", "create", "tenant-a")
	if r.code != 1 || !strings.Contains(r.stderr, "exists") {
		t.Errorf("second create: exit %d, stderr %q; want 1 and a refusal", r.code, r.stderr)
	}
	var tenants, audits int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenants), (SELECT count(*) FROM audit_log)`).
		Scan(&tenants, &audits); err != nil {
		t.Fatal(err)
	}
	if tenants != 1 || audits != 1 {
		t.Errorf("after the refused create: %d tenants, %d audit rows; want 1 and 1", tenants, audits)
	}

	t.Run("name rule", func(t *testing.T) {
		for _, name := range []string{
			"", " tenant", "tenant ", "Tenant", strings.Repeat("a", 65), "tenänt", "-tenant",
		} {
			var auditsBefore int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&auditsBefore); err != nil {
				t.Fatal(err)
			}
			// After "--", so that cobra passes a leading "-" as the name, not as a flag.
			r := casctl(t, "tenant", "create", "--", name)
			if r.code != 1 || !strings.Contains(r.stderr, "1 to 64 characters") {
				t.Errorf("name %q: exit %d, stderr %q; want 1 and the rule", name, r.code, r.stderr)
			}
			var rows, audits int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenants WHERE name = $1),
				(SELECT count(*) FROM audit_log)`, name).Scan(&rows, &audits); err != nil {
				t.Fatal(err)
			}
			if rows != 0 || audits != auditsBefore {
				t.Errorf("name %q: %d tenant rows, %d new audit rows; want none", name, rows, audits-auditsBefore)
			}
		}
		for _, name := range []string{"a", strings.Repeat("b", 64), "tenant_a-1"} {
			mustCasctl(t, "tenant", "create", name)
			var stored string
			if err := pool.QueryRow(ctx, `SELECT name FROM tenants WHERE name = $1`, name).Scan(&stored); err != nil {
				t.Errorf("name %q not stored: %v", name, err)
			}
		}
	})

	t.Run("without CASCTL_DATABASE_URL", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := run(ctx, []string{"tenant", "list"}, &out, &errOut, func(string) string { return "" })
		if code != 1 || !strings.Contains(errOut.String(), "CASCTL_DATABASE_URL") {
			t.Errorf("exit %d, stderr %q; want 1 and the name of the variable", code, errOut.String())
		}
	})
}

// C1-T402 — Req: UC-105, ADR-7, §3.2
func TestT402_IssueToken(t *testing.T) {
	pool := setup(t)
	mustCasctl(t, "tenant", "create", "tenant-a")

	token, secretHex, r := issue(t, "tenant-a")
	if r.stdout != token+"\n" {
		t.Errorf("stdout = %q, want the token alone", r.stdout)
	}
	if !strings.Contains(r.stderr, "only once") {
		t.Errorf("no note that the token is shown once: %q", r.stderr)
	}
	assertAbsent(t, "stderr", r.stderr, token, secretHex)

	keyID, secret, err := auth.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	var hash []byte
	if err := pool.QueryRow(ctx, `SELECT kind, secret_hash FROM api_credentials WHERE key_id = $1`, keyID).
		Scan(&kind, &hash); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(secret)
	if kind != "SERVICE_TOKEN" || !bytes.Equal(hash, want[:]) {
		t.Errorf("row: kind %s, hash matches %v; want SERVICE_TOKEN and the SHA-256 of the secret",
			kind, bytes.Equal(hash, want[:]))
	}
	assertAbsent(t, "the database", databaseText(t, pool), token, secretHex)
}

// C1-T411 — Req: UC-105
func TestT411_TokenFormat(t *testing.T) {
	pool := setup(t)
	mustCasctl(t, "tenant", "create", "tenant-a")

	token1, _, _ := issue(t, "tenant-a")
	token2, _, _ := issue(t, "tenant-a")
	if token1 == token2 {
		t.Fatal("two issued tokens are equal")
	}
	for _, token := range []string{token1, token2} {
		keyID, secret, err := auth.Parse(token)
		if err != nil {
			t.Fatal(err)
		}
		var hash []byte
		if err := pool.QueryRow(ctx, `SELECT secret_hash FROM api_credentials WHERE key_id = $1`, keyID).
			Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if want := sha256.Sum256(secret); !bytes.Equal(hash, want[:]) {
			t.Error("secret_hash is not the SHA-256 of the 32 secret bytes")
		}
	}
}

// C1-T413 — Req: FR-121
func TestT413_AuditOfCLIChanges(t *testing.T) {
	pool := setup(t)

	mustCasctl(t, "tenant", "create", "tenant-a")
	mustCasctl(t, "tenant", "disable", "tenant-a")
	if r := mustCasctl(t, "tenant", "disable", "tenant-a"); !strings.Contains(r.stdout, "nothing changed") {
		t.Errorf("second disable printed %q", r.stdout)
	}
	mustCasctl(t, "tenant", "enable", "tenant-a")
	token, _, _ := issue(t, "tenant-a")
	keyID, _, _ := auth.Parse(token)
	mustCasctl(t, "token", "revoke", keyID)
	if r := mustCasctl(t, "token", "revoke", keyID); !strings.Contains(r.stdout, "nothing changed") {
		t.Errorf("second revoke printed %q", r.stdout)
	}

	var tenantID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE name = 'tenant-a'`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT action, object_id, credential_id IS NULL, tenant_id::text
		FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type auditRow struct {
		action, objectID string
		noCredential     bool
		tenantID         string
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (auditRow, error) {
		var a auditRow
		err := r.Scan(&a.action, &a.objectID, &a.noCredential, &a.tenantID)
		return a, err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []auditRow{
		{"TENANT_CREATED", tenantID, true, tenantID},
		{"TENANT_DISABLED", tenantID, true, tenantID},
		{"TENANT_ENABLED", tenantID, true, tenantID},
		{"CREDENTIAL_ISSUED", keyID, true, tenantID},
		{"CREDENTIAL_REVOKED", keyID, true, tenantID},
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit rows:\n got %v\nwant %v", got, want)
	}

	t.Run("credentials of another kind are not service tokens", func(t *testing.T) {
		const processorUser = "processor-user"
		if _, err := pool.Exec(ctx, `INSERT INTO api_credentials (tenant_id, kind, key_id, secret_hash)
			VALUES ($1, 'PROCESSOR_BASIC', $2, '\x00')`, tenantID, processorUser); err != nil {
			t.Fatal(err)
		}
		var auditsBefore int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&auditsBefore); err != nil {
			t.Fatal(err)
		}

		for _, args := range [][]string{{"token", "list"}, {"token", "list", "tenant-a"}} {
			if r := mustCasctl(t, args...); strings.Contains(r.stdout, processorUser) {
				t.Errorf("casctl %v lists the PROCESSOR_BASIC credential:\n%s", args, r.stdout)
			}
		}
		if r := casctl(t, "token", "revoke", processorUser); r.code != 1 {
			t.Errorf("token revoke of a PROCESSOR_BASIC key_id: exit %d, want 1", r.code)
		}
		var revoked bool
		var audits int
		if err := pool.QueryRow(ctx, `SELECT (SELECT revoked_at IS NOT NULL FROM api_credentials WHERE key_id = $1),
			(SELECT count(*) FROM audit_log)`, processorUser).Scan(&revoked, &audits); err != nil {
			t.Fatal(err)
		}
		if revoked || audits != auditsBefore {
			t.Errorf("after the refused revoke: revoked %v, %d new audit rows; want not revoked and none",
				revoked, audits-auditsBefore)
		}
	})
}

// C1-T414 — Req: UC-105
func TestT414_ListsShowNoSecret(t *testing.T) {
	pool := setup(t)
	mustCasctl(t, "tenant", "create", "tenant-a")
	mustCasctl(t, "tenant", "create", "tenant-b")
	mustCasctl(t, "tenant", "disable", "tenant-b")
	tokenA, secretA, _ := issue(t, "tenant-a")
	tokenB, secretB, _ := issue(t, "tenant-b")
	keyA, _, _ := auth.Parse(tokenA)
	keyB, _, _ := auth.Parse(tokenB)
	mustCasctl(t, "token", "revoke", keyB)

	var hashes []string
	rows, err := pool.Query(ctx, `SELECT encode(secret_hash, 'hex') FROM api_credentials`)
	if err != nil {
		t.Fatal(err)
	}
	if hashes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	secrets := slices.Concat([]string{tokenA, tokenB, secretA, secretB}, hashes)

	tenants := mustCasctl(t, "tenant", "list")
	for _, want := range []string{"NAME", "STATUS", "CREATED", "tenant-a", "ACTIVE", "tenant-b", "DISABLED"} {
		if !strings.Contains(tenants.stdout, want) {
			t.Errorf("tenant list lacks %q:\n%s", want, tenants.stdout)
		}
	}

	all := mustCasctl(t, "token", "list")
	for _, want := range []string{"KEY_ID", "TENANT", "CREATED", "REVOKED", keyA, keyB} {
		if !strings.Contains(all.stdout, want) {
			t.Errorf("token list lacks %q:\n%s", want, all.stdout)
		}
	}
	lineA := lineWith(all.stdout, keyA)
	lineB := lineWith(all.stdout, keyB)
	if !strings.HasSuffix(strings.TrimSpace(lineA), "-") || strings.HasSuffix(strings.TrimSpace(lineB), "-") {
		t.Errorf("revoked column: %q, %q; want - for the valid token and a time for the revoked one", lineA, lineB)
	}

	one := mustCasctl(t, "token", "list", "tenant-a")
	if !strings.Contains(one.stdout, keyA) || strings.Contains(one.stdout, keyB) {
		t.Errorf("token list tenant-a:\n%s", one.stdout)
	}
	if r := casctl(t, "token", "list", "no-such-tenant"); r.code != 1 {
		t.Errorf("token list of an unknown tenant: exit %d, want 1", r.code)
	}

	for _, r := range []result{tenants, all, one} {
		assertAbsent(t, "list output", r.stdout+r.stderr, secrets...)
	}
}

func lineWith(text, s string) string {
	for line := range strings.Lines(text) {
		if strings.Contains(line, s) {
			return line
		}
	}
	return ""
}

// C1-T536 — Req: §2.1.1, §2.4, §3.1. source add-fake; cmd/server checks the flag of the server.
func TestT536_FakeSourceBehindItsFlag(t *testing.T) {
	pool := setup(t)

	if r := mustCasctl(t, "source", "add-fake"); r.stdout != "Source fake added\n" {
		t.Errorf("first run printed %q", r.stdout)
	}
	if r := mustCasctl(t, "source", "add-fake"); r.stdout != "Source fake exists: nothing changed\n" {
		t.Errorf("second run printed %q", r.stdout)
	}

	var rows int
	var kind, config string
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT count(*) OVER (), kind, enabled, config::text FROM sources WHERE code = 'fake'`).
		Scan(&rows, &kind, &enabled, &config); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || kind != "EXCHANGE" || !enabled || config != "{}" {
		t.Errorf("fake: %d rows, kind %s, enabled %v, config %s; want one enabled EXCHANGE row with {}", rows, kind, enabled, config)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Errorf("add-fake wrote %d audit rows, want none", audits)
	}
}
