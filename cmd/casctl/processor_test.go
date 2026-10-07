package main

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

var pairFormat = regexp.MustCompile(`^([0-9a-f]{12}):([0-9a-f]{64})$`)

// S2-T711 — Req: SRS — Core UC-105 rows 5 – 7. The "401 after revocation" part of the row needs the processor
// API of card-auth and follows in st5.
func TestT711_ProcessorCredentialCLI(t *testing.T) {
	pool := setup(t)
	mustCasctl(t, "tenant", "create", "tenant-a")
	mustCasctl(t, "tenant", "create", "tenant-b")

	r := mustCasctl(t, "processor", "issue", "tenant-a")
	m := pairFormat.FindStringSubmatch(strings.TrimSpace(r.stdout))
	if m == nil || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("processor issue printed %d characters, not one username:password line", len(r.stdout))
	}
	username, password := m[1], m[2]
	if !strings.Contains(r.stderr, username) || !strings.Contains(r.stderr, "only once") || strings.Contains(r.stderr, password) {
		t.Errorf("the note on stderr: %q", r.stderr)
	}
	mustCasctl(t, "processor", "issue", "tenant-b")

	t.Run("only the hash is stored", func(t *testing.T) {
		raw, err := hex.DecodeString(password)
		if err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(raw)
		var kind string
		var hash []byte
		if err := pool.QueryRow(ctx, `SELECT kind, secret_hash FROM api_credentials WHERE key_id = $1`, username).
			Scan(&kind, &hash); err != nil {
			t.Fatal(err)
		}
		if kind != "PROCESSOR_BASIC" || string(hash) != string(want[:]) {
			t.Errorf("row: kind %s, hash is the SHA-256 of the 32 password bytes: %v", kind, string(hash) == string(want[:]))
		}
		assertAbsent(t, "the database", databaseText(t, pool), password, strings.ToUpper(password))
	})

	t.Run("list shows no secret and no hash", func(t *testing.T) {
		all := mustCasctl(t, "processor", "list").stdout
		one := mustCasctl(t, "processor", "list", "tenant-a").stdout
		if strings.Count(all, "\n") != 3 || strings.Count(one, "\n") != 2 || !strings.Contains(one, username) ||
			!strings.Contains(one, "USERNAME") {
			t.Errorf("list:\n%s\nlist tenant-a:\n%s", all, one)
		}
		assertAbsent(t, "processor list", all+one, password)
		if strings.Contains(mustCasctl(t, "token", "list").stdout, username) {
			t.Error("token list shows a processor credential")
		}
	})

	t.Run("a processor credential is not a service token", func(t *testing.T) {
		if res := casctl(t, "token", "revoke", username); res.code == 0 {
			t.Errorf("token revoke accepted a processor username: %q", res.stdout)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		out := mustCasctl(t, "processor", "revoke", username).stdout
		if !strings.Contains(out, "revoked") {
			t.Errorf("revoke printed %q", out)
		}
		var revokedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT revoked_at FROM api_credentials WHERE key_id = $1`, username).Scan(&revokedAt); err != nil {
			t.Fatal(err)
		}
		if revokedAt == nil {
			t.Error("revoked_at is not set")
		}
		if again := mustCasctl(t, "processor", "revoke", username).stdout; !strings.Contains(again, "nothing changed") {
			t.Errorf("second revoke printed %q", again)
		}
		if res := casctl(t, "processor", "revoke", "000000000000"); res.code != 1 || !strings.Contains(res.stderr, "no processor credential") {
			t.Errorf("unknown username: exit %d, %q", res.code, res.stderr)
		}
	})

	t.Run("audit", func(t *testing.T) {
		rows, err := pool.Query(ctx, `SELECT action, coalesce(details->>'kind', ''), credential_id IS NULL FROM audit_log
			WHERE object_id = $1 ORDER BY id`, username)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var action, kind string
			var cli bool
			if err := rows.Scan(&action, &kind, &cli); err != nil {
				t.Fatal(err)
			}
			if !cli {
				t.Errorf("%s has a credential_id; the CLI has none", action)
			}
			got = append(got, action+" "+kind)
		}
		if want := "CREDENTIAL_ISSUED PROCESSOR_BASIC,CREDENTIAL_REVOKED PROCESSOR_BASIC"; strings.Join(got, ",") != want {
			t.Errorf("audit = %v, want %s: one row per change", got, want)
		}
	})
}
