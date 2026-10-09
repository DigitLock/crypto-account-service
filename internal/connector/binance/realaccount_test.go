package binance

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// realAccount is set only by make binance-real (X1 D-43): the owner runs it, never make check.
var realAccount = flag.Bool("real", false, "run the dry run on the owner's real Binance account")

// Environment of the dry run: the read-only key of the real account in the ignored .env, loaded by make.
const (
	envRealKey    = "BINANCE_API_KEY"
	envRealSecret = "BINANCE_API_SECRET"
)

// X1-T601, X1-T605, X1-T607 — Req: package §5; issues 2 and 6; §2.1.1 Rate limits; X1 D-4, X1 D-20, X1 D-43. The dry
// run on the owner's real account with the production base URL: the key check and the snapshot once, nothing
// stored, no connection created. Only make binance-real runs it (-real); the owner runs it. It prints the report of
// the dry run: statuses, counts, field names and used-weight headers; never a uid, an asset code, an amount, the
// key, a signature or a URL.
func TestT601_RealAccountDryRun(t *testing.T) {
	if !*realAccount {
		t.Skip("dry run on the real Binance account: make binance-real")
	}
	key, secret := os.Getenv(envRealKey), os.Getenv(envRealSecret)
	if key == "" || secret == "" {
		t.Fatalf("%s and %s must be set in .env: the read-only key of the real account", envRealKey, envRealSecret)
	}
	src := source(`{}`) // the production base URL of the source binance
	if ParseConfig(src).BaseURL != DefaultBaseURL {
		t.Fatal("the dry run runs on the production base URL of the source binance only")
	}
	aliases, err := migrationAliases()
	if err != nil {
		t.Fatal(err)
	}
	src.Aliases = aliases
	c := New(nil, nil)
	d := runDryRun(context.Background(), c, src,
		&connector.ExchangeKey{APIKey: vault.NewSecret(key), APISecret: vault.NewSecret(secret)}, realLimiter(t, c, src))
	for _, line := range d.lines() {
		t.Log(line)
	}
}

// dryRunFixture is the key check of key_read_only.json and the snapshot of snapshot_full.json, with an unknown
// permission flag, a non-boolean permits field, marker assets and amounts: a wrapper LDMRKQ of the flexible position
// MRKQ with the same amount, a marker asset MRKASSETQ and an LD code without a position LDZZZQ.
func dryRunFixture(t *testing.T) httpfixture.File {
	t.Helper()
	read := fixture(t, "key_read_only.json").Calls
	snap := fixture(t, "snapshot_full.json").Calls[1:]
	var restrictions map[string]any
	if err := json.Unmarshal(read[1].Body, &restrictions); err != nil {
		t.Fatal(err)
	}
	restrictions["enableNewThing"] = false
	restrictions["permitsLevel"] = 3
	read[1] = setBody(t, read[1], restrictions)
	snap[0] = spotWith(t, httpfixture.File{Calls: append([]httpfixture.Call{read[0]}, snap...)},
		[3]string{"BTC", "0.00100000", "0"}, [3]string{"LDMRKQ", "987654.32100000", "0"},
		[3]string{"MRKASSETQ", "987654.321", "0"}, [3]string{"LDZZZQ", "2", "0.5"})
	snap[2] = flexibleWith(t, snap[2], 2, [2]string{"USDT", "10"}, [2]string{"MRKQ", "987654.321"})
	return httpfixture.File{Description: "dry run", Calls: append(read, snap...)}
}

// X1-T601, the report — Req: X1 D-43; P-1 option B. The report of the dry run on the fixtures with marker assets and
// amounts: the statuses of the six endpoints, the time offset, the key check with ip_restricted, the unknown and the
// non-boolean permission fields by name, the counts per account type, the LD counts of issue 2, the count of assets
// without an alias row, the used-weight headers of the /sapi endpoints. No marker, uid, asset code, amount, key,
// signature or URL appears in it.
func TestT601_DryRunReport(t *testing.T) {
	aliases, err := migrationAliases()
	if err != nil || len(aliases) != 855 {
		t.Fatalf("%d alias rows of migration 000010: %v; want 855", len(aliases), err)
	}
	srv := httpfixture.Serve(t, dryRunFixture(t), weights)
	h := newHarness(t, srv.URL(), nil)
	src := source(`{"base_url": "` + srv.URL() + `"}`)
	src.Aliases = aliases
	d := runDryRun(context.Background(), h.c, src, h.s.key, h.lim)
	srv.AssertAllServed()
	got := d.lines()
	want := []string{
		"dry run of the key check and the snapshot: nothing stored, no connection created",
		"endpoint GET /api/v3/time: 200",
		"endpoint GET /api/v3/account: 200",
		"endpoint GET /sapi/v1/account/apiRestrictions: 200",
		"endpoint POST /sapi/v1/asset/get-funding-asset: 200",
		"endpoint GET /sapi/v1/simple-earn/flexible/position: 200",
		"endpoint GET /sapi/v1/simple-earn/locked/position: 200",
		"time offset (local minus Binance): " + strconv.FormatInt(d.offset.Milliseconds(), 10) + " ms",
		"key check: accepted, permissions READ",
		"ip_restricted: false",
		"unknown permission flags: enableNewThing",
		"enable/permits fields that are not booleans: permitsLevel",
		"snapshot: complete",
		"assets per account type: SPOT 3, FUNDING 1, EARN_FLEXIBLE 2, EARN_LOCKED 2",
		"spot codes starting with LD: 2; with a flexible position of the rest of the code: 1; of them with an amount equal to that position: 1",
		"assets without an alias row: 3",
		"used-weight headers of GET /sapi/v1/account/apiRestrictions: X-SAPI-USED-IP-WEIGHT-1M=1",
		"used-weight headers of POST /sapi/v1/asset/get-funding-asset: X-SAPI-USED-IP-WEIGHT-1M=1",
		"used-weight headers of GET /sapi/v1/simple-earn/flexible/position: X-SAPI-USED-IP-WEIGHT-1M=1",
		"used-weight headers of GET /sapi/v1/simple-earn/locked/position: X-SAPI-USED-IP-WEIGHT-1M=1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("report =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	text := strings.Join(got, "\n")
	for _, s := range []string{"MRK", "ZZZQ", "987654", "0.001", "BTC", "USDT", "AXS", "DOT", "100000001", "http", "127.0.0.1",
		"signature", h.s.key.APIKey.Value(), h.s.key.APISecret.Value(), "markerkey", "markersecret"} {
		if strings.Contains(text, s) {
			t.Errorf("the report shows %q", s)
		}
	}

	t.Run("a key that is not read-only: no snapshot", func(t *testing.T) {
		srv := httpfixture.Serve(t, fixture(t, "key_not_read_only.json"), weights)
		h := newHarness(t, srv.URL(), nil)
		d := runDryRun(context.Background(), h.c, source(`{"base_url": "`+srv.URL()+`"}`), h.s.key, h.lim)
		srv.AssertAllServed()
		text := strings.Join(d.lines(), "\n")
		for _, want := range []string{"key check: not read-only: enableWithdrawals", "snapshot: not run, the key check did not pass",
			"endpoint GET /api/v3/account: not called", "assets per account type: SPOT 0, FUNDING 0, EARN_FLEXIBLE 0, EARN_LOCKED 0"} {
			if !strings.Contains(text, want) {
				t.Errorf("report lacks %q:\n%s", want, text)
			}
		}
	})
}
