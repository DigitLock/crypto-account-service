// Phase 3 of docs/test-plan-x1.md, the periodic key check of the Binance connector (X1-T309, X1-T310), and X1-T210:
// the engine and CreateConnection on one limiter of the source. The Binance connector runs against the fake
// Binance server of internal/httpfixture on the fixtures of testdata/fixtures/binance/. The names carry the X1 row.
package engine_test

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/binance"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// binanceFixtures is testdata/fixtures/binance/ of the repository, seen from this package.
var binanceFixtures = filepath.Join("..", "..", "testdata", "fixtures", "binance")

// calls returns the calls of a fixture file from index from on.
func calls(t *testing.T, name string, from int) []httpfixture.Call {
	t.Helper()
	f, err := httpfixture.Read(filepath.Join(binanceFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return f.Calls[from:]
}

// withBinance registers the Binance connector in the set of the harness and points base_url of the seeded source
// binance at baseURL, with the extra values of config; the config is restored after the test.
func (h *harness) withBinance(t *testing.T, baseURL, extra string) {
	t.Helper()
	h.set.Register(binance.Code, binance.New(nil, h.logger))
	var before []byte
	if err := h.owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'binance'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := h.owner.Exec(ctx, `UPDATE sources SET config = $1 WHERE code = 'binance'`, before); err != nil {
			t.Errorf("restore the config of binance: %v", err)
		}
	})
	h.exec(t, `UPDATE sources SET config = config || jsonb_build_object('base_url', $1::text) || $2::jsonb
		WHERE code = 'binance'`, baseURL, "{"+extra+"}")
}

// createBinance creates a Binance connection with a key generated at run time, at the time of the clock.
func (h *harness) createBinance(t *testing.T) (uuid.UUID, key, error) {
	t.Helper()
	k := key{apiKey: hex.EncodeToString(randomBytes(t, 16)), apiSecret: hex.EncodeToString(randomBytes(t, 32))}
	c, err := h.conns.Create(ctx, registry.CreateInput{
		TenantID: h.tenantID, CredentialID: h.credentialID, OwnerRef: "owner-1", Source: binance.Code,
		Credentials: connector.Credentials{ExchangeKey: &connector.ExchangeKey{
			APIKey: vault.NewSecret(k.apiKey), APISecret: vault.NewSecret(k.apiSecret),
		}},
	})
	return c.ID, k, err
}

// parkBalances moves the balance stream of a connection far into the future: the rows of the key check do not run
// it.
func (h *harness) parkBalances(t *testing.T, id uuid.UUID) {
	t.Helper()
	h.exec(t, `UPDATE sync_cursors SET next_run_at = next_run_at + interval '100 years' WHERE connection_id = $1`, id)
}

// invalidAudits counts the CREDENTIALS_INVALID rows of a connection.
func (h *harness) invalidAudits(t *testing.T, id uuid.UUID) int {
	t.Helper()
	return h.count(t, `SELECT count(*) FROM audit_log WHERE object_id = $1 AND action = 'CREDENTIALS_INVALID'`, id.String())
}

// assertKeyNotLogged: the key and the secret appear in no log line of the engine and the connector.
func assertKeyNotLogged(t *testing.T, h *harness, k key) {
	t.Helper()
	if out := h.log.String(); strings.Contains(out, k.apiKey) || strings.Contains(out, k.apiSecret) {
		t.Error("the log contains the key or the secret")
	}
}

// X1-T309 — Req: FR-203, EC-203, EC-204; Core FR-119, EC-108, EC-116. The periodic key check of a Binance
// connection: a key rejected at the next check, and a key that gained a permission, become CREDENTIALS_INVALID
// with an audit row and its reason; a key that stays read-only moves permissions_checked_at and writes no audit row.
func TestT309_PeriodicCheckStopsAKey(t *testing.T) {
	for _, c := range []struct {
		name        string
		check       []httpfixture.Call
		status      string
		reason      string
		permissions []any
	}{
		{"rejected", calls(t, "key_rejected.json", 1), "CREDENTIALS_INVALID", "key_rejected", nil},
		{"gained a permission", calls(t, "key_not_read_only.json", 1), "CREDENTIALS_INVALID", "key_not_read_only",
			[]any{"enableWithdrawals"}},
		{"stays read-only", calls(t, "key_read_only.json", 1), "ACTIVE", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := setup(t)
			h.cfg.KeyCheckInterval = time.Hour // short: KEY_CHECK_INTERVAL
			srv := httpfixture.Serve(t, httpfixture.File{Description: c.name,
				Calls: append(calls(t, "key_read_only.json", 0), c.check...)}, nil)
			h.withBinance(t, srv.URL(), "")
			id, k, err := h.createBinance(t)
			if err != nil {
				t.Fatal(err)
			}
			h.parkBalances(t, id)
			e := h.engine(h.server)
			h.pass(t, e) // the check is not due yet
			if srv.Served() != 3 {
				t.Fatalf("%d requests before the check is due, want the 3 of the creation", srv.Served())
			}
			h.clock.Set(testNow.Add(time.Hour))
			h.pass(t, e)
			srv.AssertAllServed()

			if got := h.status(t, id); got != c.status {
				t.Errorf("status %s, want %s", got, c.status)
			}
			if c.reason != "" {
				assertInvalidAudit(t, h, id, c.reason, c.permissions)
			} else {
				if at := h.checkedAt(t, id); at == nil || !at.Equal(testNow.Add(time.Hour)) {
					t.Errorf("permissions_checked_at = %v, want %v", at, testNow.Add(time.Hour))
				}
				if n := h.invalidAudits(t, id); n != 0 {
					t.Errorf("%d audit rows of a key that stays read-only", n)
				}
			}
			if c.reason != "" {
				h.clock.Advance(3 * time.Hour)
				h.pass(t, e)
				if srv.Served() != len(calls(t, "key_read_only.json", 0))+len(c.check) {
					t.Error("a stopped connection was checked again")
				}
			}
			assertKeyNotLogged(t, h, k)
		})
	}
}

// X1-T310 — Req: Core EC-118. The periodic check cannot reach Binance: 429 with Retry-After 300, then 500, then
// 403. The status stays and permissions_checked_at too; the next check comes by the backoff and not before the end
// of the pause; a read-only answer then moves permissions_checked_at.
func TestT310_PeriodicCheckCannotReachBinance(t *testing.T) {
	h := setup(t)
	restrictions := calls(t, "key_rejected.json", 1)[0] // apiRestrictions of a check; its answer is replaced below
	answer := func(status int, headers map[string]string) httpfixture.Call {
		c := restrictions
		c.HTTPStatus, c.Headers, c.Body = status, headers, nil
		return c
	}
	f := httpfixture.File{Description: "429, 500, 403, then read-only", Calls: append(append(calls(t, "key_read_only.json", 0),
		answer(429, map[string]string{"Retry-After": "300"}),
		answer(500, nil),
		answer(403, nil)),
		calls(t, "key_read_only.json", 1)...)}
	srv := httpfixture.Serve(t, f, nil)
	h.withBinance(t, srv.URL(), "")
	id, k, err := h.createBinance(t)
	if err != nil {
		t.Fatal(err)
	}
	h.parkBalances(t, id)
	e := h.engine(h.server)
	start := testNow.Add(24 * time.Hour)

	for _, step := range []struct {
		at     time.Duration
		served int
	}{
		{0, 4},                              // 429: next attempt after the pause of 5 min, longer than the backoff of 30 s
		{4*time.Minute + 59*time.Second, 4}, // the pause runs
		{5 * time.Minute, 5},                // 500: backoff 60 s
		{5*time.Minute + 59*time.Second, 5},
		{6 * time.Minute, 6}, // 403: backoff 120 s
		{7*time.Minute + 59*time.Second, 6},
	} {
		h.clock.Set(start.Add(step.at))
		h.pass(t, e)
		if got := srv.Served(); got != step.served {
			t.Fatalf("at +%v: %d requests, want %d", step.at, got, step.served)
		}
		if h.status(t, id) != "ACTIVE" || !h.checkedAt(t, id).Equal(testNow) || h.invalidAudits(t, id) != 0 {
			t.Fatalf("at +%v: status %s, checked at %v; want ACTIVE, unchanged, no audit row", step.at, h.status(t, id), h.checkedAt(t, id))
		}
	}
	h.clock.Set(start.Add(8 * time.Minute))
	h.pass(t, e)
	srv.AssertAllServed()
	if at := h.checkedAt(t, id); h.status(t, id) != "ACTIVE" || at == nil || !at.Equal(start.Add(8*time.Minute)) {
		t.Errorf("after the read-only answer: status %s, checked at %v", h.status(t, id), at)
	}
	if n := strings.Count(h.log.String(), "a WAF rule of Binance"); n != 1 {
		t.Errorf("%d WARN lines of the 403, want 1", n)
	}
	if h.rec.snapshot().rateLimited != 1 {
		t.Errorf("rate-limit answers reported to the engine metrics = %d, want 1", h.rec.snapshot().rateLimited)
	}
	assertKeyNotLogged(t, h, k)
}

// X1-T210 — Req: Core FR-110; X1 D-29. A connection is created while the engine runs the key check of another
// connection: both reserve in the one limiter of the source of the process. The budgets are small (budget_share
// 0.02: api 120, each sapi: budget 240), so the reservations of both callers in the current window are seen in the
// limiter the set returns.
func TestT210_OneLimiterPerSource(t *testing.T) {
	h := setup(t)
	var mu sync.Mutex
	uids := map[string]string{}
	restrictions := map[string]int{}
	checking, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-MBX-APIKEY")
		mu.Lock()
		if _, ok := uids[k]; !ok && k != "" {
			uids[k] = fmt.Sprint(100000001 + len(uids))
		}
		uid := uids[k]
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/time":
			mu.Unlock()
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
		case "/sapi/v1/account/apiRestrictions":
			restrictions[k]++
			blocks := uid == "100000001" && restrictions[k] == 2 // the periodic check of the first connection
			mu.Unlock()
			if blocks {
				close(checking)
				<-release
			}
			fmt.Fprint(w, `{"ipRestrict":false,"enableReading":true,"enableWithdrawals":false}`)
		case "/api/v3/account":
			mu.Unlock()
			fmt.Fprintf(w, `{"uid":%s,"balances":[]}`, uid)
		default:
			mu.Unlock()
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h.withBinance(t, srv.URL, `"budget_share": 0.02`)

	a, _, err := h.createBinance(t)
	if err != nil {
		t.Fatal(err)
	}
	h.parkBalances(t, a)
	h.clock.Set(testNow.Add(24 * time.Hour)) // a new window of every budget; the check of a is due
	e := h.engine(h.server)
	passed := make(chan error, 1)
	go func() { passed <- e.RunPass(ctx) }()
	select {
	case <-checking:
	case <-time.After(5 * time.Second):
		t.Fatal("the engine did not start the key check")
	}
	b, _, err := h.createBinance(t) // while the check of a waits for its answer
	close(release)
	if err != nil {
		t.Fatalf("create during the key check: %v", err)
	}
	if err := <-passed; err != nil {
		t.Fatal(err)
	}
	if at := h.checkedAt(t, a); h.status(t, a) != "ACTIVE" || h.status(t, b) != "ACTIVE" || at == nil || !at.Equal(h.clock.Now()) {
		t.Fatalf("a %s checked at %v, b %s; want both ACTIVE and the check of a stored", h.status(t, a), at, h.status(t, b))
	}

	// In the current window: the check of a and the creation of b, 20 + 20 in api and 1 + 1 in apiRestrictions.
	src := connector.Source{Code: binance.Code, Kind: connector.KindExchange, Enabled: true,
		Config: []byte(`{"budget_share": 0.02}`)}
	conn, _ := h.set.For(src)
	lim, err := h.limiters.For(src, conn)
	if err != nil {
		t.Fatal(err)
	}
	bounded := lim.WithMaxWait(10 * time.Millisecond)
	sapi := binance.SAPIBudget("/sapi/v1/account/apiRestrictions")
	for _, c := range []struct {
		budget    string
		left      int
		reachable bool
	}{{binance.BudgetAPI, 120 - 40, true}, {sapi, 240 - 2, true}} {
		if err := bounded.Reserve(ctx, c.budget, c.left); err != nil {
			t.Errorf("%s: %d units left expected: %v", c.budget, c.left, err)
		}
		if err := bounded.Reserve(ctx, c.budget, 1); !errors.Is(err, limiter.ErrNoBudget) {
			t.Errorf("%s: one more unit: %v; want ErrNoBudget: the engine and CreateConnection reserve in one limiter", c.budget, err)
		}
	}
	if got := slices.Sorted(func(yield func(string) bool) {
		mu.Lock()
		defer mu.Unlock()
		for _, u := range uids {
			if !yield(u) {
				return
			}
		}
	}); !slices.Equal(got, []string{"100000001", "100000002"}) {
		t.Errorf("uids = %v", got)
	}
}
