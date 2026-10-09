package binance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// weights is the weight table of the endpoints of st2 for the fake server (SRS — Binance §2.1.2).
var weights = httpfixture.Weights{
	"GET /api/v3/time":    {Budget: BudgetAPI, Weight: 1},
	"GET /api/v3/account": {Budget: BudgetAPI, Weight: 20},
}

// t0 is the local clock of a test at its start: a fixed point, so the time offset is exact.
var t0 = time.UnixMilli(1790000000000)

func source(config string) connector.Source {
	return connector.Source{Code: "binance", Kind: connector.KindExchange, Enabled: true, Config: json.RawMessage(config)}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// testKey is a key and a secret generated at run time, with marker prefixes a test can search for.
func testKey(t *testing.T) *connector.ExchangeKey {
	t.Helper()
	return &connector.ExchangeKey{
		APIKey:    vault.NewSecret("markerkey" + randomHex(t, 24)),
		APISecret: vault.NewSecret("markersecret" + randomHex(t, 24)),
	}
}

// fakeClock is the local clock of a test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingLimits records the calls of the limiter seam.
type recordingLimits struct {
	mu       sync.Mutex
	reserved []string
	paused   []time.Duration
}

func (l *recordingLimits) reserve(_ context.Context, ep endpoint) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserved = append(l.reserved, ep.path)
	return nil
}

func (l *recordingLimits) observe(endpoint, http.Header) {}

func (l *recordingLimits) pause(_ endpoint, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = append(l.paused, d)
}

func (l *recordingLimits) pauses() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Duration(nil), l.paused...)
}

// syncBuffer is a log sink that handlers of several goroutines may write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// harness is a connector on a fake clock with a debug log in a buffer, and a session with a test key on baseURL.
type harness struct {
	c     *Connector
	s     *session
	clock *fakeClock
	log   *syncBuffer
	lim   *recordingLimits
}

func newHarness(t *testing.T, baseURL string, metrics Metrics) *harness {
	t.Helper()
	log := &syncBuffer{}
	c := New(metrics, slog.New(slog.NewJSONHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	clock := &fakeClock{t: t0}
	c.now = clock.now
	lim := &recordingLimits{}
	s := c.session(source(`{"base_url": "`+baseURL+`"}`), testKey(t))
	s.lim = lim
	return &harness{c: c, s: s, clock: clock, log: log, lim: lim}
}

// timeCall is the answer of GET /api/v3/time with the server time at ms.
func timeCall(ms int64) httpfixture.Call {
	body, _ := json.Marshal(map[string]int64{"serverTime": ms})
	return httpfixture.Call{Method: "GET", Path: "/api/v3/time", HTTPStatus: 200, Body: body,
		Headers: map[string]string{"X-MBX-USED-WEIGHT-1M": "1"}}
}

// accountQuery is the query of the signed account call without timestamp and signature.
const accountQuery = "omitZeroBalances=true&recvWindow=5000"

func accountCall(status int, body string) httpfixture.Call {
	c := httpfixture.Call{Method: "GET", Path: "/api/v3/account", Query: accountQuery, HTTPStatus: status}
	if body != "" {
		c.Body = json.RawMessage(body)
	}
	return c
}

const accountOK = `{"uid":100000001,"accountType":"SPOT","balances":[{"asset":"BTC","free":"0.00100000","locked":"0.00000000"}]}`

func (h *harness) account(ctx context.Context) ([]byte, error) {
	return h.s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"})
}
