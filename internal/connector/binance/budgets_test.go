package binance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
)

// limiterClock is the clock of internal/limiter in a test: it moves only when the test advances it, and Wait
// blocks until then.
type limiterClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters map[chan struct{}]time.Time
}

func newLimiterClock() *limiterClock {
	return &limiterClock{now: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC), waiters: map[chan struct{}]time.Time{}}
}

func (c *limiterClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *limiterClock) Wait(ctx context.Context, until time.Time) error {
	c.mu.Lock()
	if !c.now.Before(until) {
		c.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	c.waiters[ch] = until
	c.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, ch)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *limiterClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for ch, until := range c.waiters {
		if !c.now.Before(until) {
			close(ch)
			delete(c.waiters, ch)
		}
	}
}

// waiting waits a little for a reservation that waits on the clock and returns its wake-up time.
func (c *limiterClock) waiting(t *testing.T) time.Time {
	t.Helper()
	for range 2000 {
		c.mu.Lock()
		for _, until := range c.waiters {
			c.mu.Unlock()
			return until
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no reservation waits on the clock")
	return time.Time{}
}

// drive moves the clock to the earliest wake-up time whenever all active goroutines wait on it, until active is
// 0: a burst runs through its windows without real waiting, and no request is in flight while the time moves.
func (c *limiterClock) drive(active *atomic.Int64) {
	for active.Load() > 0 {
		c.mu.Lock()
		if int64(len(c.waiters)) >= active.Load() {
			earliest := time.Time{}
			for _, until := range c.waiters {
				if earliest.IsZero() || until.Before(earliest) {
					earliest = until
				}
			}
			c.mu.Unlock()
			c.Advance(earliest.Sub(c.Now()))
			continue
		}
		c.mu.Unlock()
		time.Sleep(100 * time.Microsecond)
	}
}

// sharedLimiter is a limiter of internal/limiter with the budgets of the connector for src, on clock.
func sharedLimiter(t *testing.T, c *Connector, src connector.Source, clock limiter.Clock) *limiter.Limiter {
	t.Helper()
	l, err := limiter.New(c.Budgets(src), clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// metric reads the value of a gauge or counter of reg with one label, or 0 when the series does not exist.
func metric(t *testing.T, reg *prometheus.Registry, name, label, value string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					if m.GetCounter() != nil {
						return m.GetCounter().GetValue()
					}
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

// sapiCall is an answer of a signed /sapi endpoint without parameters.
func sapiCall(ep endpoint, status int, headers map[string]string, body string) httpfixture.Call {
	c := httpfixture.Call{Method: ep.method, Path: ep.path, Query: "recvWindow=5000", HTTPStatus: status, Headers: headers}
	if body != "" {
		c.Body = []byte(body)
	}
	return c
}

func assertBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the request went out before the end of its wait: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func assertDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the request did not go out")
	}
}

// X1-T201 — Req: SRS — Binance §2.1.1 Rate limits; X1 D-6, X1 D-25. budget_share 0.5: one budget api of 3000 per
// minute; one budget per /sapi endpoint of X1 of 6000 per minute; no budget per account; no request.
func TestT201_Budgets(t *testing.T) {
	c := New(nil, nil)
	c.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("Budgets sent a request")
		return nil, errors.New("no request expected")
	})
	budget := func(name string, units int) connector.Budget {
		return connector.Budget{Name: name, Units: units, Window: time.Minute}
	}
	want := []connector.Budget{
		budget("api", 3000),
		budget("sapi:/sapi/v1/account/apiRestrictions", 6000),
		budget("sapi:/sapi/v1/asset/get-funding-asset", 6000),
		budget("sapi:/sapi/v1/simple-earn/flexible/position", 6000),
		budget("sapi:/sapi/v1/simple-earn/locked/position", 6000),
	}
	for _, config := range []string{`{"budget_share": 0.5}`, `{}`, `{"budget_share": "0.9"}`} {
		if got := c.Budgets(source(config)); !reflect.DeepEqual(got, want) {
			t.Errorf("Budgets(%s) =\n %v\nwant\n %v", config, got, want)
		}
	}
	for share, units := range map[string][2]int{"1": {6000, 12000}, "0.29": {1740, 3480}, "0.0001": {1, 1}, "0.33333": {1999, 3999}} {
		got := c.Budgets(source(`{"budget_share": ` + share + `}`))
		if len(got) != 5 || got[0].Units != units[0] || got[1].Units != units[1] || got[4].Units != units[1] {
			t.Errorf("budget_share %s: %v, want %d and %d", share, got, units[0], units[1])
		}
	}
	if _, err := limiter.New(c.Budgets(source(`{}`)), limiter.SystemClock{}, nil); err != nil {
		t.Errorf("the limiter refuses the budgets: %v", err)
	}
	for _, ep := range endpoints {
		if want := map[bool]string{true: BudgetAPI, false: SAPIBudget(ep.path)}[ep.path[:5] == "/api/"]; ep.budget != want {
			t.Errorf("%s: budget %s, want %s", ep.path, ep.budget, want)
		}
	}
}

// X1-T202 — Req: Core FR-110; SRS — Binance §3.2 Performance. At the level of each endpoint row: each request
// reserves the weight of its row in its budget before it is sent, the time call included. The sequences of the key
// check and the snapshot come with st4 and st5. No limiter is an error of the caller.
func TestT202_ReservationBeforeEveryRequest(t *testing.T) {
	ctx := context.Background()
	for _, ep := range endpoints {
		t.Run(ep.path, func(t *testing.T) {
			calls := []httpfixture.Call{timeCall(t0.UnixMilli())}
			want := []string{"reserve api 1 after 0 requests"}
			if ep != endpointTime {
				calls = append(calls, sapiCall(ep, 200, nil, `{}`))
				want = append(want, "reserve "+ep.budget+" "+strconv.Itoa(ep.weight)+" after 1 requests")
			}
			srv := httpfixture.Serve(t, httpfixture.File{Description: ep.path, Calls: calls}, weights)
			h := newHarness(t, srv.URL(), nil)
			h.lim.sent = func() int { return len(srv.Received()) }
			if _, err := h.s.call(ctx, ep); err != nil {
				t.Fatal(err)
			}
			srv.AssertAllServed()
			var reserved []string
			for _, e := range h.lim.log() {
				if e[:7] == "reserve" {
					reserved = append(reserved, e)
				}
			}
			if !slices.Equal(reserved, want) {
				t.Errorf("reservations = %v, want %v", reserved, want)
			}
			if used := srv.Used(ep.budget); used != ep.weight+map[bool]int{true: 1, false: 0}[ep.budget == BudgetAPI && ep != endpointTime] {
				t.Errorf("weight at the fake server in %s = %d", ep.budget, used)
			}
		})
	}
	if _, err := New(nil, nil).session(source(`{}`), testKey(t), nil); !errors.Is(err, errNoLimiter) {
		t.Errorf("session without a limiter: %v, want errNoLimiter", err)
	}
}

// X1-T203 — Req: FR-213; X1 D-7. The answers carry X-MBX-USED-WEIGHT-1M above the weight reserved in the window:
// the budget counts at least the header value, and the next reservation that does not fit waits for the next
// window. The unit part of Observe is in internal/limiter.
func TestT203_UsedWeightOfAPI(t *testing.T) {
	src := `{"budget_share": 0.01}` // api: 60 per minute
	header := func(v string) map[string]string { return map[string]string{"X-MBX-USED-WEIGHT-1M": v} }
	srv := httpfixture.Serve(t, httpfixture.File{Description: "used weight above the reserved weight", Calls: []httpfixture.Call{
		{Method: "GET", Path: "/api/v3/time", HTTPStatus: 200, Headers: header("30"), Body: []byte(`{"serverTime":1790000000000}`)},
		{Method: "GET", Path: "/api/v3/account", Query: accountQuery, HTTPStatus: 200, Headers: header("55"), Body: []byte(accountOK)},
		{Method: "GET", Path: "/api/v3/account", Query: accountQuery, HTTPStatus: 200, Headers: header("20"), Body: []byte(accountOK)},
	}}, weights)
	h := newHarness(t, srv.URL(), nil)
	clock := newLimiterClock()
	start := clock.Now()
	s := mustSession(t, h.c, source(`{"base_url": "`+srv.URL()+`", "budget_share": 0.01}`), h.s.key,
		sharedLimiter(t, h.c, source(src), clock))
	ctx := context.Background()

	// Reserved 1 + 20 = 21; Binance counts 55: 55 + 20 does not fit 60.
	if _, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"}); done <- err }()
	if got := clock.waiting(t); !got.Equal(start.Add(time.Minute)) {
		t.Errorf("waits until %v, want the next window at %v", got, start.Add(time.Minute))
	}
	assertBlocked(t, done)
	if got := len(srv.Received()); got != 2 {
		t.Fatalf("%d requests before the next window, want 2", got)
	}
	clock.Advance(time.Minute)
	assertDone(t, done)
	srv.AssertAllServed()
}

// X1-T204 — Req: FR-213; X1 D-7. Paged reads of flexible/position carry X-SAPI-USED-IP-WEIGHT-1M: only the budget of
// that endpoint takes the header value; a header of the /api family on a /sapi answer, a missing or a malformed
// header reports nothing.
func TestT204_UsedWeightOfSAPI(t *testing.T) {
	flexible := func(page int, headers map[string]string) httpfixture.Call {
		return httpfixture.Call{Method: "GET", Path: endpointFlexible.path, Query: "size=100&current=" + strconv.Itoa(page) + "&recvWindow=5000",
			HTTPStatus: 200, Headers: headers, Body: []byte(`{"rows":[],"total":0}`)}
	}
	srv := httpfixture.Serve(t, httpfixture.File{Description: "flexible pages with the IP header", Calls: []httpfixture.Call{
		{Method: "GET", Path: "/api/v3/time", HTTPStatus: 200, Headers: map[string]string{"X-MBX-USED-WEIGHT-1M": "5"}, Body: []byte(`{"serverTime":1790000000000}`)},
		flexible(1, map[string]string{"X-SAPI-USED-IP-WEIGHT-1M": "150", "X-MBX-USED-WEIGHT-1M": "999"}),
		flexible(2, map[string]string{"X-SAPI-USED-IP-WEIGHT-1M": "300"}),
		flexible(3, map[string]string{"X-SAPI-USED-IP-WEIGHT-1M": "abc"}),
		flexible(4, map[string]string{"X-SAPI-USED-IP-WEIGHT-1M": "-1"}),
		flexible(5, nil),
	}}, weights)
	reg := prometheus.NewRegistry()
	h := newHarness(t, srv.URL(), NewPromMetrics(reg))
	for page := 1; page <= 5; page++ {
		if _, err := h.s.call(context.Background(), endpointFlexible, param{"size", "100"}, param{"current", strconv.Itoa(page)}); err != nil {
			t.Fatal(err)
		}
	}
	srv.AssertAllServed()
	var observed []string
	for _, e := range h.lim.log() {
		if e[:7] == "observe" {
			observed = append(observed, e)
		}
	}
	budget := SAPIBudget(endpointFlexible.path)
	if want := []string{"observe api 5", "observe " + budget + " 150", "observe " + budget + " 300"}; !slices.Equal(observed, want) {
		t.Errorf("observations = %v, want %v", observed, want)
	}
	if got := metric(t, reg, "binance_used_weight", "budget", budget); got != 300 {
		t.Errorf("binance_used_weight{%s} = %v, want 300", budget, got)
	}
	if got := metric(t, reg, "binance_used_weight", "budget", "api"); got != 5 {
		t.Errorf("binance_used_weight{api} = %v, want 5: a /sapi answer does not report the /api header", got)
	}
}

// X1-T205 — Req: FR-213, SRS — Binance §2.5.1; X1 D-16. A burst of signed requests of several sessions on one
// limiter: the weight counted at the fake server in each window of the limiter never exceeds the share of the
// budget; binance_used_weight{api} is the last header value.
func TestT205_ShareNeverExceeded(t *testing.T) {
	const sessions, perSession = 3, 4
	calls := []httpfixture.Call{timeCall(t0.UnixMilli())}
	for range sessions * perSession {
		calls = append(calls, httpfixture.Call{Method: "GET", Path: "/api/v3/account", Query: accountQuery, HTTPStatus: 200,
			Headers: map[string]string{"X-MBX-USED-WEIGHT-1M": "1"}, Body: []byte(accountOK)})
	}
	calls = append(calls, httpfixture.Call{Method: "GET", Path: "/api/v3/account", Query: accountQuery, HTTPStatus: 200,
		Headers: map[string]string{"X-MBX-USED-WEIGHT-1M": "37"}, Body: []byte(accountOK)})
	srv := httpfixture.Serve(t, httpfixture.File{Description: "a burst of account calls", Calls: calls}, weights)

	// In front of the fake server: the weight of each request by the window of the limiter it was sent in. The
	// clock moves only while every session waits, so the time of arrival is the time of the reservation.
	clock := newLimiterClock()
	var mu sync.Mutex
	perWindow := map[time.Time]int{}
	target, _ := url.Parse(srv.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		perWindow[clock.Now()] += weights[r.Method+" "+r.URL.Path].Weight
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()

	src := source(`{"base_url": "` + front.URL + `", "budget_share": 0.01}`) // api: 60 per minute
	reg := prometheus.NewRegistry()
	h := newHarness(t, front.URL, NewPromMetrics(reg))
	lim := sharedLimiter(t, h.c, src, clock)
	var wg sync.WaitGroup
	var active atomic.Int64
	active.Store(sessions)
	go clock.drive(&active)
	errs := make(chan error, sessions*perSession)
	for range sessions {
		s := mustSession(t, h.c, src, testKey(t), lim)
		wg.Go(func() {
			defer active.Add(-1)
			for range perSession {
				_, err := s.call(context.Background(), endpointAccount, param{"omitZeroBalances", "true"})
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	last := mustSession(t, h.c, src, testKey(t), lim)
	if _, err := last.call(context.Background(), endpointAccount, param{"omitZeroBalances", "true"}); err != nil {
		t.Fatal(err)
	}
	srv.AssertAllServed()

	units := h.c.Budgets(src)[0].Units
	total := 0
	for start, w := range perWindow {
		total += w
		if w > units {
			t.Errorf("window at %v: weight %d above the share %d", start, w, units)
		}
	}
	if want := 1 + (sessions*perSession+1)*20; total != want || srv.Used(BudgetAPI) != want || len(perWindow) < want/units {
		t.Errorf("weight %d in %d windows, server %d; want %d in at least %d windows", total, len(perWindow), srv.Used(BudgetAPI), want, want/units)
	}
	if got := metric(t, reg, "binance_used_weight", "budget", BudgetAPI); got != 37 {
		t.Errorf("binance_used_weight{api} = %v, want the last header value 37", got)
	}
}

// pauseCase runs a limit answer of the account call: the RateLimitError with its budget and pause, the pause of
// that budget in the limiter, a request of another budget going on, no request of the paused budget before the end
// of the pause.
func pauseCase(t *testing.T, status int, retryAfter string, want time.Duration) {
	t.Helper()
	limited := httpfixture.Call{Method: "GET", Path: "/api/v3/account", Query: accountQuery, HTTPStatus: status}
	if retryAfter != "" {
		limited.Headers = map[string]string{"Retry-After": retryAfter}
	}
	srv := httpfixture.Serve(t, httpfixture.File{Description: "limit answer of /api", Calls: []httpfixture.Call{
		timeCall(t0.UnixMilli()),
		limited,
		sapiCall(endpointRestrictions, 200, nil, `{"enableReading":true}`),
		accountCall(200, accountOK),
	}}, weights)
	reg := prometheus.NewRegistry()
	h := newHarness(t, srv.URL(), NewPromMetrics(reg))
	clock := newLimiterClock()
	src := source(`{"base_url": "` + srv.URL() + `"}`)
	s := mustSession(t, h.c, src, h.s.key, sharedLimiter(t, h.c, src, clock))
	ctx := context.Background()

	_, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"})
	var rl *connector.RateLimitError
	if !errors.As(err, &rl) || rl.Budget != BudgetAPI || rl.Pause != want {
		t.Fatalf("error %v; want RateLimitError of api for %v", err, want)
	}
	if got := len(srv.Received()); got != 2 {
		t.Fatalf("%d requests; no further request of the budget in the call", got)
	}
	if got := metric(t, reg, "binance_rate_limit_responses_total", "code", strconv.Itoa(status)); got != 1 {
		t.Errorf("binance_rate_limit_responses_total{code=%d} = %v, want 1", status, got)
	}

	done := make(chan error, 1)
	go func() { _, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"}); done <- err }()
	if got := clock.waiting(t); !got.Equal(clock.Now().Add(want)) {
		t.Errorf("the api request waits until %v, want the end of the pause %v", got, clock.Now().Add(want))
	}
	if _, err := s.call(ctx, endpointRestrictions); err != nil {
		t.Fatalf("a /sapi request during the pause of api: %v", err)
	}
	clock.Advance(want - time.Second)
	assertBlocked(t, done)
	if got := len(srv.Received()); got != 3 {
		t.Fatalf("%d requests during the pause, want 3: no api request", got)
	}
	clock.Advance(time.Second)
	assertDone(t, done)
	srv.AssertAllServed()
}

// X1-T206 — Req: FR-214, EC-205; Core EC-107. /api answers 429 with Retry-After 30.
func TestT206_429WithRetryAfter(t *testing.T) { pauseCase(t, 429, "30", 30*time.Second) }

// X1-T207 — Req: FR-214. /api answers 418 with Retry-After 120. /sapi answers only 429: no 418 case for /sapi.
func TestT207_418WithRetryAfter(t *testing.T) { pauseCase(t, 418, "120", 120*time.Second) }

// X1-T208 — Req: FR-214; X1 D-3. A /sapi endpoint answers 429 without Retry-After: the budget of that endpoint is
// paused for 60 s; the /api budget and the other /sapi budgets go on.
func TestT208_429WithoutRetryAfter(t *testing.T) {
	srv := httpfixture.Serve(t, httpfixture.File{Description: "429 of get-funding-asset without Retry-After", Calls: []httpfixture.Call{
		timeCall(t0.UnixMilli()),
		sapiCall(endpointFunding, 429, nil, ""),
		accountCall(200, accountOK),
		sapiCall(endpointRestrictions, 200, nil, `{}`),
		sapiCall(endpointFunding, 200, nil, `[]`),
	}}, weights)
	reg := prometheus.NewRegistry()
	h := newHarness(t, srv.URL(), NewPromMetrics(reg))
	clock := newLimiterClock()
	src := source(`{"base_url": "` + srv.URL() + `"}`)
	s := mustSession(t, h.c, src, h.s.key, sharedLimiter(t, h.c, src, clock))
	ctx := context.Background()
	budget := SAPIBudget(endpointFunding.path)

	_, err := s.call(ctx, endpointFunding)
	var rl *connector.RateLimitError
	if !errors.As(err, &rl) || rl.Budget != budget || rl.Pause != DefaultRateLimitPause || DefaultRateLimitPause != time.Minute {
		t.Fatalf("error %v; want RateLimitError of %s for 60s", err, budget)
	}
	done := make(chan error, 1)
	go func() { _, err := s.call(ctx, endpointFunding); done <- err }()
	if got := clock.waiting(t); !got.Equal(clock.Now().Add(time.Minute)) {
		t.Errorf("funding waits until %v, want 60 s from now", got)
	}
	if _, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"}); err != nil {
		t.Fatalf("the api budget during the pause of %s: %v", budget, err)
	}
	if _, err := s.call(ctx, endpointRestrictions); err != nil {
		t.Fatalf("another /sapi budget during the pause of %s: %v", budget, err)
	}
	clock.Advance(59 * time.Second)
	assertBlocked(t, done)
	clock.Advance(time.Second)
	assertDone(t, done)
	srv.AssertAllServed()
	if got := metric(t, reg, "binance_rate_limit_responses_total", "code", "429"); got != 1 {
		t.Errorf("binance_rate_limit_responses_total{code=429} = %v, want 1", got)
	}
}

// X1-T209 — Req: SRS — Binance §2.5.1; X1 D-16. The connector half: binance_rate_limit_responses_total{code} +1 per
// limit answer, by its code. rate_limit_rejections_total{source} of the engine comes with st5 (X1 D-29).
func TestT209_RateLimitResponsesCounted(t *testing.T) {
	srv := httpfixture.Serve(t, httpfixture.File{Description: "limit answers", Calls: []httpfixture.Call{
		{Method: "GET", Path: "/api/v3/time", HTTPStatus: 429},
		{Method: "GET", Path: "/api/v3/time", HTTPStatus: 418, Headers: map[string]string{"Retry-After": "120"}},
		timeCall(t0.UnixMilli()),
		sapiCall(endpointLocked, 429, map[string]string{"Retry-After": "5"}, ""),
		sapiCall(endpointRestrictions, 403, nil, ""),
	}}, weights)
	reg := prometheus.NewRegistry()
	h := newHarness(t, srv.URL(), NewPromMetrics(reg))
	ctx := context.Background()
	for _, ep := range []endpoint{endpointTime, endpointTime, endpointLocked, endpointRestrictions} {
		if _, err := h.s.call(ctx, ep); err == nil {
			t.Fatalf("%s: no error", ep.path)
		}
	}
	srv.AssertAllServed()
	for code, want := range map[string]float64{"429": 2, "418": 1, "403": 0} {
		if got := metric(t, reg, "binance_rate_limit_responses_total", "code", code); got != want {
			t.Errorf("binance_rate_limit_responses_total{code=%s} = %v, want %v", code, got, want)
		}
	}
	if got := h.lim.pauses(); !slices.Equal(got, []time.Duration{time.Minute, 120 * time.Second, 5 * time.Second}) {
		t.Errorf("pauses = %v; the 403 pauses nothing", got)
	}
}

// X1-T109 — Req: SRS — Binance §2.6; X1 D-13. Offline replay of every committed fixture of testdata/fixtures/binance/
// through the fake server, with the matching calls of the connector and no network.
func TestT109_ReplayCommittedFixtures(t *testing.T) {
	ctx := context.Background()
	runs := map[string]func(h *harness) error{
		"time.json": func(h *harness) error {
			body, err := h.s.call(ctx, endpointTime)
			if err == nil && len(body) == 0 {
				err = errors.New("no body")
			}
			return err
		},
		"account.json": func(h *harness) error {
			body, err := h.account(ctx)
			if err != nil {
				return err
			}
			if a := parseAccount(t, body); a.UID == nil || a.UID.String() != strconv.Itoa(httpfixture.FictitiousUID) {
				return errors.New("the uid is not the fictitious one")
			}
			return nil
		},
		"error_bad_signature.json":     keyRejected,
		"error_bad_key.json":           keyRejected,
		"key_read_only.json":           checkAccount(false, nil),
		"key_ip_restricted.json":       checkAccount(true, nil),
		"key_not_read_only.json":       checkAccount(false, &connector.KeyNotReadOnlyError{Permissions: []string{"enableWithdrawals"}}),
		"key_reading_disabled.json":    checkAccount(false, connector.ErrKeyRejected),
		"key_rejected.json":            checkAccount(false, connector.ErrKeyRejected),
		"key_account_rejected.json":    checkAccount(false, connector.ErrKeyRejected),
		"snapshot_full.json":           fetchSnapshot(8),
		"snapshot_paged.json":          fetchSnapshot(4),
		"snapshot_funding_fails.json":  fetchSnapshot(-1),
		"snapshot_flexible_fails.json": fetchSnapshot(-1),
		"snapshot_locked_fails.json":   fetchSnapshot(-1),
	}
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, e := range entries {
		name := e.Name()
		seen = append(seen, name)
		run, ok := runs[name]
		if !ok {
			t.Errorf("%s has no replay in this test", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			srv := httpfixture.ServeFile(t, filepath.Join(fixturesDir, name), weights)
			h := newHarness(t, srv.URL(), nil)
			if err := run(h); err != nil {
				t.Fatal(err)
			}
			srv.AssertAllServed()
		})
	}
	for name := range runs {
		if !slices.Contains(seen, name) {
			t.Errorf("%s is missing from %s", name, fixturesDir)
		}
	}
}

// checkAccount runs the key check of UC-201 on a fixture: the account with the fictitious uid and ipRestrict, or the
// error want.
func checkAccount(ipRestricted bool, want error) func(h *harness) error {
	return func(h *harness) error {
		info, err := h.c.CheckAccount(context.Background(), source(`{"base_url": "`+h.s.cfg.BaseURL+`"}`),
			connector.Credentials{ExchangeKey: h.s.key}, h.lim)
		var notReadOnly, wantNotReadOnly *connector.KeyNotReadOnlyError
		switch {
		case errors.As(want, &wantNotReadOnly):
			if !errors.As(err, &notReadOnly) || !slices.Equal(notReadOnly.Permissions, wantNotReadOnly.Permissions) {
				return errors.New("want KeyNotReadOnlyError " + wantNotReadOnly.Error() + ", got: " + errString(err))
			}
		case want != nil:
			if !errors.Is(err, want) {
				return errors.New("want " + want.Error() + ", got: " + errString(err))
			}
		case err != nil:
			return err
		case info.Identity != strconv.Itoa(httpfixture.FictitiousUID) || !slices.Equal(info.Permissions, []string{"READ"}) ||
			info.IPRestricted == nil || *info.IPRestricted != ipRestricted:
			return errors.New("unexpected account info")
		}
		return nil
	}
}

// fetchSnapshot runs FetchSnapshot on a fixture: n balances, or an error when n is -1.
func fetchSnapshot(n int) func(h *harness) error {
	return func(h *harness) error {
		src := source(`{"base_url": "` + h.s.cfg.BaseURL + `"}`)
		src.Aliases = []connector.Alias{{NativeAsset: "LDO", Asset: "LDO"}}
		snap, err := h.c.FetchSnapshot(context.Background(), connector.Connection{Source: src, Key: h.s.key, Limiter: h.lim})
		switch {
		case n < 0 && err == nil:
			return errors.New("want an error")
		case n >= 0 && err != nil:
			return err
		case n >= 0 && len(snap.Balances) != n:
			return errors.New("want " + strconv.Itoa(n) + " balances, got " + strconv.Itoa(len(snap.Balances)))
		}
		return nil
	}
}

func keyRejected(h *harness) error {
	if _, err := h.account(context.Background()); !errors.Is(err, connector.ErrKeyRejected) {
		return errors.New("want key rejected, got: " + errString(err))
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}
