package binance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Flags of the tests the owner runs (X1 D-20, X1 D-21): never set by make check. make binance-live sets -live,
// make fixtures-record-binance sets -record.
var (
	live   = flag.Bool("live", false, "run the live tests on the Binance test network")
	record = flag.Bool("record", false, "write the fixtures of testdata/fixtures/binance/ from the Binance test network")
)

// Environment of the live and recording tests: the test-network key in the ignored .env, loaded by make.
const (
	envTestnetKey    = "BINANCE_TESTNET_API_KEY"
	envTestnetSecret = "BINANCE_TESTNET_API_SECRET"
	envTestnetURL    = "BINANCE_TESTNET_URL"
	defaultTestnet   = "https://testnet.binance.vision"
)

// fixturesDir is testdata/fixtures/binance/ of the repository, seen from this package.
var fixturesDir = filepath.Join("..", "..", "..", "testdata", "fixtures", "binance")

// accountAnswer is the part of the account answer the tests of st2 read.
type accountAnswer struct {
	UID      *json.Number      `json:"uid"`
	Balances []json.RawMessage `json:"balances"`
}

func parseAccount(t *testing.T, body []byte) accountAnswer {
	t.Helper()
	var a accountAnswer
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&a); err != nil {
		t.Fatalf("the account answer is not JSON (%d bytes)", len(body))
	}
	return a
}

// checkFixture fails the test when the file at path holds one of the forbidden strings, or a header or member a
// fixture must not have.
func checkFixture(t *testing.T, path string, forbidden ...string) httpfixture.File {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(forbidden, "timestamp", "signature", "X-MBX-APIKEY", "http://", "https://") {
		if s != "" && strings.Contains(string(data), s) {
			t.Errorf("%s holds a forbidden value (%d characters)", filepath.Base(path), len(s))
		}
	}
	f, err := httpfixture.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// X1-T110 — Req: FR-217, SRS — Binance §2.6; X1 D-13, X1 P-6. Offline form: a signed account call of the connector
// through the recorder, against a fake upstream that demands a marker key in X-MBX-APIKEY and checks the signature,
// called through a URL with a host. The file holds method, path, the query without timestamp and signature, status,
// the used-weight headers and the body with uid replaced; no host, no key, no signature.
func TestT110_RecordSignedAccount(t *testing.T) {
	key := testKey(t)
	const realUID = 424242424242
	var signatures []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-MBX-USED-WEIGHT-1M", "21")
		w.Header().Set("X-MBX-UUID", "a-request-id")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/time":
			fmt.Fprintf(w, `{"serverTime":%d}`, t0.UnixMilli())
			return
		case "/api/v3/account":
		default:
			http.NotFound(w, r)
			return
		}
		if r.Header.Get(headerAPIKey) != key.APIKey.Value() {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`)
			return
		}
		payload, sig, _ := strings.Cut(r.URL.RawQuery, "&signature=")
		signatures = append(signatures, sig)
		if sig != signature(key.APISecret.Value(), payload) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code":-1022,"msg":"Signature for this request is not valid."}`)
			return
		}
		fmt.Fprintf(w, `{"makerCommission":10,"canTrade":true,"accountType":"SPOT","balances":[{"asset":"BTC","free":"0.00100000","locked":"0.00000000"}],"permissions":["SPOT"],"uid":%d}`, realUID)
	}))
	defer upstream.Close()

	rec := &httpfixture.Recorder{}
	h := newHarness(t, upstream.URL, nil)
	h.s.key = key
	h.c.client.Transport = rec
	body, err := h.account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a := parseAccount(t, body); a.UID == nil || a.UID.String() != strconv.Itoa(realUID) {
		t.Fatalf("the connector must get the answer as received, uid included")
	}
	bad := mustSession(t, h.c, source(`{"base_url": "`+upstream.URL+`"}`), &connector.ExchangeKey{
		APIKey: vault.NewSecret("fictitious" + randomHex(t, 16)), APISecret: key.APISecret}, h.lim)
	if _, err := bad.call(context.Background(), endpointAccount, param{"omitZeroBalances", "true"}); !errors.Is(err, connector.ErrKeyRejected) {
		t.Fatalf("fictitious key: %v; want key rejected", err)
	}

	f, err := rec.File("Signed account call, then the same call with a key the upstream rejects")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "account.json")
	if err := httpfixture.Write(path, f); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	f = checkFixture(t, path, append([]string{key.APIKey.Value(), key.APISecret.Value(), "markerkey", "markersecret",
		"fictitious", upstream.URL, u.Host, u.Port(), "127.0.0.1", "X-MBX-UUID", strconv.Itoa(realUID)}, signatures...)...)

	if len(f.Calls) != 3 {
		t.Fatalf("calls = %+v; want time, account, account", f.Calls)
	}
	for i, want := range []struct {
		path, query string
		status      int
	}{{"/api/v3/time", "", 200}, {"/api/v3/account", accountQuery, 200}, {"/api/v3/account", accountQuery, 401}} {
		c := f.Calls[i]
		if c.Method != "GET" || c.Path != want.path || c.Query != want.query || c.HTTPStatus != want.status ||
			len(c.Headers) != 1 || c.Headers["X-MBX-USED-WEIGHT-1M"] != "21" {
			t.Errorf("call %d = %+v", i, c)
		}
	}
	if a := parseAccount(t, f.Calls[1].Body); a.UID == nil || a.UID.String() != strconv.Itoa(httpfixture.FictitiousUID) || len(a.Balances) != 1 {
		t.Errorf("recorded account body: uid %v, %d balances; want the fictitious uid", a.UID, len(a.Balances))
	}

	t.Run("replayed", func(t *testing.T) {
		srv := httpfixture.ServeFile(t, path, weights)
		h := newHarness(t, srv.URL(), nil)
		body, err := h.account(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if a := parseAccount(t, body); a.UID == nil || a.UID.String() != strconv.Itoa(httpfixture.FictitiousUID) {
			t.Errorf("replayed uid %v", a.UID)
		}
		if _, err := h.account(context.Background()); !errors.Is(err, connector.ErrKeyRejected) {
			t.Errorf("replayed 401: %v", err)
		}
		srv.AssertAllServed()
	})
}

// testnet returns the base URL and the key of the test network from the environment, for a test the owner runs with
// an explicit flag. It refuses the production base URL.
func testnet(t *testing.T) (string, *connector.ExchangeKey) {
	t.Helper()
	key, secret := os.Getenv(envTestnetKey), os.Getenv(envTestnetSecret)
	if key == "" || secret == "" {
		t.Fatalf("%s and %s must be set in .env: the key of the test network", envTestnetKey, envTestnetSecret)
	}
	base := os.Getenv(envTestnetURL)
	if base == "" {
		base = defaultTestnet
	}
	u, ok := baseURL(base)
	if !ok || u == DefaultBaseURL || !strings.HasPrefix(u, "https://") {
		t.Fatalf("%s must be an https base URL of the test network, never %s", envTestnetURL, DefaultBaseURL)
	}
	return u, &connector.ExchangeKey{APIKey: vault.NewSecret(key), APISecret: vault.NewSecret(secret)}
}

// X1-T110 — Req: FR-217, SRS — Binance §2.6; X1 D-13, X1 D-21. Records the committed fixtures of
// testdata/fixtures/binance/ from the test network through the recorder: time.json, account.json,
// error_bad_signature.json (account signed with a wrong secret), error_bad_key.json (account with a fictitious key).
// Only make fixtures-record-binance runs it (-record); the owner runs it.
func TestT110_RecordFromTestnet(t *testing.T) {
	if !*record {
		t.Skip("records from the Binance test network: make fixtures-record-binance")
	}
	base, key := testnet(t)
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := source(`{"base_url": "` + base + `"}`)
	wrongSecret := vault.NewSecret(randomHex(t, 32))
	fictitiousKey := vault.NewSecret(randomHex(t, 32))
	var realUID string

	cases := []struct {
		file, description string
		key               *connector.ExchangeKey
		run               func(s *session) error
	}{
		{"time.json", "GET /api/v3/time of the test network", nil,
			func(s *session) error { _, err := s.call(ctx, endpointTime); return err }},
		{"account.json", "Signed GET /api/v3/account with omitZeroBalances=true on the test network: the time first, then the account; uid replaced",
			key, func(s *session) error {
				body, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"})
				if err == nil {
					a := parseAccount(t, body)
					if a.UID == nil || a.UID.String() == "" {
						return errors.New("the account answer has no uid")
					}
					realUID = a.UID.String()
				}
				return err
			}},
		{"error_bad_signature.json", "GET /api/v3/account signed with a wrong secret on the test network: the time first, then -1022",
			&connector.ExchangeKey{APIKey: key.APIKey, APISecret: wrongSecret}, func(s *session) error {
				_, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"})
				if !errors.Is(err, connector.ErrKeyRejected) {
					return fmt.Errorf("want key rejected, got %v", err)
				}
				return nil
			}},
		{"error_bad_key.json", "GET /api/v3/account with a fictitious key on the test network: the time first, then the key rejected",
			&connector.ExchangeKey{APIKey: fictitiousKey, APISecret: wrongSecret}, func(s *session) error {
				_, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"})
				if !errors.Is(err, connector.ErrKeyRejected) {
					return fmt.Errorf("want key rejected, got %v", err)
				}
				return nil
			}},
	}
	for _, c := range cases {
		rec := &httpfixture.Recorder{}
		conn := New(nil, nil) // a new connector per file: each file starts with its own read of the time
		conn.client.Transport = rec
		if err := c.run(mustSession(t, conn, src, c.key, realLimiter(t, conn, src))); err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		f, err := rec.File(c.description)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fixturesDir, c.file)
		if err := httpfixture.Write(path, f); err != nil {
			t.Fatal(err)
		}
		host, _ := url.Parse(base)
		checkFixture(t, path, key.APIKey.Value(), key.APISecret.Value(), wrongSecret.Value(), fictitiousKey.Value(), realUID,
			host.Host)
		t.Logf("%s: %d calls", c.file, len(f.Calls))
	}

	t.Run("account.json replayed", func(t *testing.T) {
		srv := httpfixture.ServeFile(t, filepath.Join(fixturesDir, "account.json"), weights)
		h := newHarness(t, srv.URL(), nil)
		body, err := h.account(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if a := parseAccount(t, body); a.UID == nil || a.UID.String() != strconv.Itoa(httpfixture.FictitiousUID) {
			t.Error("the replayed uid is not the fictitious one")
		}
		srv.AssertAllServed()
	})
}
