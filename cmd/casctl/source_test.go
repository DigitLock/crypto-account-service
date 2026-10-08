package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
)

// randomAddress returns a fictitious address generated at run time, in lower case and in EIP-55 form.
func randomAddress(t *testing.T) (lower, eip55 string) {
	t.Helper()
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	digits := hex.EncodeToString(b)
	return "0x" + digits, evm.ToEIP55(digits)
}

func anvilConfig(t *testing.T, pool *pgxpool.Pool) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func anvilAliases(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT format('%s %s %s', a.native_asset, a.asset, a.decimals)
		FROM asset_aliases a JOIN sources s ON s.id = a.source_id WHERE s.code = 'anvil' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// S3-T108 — Req: SRS — Core UC-105 row 8, EC-122; S3 D-17, S3 D-27
func TestT108_SourceSet(t *testing.T) {
	pool := setup(t)
	// The config of the seeded source anvil is restored for the other tests; testdb.Clean removes its aliases.
	var saved []byte
	if err := pool.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `UPDATE sources SET config = $1 WHERE code = 'anvil'`, saved); err != nil {
			t.Error(err)
		}
	})
	before := anvilConfig(t, pool)
	controllerLower, controller := randomAddress(t)
	tokenLower, token := randomAddress(t)
	_, token2 := randomAddress(t)

	t.Run("values written", func(t *testing.T) {
		r := mustCasctl(t, "source", "set", "anvil", "controller_address="+controllerLower)
		if want := "Source anvil, controller_address: previous unset, new " + controller + "\n"; r.stdout != want {
			t.Errorf("stdout = %q, want %q", r.stdout, want)
		}
		r = mustCasctl(t, "source", "set", "anvil", "backfill_floor=123")
		if want := "previous unset, new 123\n"; !strings.HasSuffix(r.stdout, want) {
			t.Errorf("stdout = %q, want the suffix %q", r.stdout, want)
		}
		r = mustCasctl(t, "source", "set", "anvil", "backfill_floor=456")
		if want := "previous 123, new 456\n"; !strings.HasSuffix(r.stdout, want) {
			t.Errorf("stdout = %q, want the suffix %q", r.stdout, want)
		}
		r = mustCasctl(t, "source", "set", "anvil", "controller_address="+controller)
		if want := "previous " + controller + ", new " + controller + "\n"; !strings.HasSuffix(r.stdout, want) {
			t.Errorf("stdout = %q, want the suffix %q", r.stdout, want)
		}

		got := anvilConfig(t, pool)
		want := map[string]string{}
		for k, v := range before {
			want[k] = string(v)
		}
		want["controller_address"] = `"` + controller + `"`
		want["backfill_floor"] = "456" // a JSON number
		if len(got) != len(want) {
			t.Errorf("config = %v, want %v", got, want)
		}
		for k, v := range want {
			if string(got[k]) != v {
				t.Errorf("config %s = %s, want %s", k, got[k], v)
			}
		}
	})

	t.Run("token alias written and replaced", func(t *testing.T) {
		configBefore := anvilConfig(t, pool)
		r := mustCasctl(t, "source", "set", "anvil", "token_address="+tokenLower)
		if want := "Source anvil, token_address: previous unset, new " + token + "\n"; r.stdout != want {
			t.Errorf("stdout = %q, want %q", r.stdout, want)
		}
		if got := anvilAliases(t, pool); len(got) != 1 || got[0] != token+" USDC 6" {
			t.Errorf("aliases = %v, want %s USDC 6", got, token)
		}
		r = mustCasctl(t, "source", "set", "anvil", "token_address="+token2)
		if want := "previous " + token + ", new " + token2 + "\n"; !strings.HasSuffix(r.stdout, want) {
			t.Errorf("stdout = %q, want the suffix %q", r.stdout, want)
		}
		if got := anvilAliases(t, pool); len(got) != 1 || got[0] != token2+" USDC 6" {
			t.Errorf("aliases = %v, want only %s USDC 6", got, token2)
		}
		if got := anvilConfig(t, pool); len(got) != len(configBefore) || string(got["controller_address"]) != `"`+controller+`"` {
			t.Errorf("token_address changed config: %v", got)
		}
	})

	t.Run("no audit row", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d audit rows, want 0", n)
		}
	})

	t.Run("refused, nothing changed", func(t *testing.T) {
		mustCasctl(t, "source", "add-fake")
		_, mixed := randomAddress(t)
		// A wrong checksum: the case of one letter flipped.
		bad := []byte(mixed)
		for i := 2; i < len(bad); i++ {
			if c := bad[i]; c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
				bad[i] ^= 0x20
				break
			}
		}
		snapshot := databaseText(t, pool)
		for name, c := range map[string]struct {
			args    []string
			message string
		}{
			"key outside the list":       {[]string{"anvil", "finality_mode=tag"}, "the key must be one of"},
			"treasury_connection":        {[]string{"anvil", "treasury_connection=0b0b0b0b-0000-4000-8000-000000000001"}, "the key must be one of"},
			"chain_id":                   {[]string{"anvil", "chain_id=1"}, "the key must be one of"},
			"malformed address":          {[]string{"anvil", "controller_address=0x1234"}, "must be an address"},
			"wrong checksum":             {[]string{"anvil", "controller_address=" + string(bad)}, "EIP-55"},
			"zero address":               {[]string{"anvil", "token_address=0x0000000000000000000000000000000000000000"}, "zero address"},
			"address with a space":       {[]string{"anvil", "token_address= " + controllerLower}, "must be an address"},
			"negative block":             {[]string{"anvil", "backfill_floor=-1"}, "block number"},
			"fraction":                   {[]string{"anvil", "backfill_floor=1.5"}, "block number"},
			"empty block":                {[]string{"anvil", "backfill_floor="}, "block number"},
			"block with a sign":          {[]string{"anvil", "backfill_floor=+5"}, "block number"},
			"block above BIGINT":         {[]string{"anvil", "backfill_floor=9223372036854775808"}, "block number"},
			"unknown source":             {[]string{"polygon", "backfill_floor=1"}, "no source with this code"},
			"source of kind EXCHANGE":    {[]string{"fake", "backfill_floor=1"}, "not of kind EVM"},
			"EXCHANGE and a token":       {[]string{"fake", "token_address=" + controllerLower}, "not of kind EVM"},
			"no pair":                    {[]string{"anvil", "backfill_floor"}, "<key>=<value>"},
			"two pairs in one call":      {[]string{"anvil", "backfill_floor=1", "controller_address=" + controllerLower}, "accepts 2 arg"},
			"source without a pair":      {[]string{"anvil"}, "accepts 2 arg"},
			"key in another letter case": {[]string{"anvil", "Backfill_Floor=1"}, "the key must be one of"},
		} {
			r := casctl(t, append([]string{"source", "set"}, c.args...)...)
			if r.code == 0 || !strings.Contains(r.stderr, c.message) || r.stdout != "" {
				t.Errorf("%s: exit %d, stdout %q, stderr %q; want an error with %q", name, r.code, r.stdout, r.stderr, c.message)
			}
		}
		if got := databaseText(t, pool); got != snapshot {
			t.Error("a refused change changed the database")
		}
	})
}

// insertConnection inserts a connection of tenant on source with the wallet address, as CreateConnection stores it,
// and returns its ID. The connections of these tests are rows only: no stream runs.
func insertConnection(t *testing.T, pool *pgxpool.Pool, tenant, source, address string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account, label)
		SELECT t.id, 'treasury', s.id, $3, 'Treasury' FROM tenants t, sources s WHERE t.name = $1 AND s.code = $2
		RETURNING id::text`, tenant, source, address).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// keepAnvilConfig restores the config of the seeded source anvil at the end of the test.
func keepAnvilConfig(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var saved []byte
	if err := pool.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `UPDATE sources SET config = $1 WHERE code = 'anvil'`, saved); err != nil {
			t.Error(err)
		}
	})
}

// S3-T505, the casctl part — Req: SRS — Core UC-105 row 9; S3 D-2, S3 D-32. set-treasury writes the ID of the
// connection and its address in EIP-55 form, keeps the other keys, prints the previous and new values; no audit row.
// The connection created through the API and the start check are shown in internal/engine.
func TestT505_SetTreasury(t *testing.T) {
	pool := setup(t)
	keepAnvilConfig(t, pool)
	mustCasctl(t, "tenant", "create", "cas-platform")
	_, first := randomAddress(t)
	_, second := randomAddress(t)
	id1 := insertConnection(t, pool, "cas-platform", "anvil", first)
	id2 := insertConnection(t, pool, "cas-platform", "anvil", second)
	before := anvilConfig(t, pool)
	audit := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	auditBefore := audit() // TENANT_CREATED

	r := mustCasctl(t, "source", "set-treasury", "anvil", id1)
	want := "Source anvil, treasury_connection: previous unset, new " + id1 + "\n" +
		"Source anvil, treasury_address: previous unset, new " + first + "\n"
	if r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}
	r = mustCasctl(t, "source", "set-treasury", "anvil", id2)
	want = "Source anvil, treasury_connection: previous " + id1 + ", new " + id2 + "\n" +
		"Source anvil, treasury_address: previous " + first + ", new " + second + "\n"
	if r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}

	got := anvilConfig(t, pool)
	if string(got["treasury_connection"]) != `"`+id2+`"` || string(got["treasury_address"]) != `"`+second+`"` {
		t.Errorf("config %v, want the second connection and its address", got)
	}
	for k, v := range before {
		if string(got[k]) != string(v) {
			t.Errorf("config %s = %s, was %s", k, got[k], v)
		}
	}
	if len(got) != len(before)+2 {
		t.Errorf("config %v, want the keys before and two more", got)
	}
	if n := audit() - auditBefore; n != 0 {
		t.Errorf("%d audit rows written by set-treasury, want 0", n)
	}
}

// S3-T506 — Req: SRS — Core EC-122. set-treasury refuses with a message and changes nothing: an unknown source, an
// unknown or malformed connection ID, a connection of another source, an exchange connection, a source of kind
// EXCHANGE.
func TestT506_SetTreasuryRefusals(t *testing.T) {
	pool := setup(t)
	keepAnvilConfig(t, pool)
	mustCasctl(t, "source", "add-fake")
	mustCasctl(t, "tenant", "create", "cas-platform")
	_, address := randomAddress(t)
	_, other := randomAddress(t)
	onAnvil := insertConnection(t, pool, "cas-platform", "anvil", address)
	onSepolia := insertConnection(t, pool, "cas-platform", "base-sepolia", other)
	exchange := insertConnection(t, pool, "cas-platform", "fake", "account-1")
	snapshot := databaseText(t, pool)
	config := anvilConfig(t, pool)

	for name, c := range map[string]struct {
		args    []string
		message string
	}{
		"unknown source":             {[]string{"polygon", onAnvil}, "no source with this code"},
		"unknown connection":         {[]string{"anvil", "0b0b0b0b-0000-4000-8000-00000000dead"}, "no such connection"},
		"malformed connection ID":    {[]string{"anvil", "not-a-uuid"}, "must be a UUID"},
		"connection ID without dash": {[]string{"anvil", strings.ReplaceAll(onAnvil, "-", "")}, "must be a UUID"},
		"connection of base-sepolia": {[]string{"anvil", onSepolia}, "belongs to another source"},
		"exchange connection":        {[]string{"anvil", exchange}, "not of kind EVM_WALLET"},
		"source of kind EXCHANGE":    {[]string{"fake", exchange}, "not of kind EVM"},
		"one argument":               {[]string{"anvil"}, "accepts 2 arg"},
	} {
		r := casctl(t, append([]string{"source", "set-treasury"}, c.args...)...)
		if r.code == 0 || !strings.Contains(r.stderr, c.message) || r.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want an error with %q", name, r.code, r.stdout, r.stderr, c.message)
		}
	}
	if got := databaseText(t, pool); got != snapshot {
		t.Error("a refused change changed the database")
	}
	if got := anvilConfig(t, pool); len(got) != len(config) || got["treasury_connection"] != nil {
		t.Errorf("config of anvil changed: %v", got)
	}
}
