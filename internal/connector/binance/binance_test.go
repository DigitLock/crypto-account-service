package binance

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
)

// X1-T102 — Req: SRS — Binance §3.1; Core §3.1. Values as stored; a missing or malformed value takes its default.
func TestT102_ConfigDefaults(t *testing.T) {
	defaults := Config{
		BaseURL: DefaultBaseURL, BudgetShare: 0.5, RecvWindowMS: 5000, TimeSyncInterval: time.Hour,
		BalancesInterval: 15 * time.Minute,
	}
	for _, c := range []struct {
		name, config string
		want         Config
	}{
		{"migration values", `{"base_url": "https://api.binance.com", "budget_share": 0.5, "recv_window_ms": 5000,
			"time_sync_interval": "1h", "sync_interval": {"balances": "15m"}}`, defaults},
		{"complete, other values", `{"base_url": "http://127.0.0.1:18080/", "budget_share": 0.25, "recv_window_ms": 60000,
			"time_sync_interval": "30m", "sync_interval": {"balances": "5m", "ledger": "2h"}}`,
			Config{BaseURL: "http://127.0.0.1:18080", BudgetShare: 0.25, RecvWindowMS: 60000, TimeSyncInterval: 30 * time.Minute,
				BalancesInterval: 5 * time.Minute}},
		{"share of 1", `{"budget_share": 1}`, func() Config { c := defaults; c.BudgetShare = 1; return c }()},
		{"empty object", `{}`, defaults},
		{"null", `null`, defaults},
		{"not JSON", `{"base_url":`, defaults},
		{"not an object", `[1, 2]`, defaults},
		{"malformed values", `{"base_url": "api.binance.com", "budget_share": "0.5", "recv_window_ms": 5000.5,
			"time_sync_interval": "an hour", "sync_interval": {"balances": "0s"}}`, defaults},
		{"out of range", `{"budget_share": 1.5, "recv_window_ms": 60001, "time_sync_interval": "-1h"}`, defaults},
		{"zero", `{"budget_share": 0, "recv_window_ms": 0, "time_sync_interval": "0s"}`, defaults},
		{"wrong JSON types", `{"base_url": 1, "budget_share": true, "recv_window_ms": "5000", "time_sync_interval": 3600,
			"sync_interval": {"balances": 900}}`, defaults},
		{"base URL with user info", `{"base_url": "https://user:pass@api.binance.com"}`, defaults},
		{"base URL with a query", `{"base_url": "https://api.binance.com?x=1"}`, defaults},
		{"base URL with a path", `{"base_url": "https://api.binance.com/api"}`, defaults},
		{"base URL of another scheme", `{"base_url": "ftp://api.binance.com"}`, defaults},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseConfig(source(c.config)); got != c.want {
				t.Errorf("ParseConfig(%s)\n got  %+v\n want %+v", c.config, got, c.want)
			}
		})
	}
}

// The HMAC example of the Binance Spot API documentation (SIGNED endpoint security, HMAC keys). A public test
// vector of the documentation: not a key of any account.
const (
	docSecret    = "NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j"
	docQuery     = "symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1&recvWindow=5000&timestamp=1499827319559"
	docSignature = "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71"
)

// X1-T103 — Req: SRS — Binance §2.1.1 Signature; X1 D-4. The documented signature; signature the last parameter;
// the parameters of a POST in the query.
func TestT103_HMACSignature(t *testing.T) {
	if got := signature(docSecret, docQuery); got != docSignature {
		t.Fatalf("signature = %s, want %s", got, docSignature)
	}
	ps := []param{{"symbol", "LTCBTC"}, {"side", "BUY"}, {"type", "LIMIT"}, {"timeInForce", "GTC"}, {"quantity", "1"}, {"price", "0.1"}}
	if got, want := signedQuery(ps, 5000, 1499827319559, docSecret), docQuery+"&signature="+docSignature; got != want {
		t.Fatalf("signedQuery =\n %s\nwant\n %s", got, want)
	}
	if got := redactSignature(docQuery + "&signature=" + docSignature); got != docQuery {
		t.Errorf("redactSignature = %s", got)
	}

	t.Run("POST parameters in the query, signature last", func(t *testing.T) {
		post := endpoint{method: http.MethodPost, path: "/sapi/v1/asset/get-funding-asset", signed: true, budget: "test", weight: 1}
		srv := httpfixture.Serve(t, httpfixture.File{Description: "signed POST", Calls: []httpfixture.Call{
			timeCall(t0.UnixMilli()),
			{Method: "POST", Path: post.path, Query: "asset=BTC&needBtcValuation=false&recvWindow=5000", HTTPStatus: 200, Body: json.RawMessage(`[]`)},
		}}, nil)
		var bodyLen int64 = -1
		var contentType string
		h := newHarness(t, srv.URL(), nil)
		h.c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				bodyLen, contentType = r.ContentLength, r.Header.Get("Content-Type")
			}
			return http.DefaultTransport.RoundTrip(r)
		})
		if _, err := h.s.call(context.Background(), post, param{"asset", "BTC"}, param{"needBtcValuation", "false"}); err != nil {
			t.Fatal(err)
		}
		srv.AssertAllServed()
		if bodyLen != 0 || contentType != "" {
			t.Errorf("POST body length %d, content type %q; want no body", bodyLen, contentType)
		}
		raw := srv.Received()[1].RawQuery
		payload, sig, ok := strings.Cut(raw, "&signature=")
		if !ok || strings.Contains(sig, "&") {
			t.Fatalf("query %s: signature is not the last parameter", raw)
		}
		if want := signature(h.s.key.APISecret.Value(), payload); !hmac.Equal([]byte(sig), []byte(want)) {
			t.Error("the signature does not cover the query as sent")
		}
		if !strings.HasPrefix(payload, "asset=BTC&needBtcValuation=false&recvWindow=5000&timestamp=") {
			t.Errorf("query %s: parameters, recvWindow, timestamp in this order", payload)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// gauge reads the value of a gauge without labels from reg.
func gauge(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("%s not found", name)
	return 0
}

// timestampOf returns the timestamp parameter of a raw query.
func timestampOf(t *testing.T, raw string) int64 {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	if err != nil {
		t.Fatalf("query %s: no timestamp", raw)
	}
	return ms
}

// X1-T104 — Req: SRS — Binance §2.1.1 Time, §2.5.1; X1 D-8. The server clock is 1500 ms ahead: the first signed
// request reads the time first (weight 1 on the /api budget), timestamp carries the offset, the time is read again
// after time_sync_interval and not before, binance_time_offset_ms = −1500.
func TestT104_TimeOffset(t *testing.T) {
	const ahead = 1500
	interval := time.Hour
	srv := httpfixture.Serve(t, httpfixture.File{Description: "server clock 1500 ms ahead", Calls: []httpfixture.Call{
		timeCall(t0.UnixMilli() + ahead),
		accountCall(200, accountOK),
		accountCall(200, accountOK),
		timeCall(t0.Add(interval).UnixMilli() + ahead),
		accountCall(200, accountOK),
	}}, weights)
	reg := prometheus.NewRegistry()
	h := newHarness(t, srv.URL(), NewPromMetrics(reg))
	ctx := context.Background()

	if _, err := h.account(ctx); err != nil {
		t.Fatal(err)
	}
	if got := srv.Received(); len(got) != 2 || got[0].Path != "/api/v3/time" || srv.Used(BudgetAPI) != 21 {
		t.Fatalf("requests %v, weight %d; want the time first, then the account: 1 + 20", got, srv.Used(BudgetAPI))
	}
	if got := h.lim.log(); !reflect.DeepEqual(got, []string{"reserve api 1", "observe api 1", "reserve api 20"}) {
		t.Errorf("limiter = %v; each request reserves first", got)
	}
	if got := gauge(t, reg, "binance_time_offset_ms"); got != -ahead {
		t.Errorf("binance_time_offset_ms = %v, want %d", got, -ahead)
	}
	if ts := timestampOf(t, srv.Received()[1].RawQuery); ts != t0.UnixMilli()+ahead {
		t.Errorf("timestamp = %d, want the local clock plus %d ms: %d", ts, ahead, t0.UnixMilli()+ahead)
	}

	// Just before time_sync_interval: no new read.
	h.clock.advance(interval - time.Millisecond)
	if _, err := h.account(ctx); err != nil {
		t.Fatal(err)
	}
	if got := srv.Received(); len(got) != 3 || got[2].Path != "/api/v3/account" {
		t.Fatalf("requests %v; the time is not read before time_sync_interval", got)
	}
	if ts := timestampOf(t, srv.Received()[2].RawQuery); ts != h.clock.now().UnixMilli()+ahead {
		t.Errorf("timestamp = %d, want %d", ts, h.clock.now().UnixMilli()+ahead)
	}

	// At time_sync_interval: read again.
	h.clock.advance(time.Millisecond)
	if _, err := h.account(ctx); err != nil {
		t.Fatal(err)
	}
	srv.AssertAllServed()
	if srv.Used(BudgetAPI) != 62 {
		t.Errorf("weight of the /api budget = %d, want 62", srv.Used(BudgetAPI))
	}

	t.Run("one offset per base URL", func(t *testing.T) {
		other := httpfixture.Serve(t, httpfixture.File{Description: "another base URL", Calls: []httpfixture.Call{
			timeCall(h.clock.now().UnixMilli() - 200),
			accountCall(200, accountOK),
		}}, weights)
		s := mustSession(t, h.c, source(`{"base_url": "`+other.URL()+`"}`), testKey(t), h.lim)
		if _, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"}); err != nil {
			t.Fatal(err)
		}
		other.AssertAllServed()
		if got := gauge(t, reg, "binance_time_offset_ms"); got != 200 {
			t.Errorf("binance_time_offset_ms = %v, want 200", got)
		}
		if got := h.c.clockOf(srv.URL()).get(); got != -ahead*time.Millisecond {
			t.Errorf("offset of the first base URL = %v, want unchanged", got)
		}
	})
}

// timestampError is the answer -1021 of Binance.
const timestampError = `{"code":-1021,"msg":"Timestamp for this request is outside of the recvWindow."}`

// X1-T105 — Req: EC-221; X1 D-3, X1 D-8. -1021 once: the time is read again and the request repeated once,
// success. -1021 twice: one repeat, then a plain failure.
func TestT105_TimestampOutsideRecvWindow(t *testing.T) {
	srv := httpfixture.Serve(t, httpfixture.File{Description: "-1021 once, then twice", Calls: []httpfixture.Call{
		timeCall(t0.UnixMilli()),
		accountCall(400, timestampError),
		timeCall(t0.UnixMilli() + 3000),
		accountCall(200, accountOK),
		accountCall(400, timestampError),
		timeCall(t0.UnixMilli() + 3000),
		accountCall(400, timestampError),
	}}, weights)
	h := newHarness(t, srv.URL(), nil)
	ctx := context.Background()

	if _, err := h.account(ctx); err != nil {
		t.Fatalf("first request: %v; want success after one repeat", err)
	}
	if got := srv.Served(); got != 4 {
		t.Fatalf("served %d, want 4: time, -1021, time, account", got)
	}
	if ts := timestampOf(t, srv.Received()[3].RawQuery); ts != t0.UnixMilli()+3000 {
		t.Errorf("timestamp of the repeat = %d, want the new offset: %d", ts, t0.UnixMilli()+3000)
	}

	_, err := h.account(ctx)
	srv.AssertAllServed()
	var rl *connector.RateLimitError
	if err == nil || errors.Is(err, connector.ErrKeyRejected) || errors.Is(err, connector.ErrUnreachable) ||
		errors.As(err, &rl) || errors.Is(err, errTimestamp) || !strings.Contains(err.Error(), "EC-221") {
		t.Errorf("second request: %v; want a plain failure", err)
	}
}

// failingServer answers every request by closing the connection, or by never answering.
func failingServer(t *testing.T, hang bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang {
			<-r.Context().Done()
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// errorCase is one answer of the table "Answers to errors".
type errorCase struct {
	name   string
	status int
	body   string
	want   error // ErrKeyRejected, ErrUnreachable, or nil for a plain failure
}

var errorCases = []errorCase{
	{"401", 401, ``, connector.ErrKeyRejected},
	{"-2015", 400, `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`, connector.ErrKeyRejected},
	{"-2014", 400, `{"code":-2014,"msg":"API-key format invalid."}`, connector.ErrKeyRejected},
	{"-1022", 400, `{"code":-1022,"msg":"Signature for this request is not valid."}`, connector.ErrKeyRejected},
	{"500", 500, ``, connector.ErrUnreachable},
	{"-1001", 400, `{"code":-1001,"msg":"Internal error; unable to process your request. Please try again."}`, connector.ErrUnreachable},
	{"-1008", 400, `{"code":-1008,"msg":"Server is currently overloaded with other requests. Please try again in a few minutes."}`, connector.ErrUnreachable},
	{"403", 403, ``, connector.ErrUnreachable},
	{"unknown code", 400, `{"code":-1100,"msg":"Illegal characters found in a parameter."}`, nil},
}

// checkError checks err against the typed error of a case: nil want is a plain failure, with no typed error.
func checkError(t *testing.T, name string, err, want error) {
	t.Helper()
	var rl *connector.RateLimitError
	typed := errors.Is(err, connector.ErrKeyRejected) || errors.Is(err, connector.ErrUnreachable) || errors.As(err, &rl)
	switch {
	case err == nil:
		t.Errorf("%s: no error", name)
	case want == nil && typed:
		t.Errorf("%s: %v; want a plain failure", name, err)
	case want != nil && !errors.Is(err, want):
		t.Errorf("%s: %v; want %v", name, err, want)
	}
}

// X1-T106 — Req: SRS — Binance §2.1.1 Answers to errors; X1 D-3. Key rejected for 401, -2015, -2014, -1022;
// unreachable for 500, -1001, -1008, a closed connection and no answer within the timeout; 403 unreachable with one
// WARN line and no pause; an unknown code a plain failure. The codes come with HTTP 400 here, so the code alone
// decides.
func TestT106_AnswersToErrors(t *testing.T) {
	calls := []httpfixture.Call{timeCall(t0.UnixMilli())}
	for _, c := range errorCases {
		calls = append(calls, accountCall(c.status, c.body))
	}
	srv := httpfixture.Serve(t, httpfixture.File{Description: "answers to errors", Calls: calls}, weights)
	h := newHarness(t, srv.URL(), nil)
	ctx := context.Background()
	for _, c := range errorCases {
		before := strings.Count(h.log.String(), `"level":"WARN"`)
		_, err := h.account(ctx)
		checkError(t, c.name, err, c.want)
		warns := strings.Count(h.log.String(), `"level":"WARN"`) - before
		if want := map[bool]int{true: 1, false: 0}[c.status == 403]; warns != want {
			t.Errorf("%s: %d WARN lines, want %d", c.name, warns, want)
		}
	}
	srv.AssertAllServed()
	if got := h.lim.pauses(); len(got) != 0 {
		t.Errorf("pauses = %v; no answer of this table pauses a budget", got)
	}
	if !strings.Contains(h.log.String(), `"msg":"binance: HTTP 403, a WAF rule of Binance: a rate limit violation or a security block"`) {
		t.Errorf("no WARN line of the 403: %s", h.log.String())
	}

	t.Run("closed connection", func(t *testing.T) {
		h := newHarness(t, failingServer(t, false), nil)
		_, err := h.s.call(ctx, endpointTime)
		checkError(t, "closed connection", err, connector.ErrUnreachable)
	})
	t.Run("no answer within the timeout", func(t *testing.T) {
		h := newHarness(t, failingServer(t, true), nil)
		h.c.timeout = 200 * time.Millisecond // CallTimeout is 10 s; the rule is the same
		start := time.Now()
		_, err := h.s.call(ctx, endpointTime)
		checkError(t, "no answer", err, connector.ErrUnreachable)
		if err == nil || !strings.Contains(err.Error(), "no answer within 200ms") || time.Since(start) > 5*time.Second {
			t.Errorf("error %v after %v; want no answer within the timeout", err, time.Since(start))
		}
		if CallTimeout != 10*time.Second || New(nil, nil).timeout != CallTimeout {
			t.Errorf("call timeout %v, want 10s", CallTimeout)
		}
	})
	t.Run("stopped from outside", func(t *testing.T) {
		h := newHarness(t, failingServer(t, true), nil)
		cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if _, err := h.s.call(cctx, endpointTime); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, connector.ErrUnreachable) {
			t.Errorf("error %v; want the error of the context, not unreachable", err)
		}
	})
	t.Run("redirect not followed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v3/time" {
				t.Errorf("the redirect was followed to %s", r.URL.Path)
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}))
		defer srv.Close()
		h := newHarness(t, srv.URL, nil)
		_, err := h.s.call(ctx, endpointTime)
		checkError(t, "redirect", err, nil)
	})
	t.Run("rate limit answers pause the budget in the limiter", func(t *testing.T) {
		srv := httpfixture.Serve(t, httpfixture.File{Description: "429 and 418", Calls: []httpfixture.Call{
			{Method: "GET", Path: "/api/v3/time", HTTPStatus: 429, Headers: map[string]string{"Retry-After": "30"}},
			{Method: "GET", Path: "/api/v3/time", HTTPStatus: 418},
		}}, weights)
		h := newHarness(t, srv.URL(), nil)
		for _, want := range []time.Duration{30 * time.Second, DefaultRateLimitPause} {
			_, err := h.s.call(ctx, endpointTime)
			var rl *connector.RateLimitError
			if !errors.As(err, &rl) || rl.Pause != want || rl.Budget != BudgetAPI {
				t.Errorf("error %v; want RateLimitError of %s for %v", err, BudgetAPI, want)
			}
		}
		if got := h.lim.pauses(); !reflect.DeepEqual(got, []time.Duration{30 * time.Second, DefaultRateLimitPause}) {
			t.Errorf("pauses = %v", got)
		}
	})
}

// X1-T106 — Req: X1 D-3, X1 D-28, X1 D-56. A 3xx is a plain failure before its body is read: a 3xx whose JSON body
// carries -2015 or -1001 is neither key rejected nor unreachable.
func TestT106_RedirectWithBinanceCodeIsPlainFailure(t *testing.T) {
	cases := []errorCase{
		{"302 with -2015", 302, `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`, nil},
		{"307 with -1001", 307, `{"code":-1001,"msg":"Internal error; unable to process your request. Please try again."}`, nil},
	}
	calls := []httpfixture.Call{timeCall(t0.UnixMilli())}
	for _, c := range cases {
		calls = append(calls, accountCall(c.status, c.body))
	}
	srv := httpfixture.Serve(t, httpfixture.File{Description: "3xx with a Binance code", Calls: calls}, weights)
	h := newHarness(t, srv.URL(), nil)
	for _, c := range cases {
		_, err := h.account(context.Background())
		checkError(t, c.name, err, nil)
	}
	srv.AssertAllServed()
	if got := h.lim.pauses(); len(got) != 0 {
		t.Errorf("pauses = %v; a 3xx pauses no budget", got)
	}
}

// signatureParam finds the value of a signature parameter in a text.
var signatureParam = regexp.MustCompile(`signature=([0-9a-f]+)`)

// X1-T107 — Req: SRS — Binance §3.2 Security; Core Connector contract Secrets; FR-217. Marker key and secret, log
// at debug; a request of each kind of T106 fails, and a second -1021: the markers and every signature sent appear
// in no log line and no error; a URL in a log line has no signature. The run of the engine comes with the
// registration of the connector (st4, st5).
func TestT107_SecretsNeverPrinted(t *testing.T) {
	calls := []httpfixture.Call{timeCall(t0.UnixMilli()), accountCall(200, accountOK)}
	for _, c := range errorCases {
		calls = append(calls, accountCall(c.status, c.body))
	}
	calls = append(calls, accountCall(400, timestampError), timeCall(t0.UnixMilli()), accountCall(400, timestampError),
		accountCall(429, ``))
	srv := httpfixture.Serve(t, httpfixture.File{Description: "every kind of failure", Calls: calls}, weights)
	h := newHarness(t, srv.URL(), nil)
	ctx := context.Background()
	var errs []string
	for range 1 + len(errorCases) + 2 { // success, each error, -1021 twice, 429
		_, err := h.account(ctx)
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	srv.AssertAllServed()
	for _, base := range []string{failingServer(t, false), failingServer(t, true)} {
		other := newHarness(t, base, nil)
		other.c.timeout = 200 * time.Millisecond
		other.s.key = h.s.key
		other.c.clockOf(base).readAt = t0 // a fresh offset: the signed request goes out
		_, err := other.account(ctx)
		if err == nil {
			t.Fatal("no error from a failing server")
		}
		errs = append(errs, err.Error())
		h.log.Write([]byte(other.log.String()))
	}
	if len(errs) < len(errorCases)+3 {
		t.Fatalf("errors = %v", errs)
	}

	var signatures []string
	for _, r := range srv.Received() {
		if m := signatureParam.FindStringSubmatch(r.RawQuery); m != nil {
			signatures = append(signatures, m[1])
		}
	}
	if len(signatures) < len(errorCases)+3 {
		t.Fatalf("%d signed requests seen", len(signatures))
	}
	log := h.log.String()
	if !strings.Contains(log, `"level":"DEBUG"`) || !strings.Contains(log, "/api/v3/account?omitZeroBalances=true&recvWindow=5000&timestamp=") {
		t.Fatalf("the debug log has no signed URL: %s", log)
	}
	forbidden := append([]string{h.s.key.APIKey.Value(), h.s.key.APISecret.Value(), "markerkey", "markersecret", "signature"}, signatures...)
	for _, text := range append([]string{log}, errs...) {
		for _, s := range forbidden {
			if strings.Contains(text, s) {
				t.Errorf("%q appears in: %s", s, text)
			}
		}
	}
	for _, e := range errs {
		if strings.Contains(e, "http://") || strings.Contains(e, "omitZeroBalances") {
			t.Errorf("an error quotes the URL: %s", e)
		}
	}
}

// X1-T108 — Req: SRS — Binance §2.1.1 Amounts; Core Connector contract. Decimal strings normalised without
// trailing zeros and without exponent; never parsed as a float.
func TestT108_AmountsAsDecimals(t *testing.T) {
	for in, want := range map[string]string{
		"0.00100000": "0.001", "0.00000000": "0", "12.50000000": "12.5", "100": "100", "100.0": "100", "007.0700": "7.07",
		"123456789012345678901234.123456789012345678901": "123456789012345678901234.123456789012345678901",
		"0.000000000000000001":                           "0.000000000000000001", "-0.000": "0", "-1.50": "-1.5",
	} {
		got, err := decimal(in)
		if err != nil || got != want {
			t.Errorf("decimal(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "1e-3", "1E3", ".5", "5.", "+1", "1,5", " 1", "1 ", "0x10", "NaN", "Inf", "--1", "1.2.3"} {
		if got, err := decimal(in); err == nil {
			t.Errorf("decimal(%q) = %q; want an error", in, got)
		} else if in != "" && strings.Contains(err.Error(), in) {
			t.Errorf("the error quotes the value: %v", err)
		}
	}
	src, err := os.ReadFile("decimal.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"float32", "float64", "ParseFloat", `"math`, `"strconv"`} {
		if strings.Contains(string(src), s) {
			t.Errorf("decimal.go uses %s: no float on the path of an amount", s)
		}
	}
}
