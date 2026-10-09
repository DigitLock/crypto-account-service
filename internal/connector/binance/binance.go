// Package binance is the connector of the Binance exchange (SRS — Binance). X1 st2 holds the transport: the
// parsing of sources.config (config.go), the HMAC-SHA256 signature (sign.go), the HTTP client with the answers to
// errors and the time offset (this file), the decimal amounts (decimal.go) and the metrics (metrics.go). The
// limiter budgets, the key check and the balance snapshot come with st3, st4 and st5.
//
// Read calls only (ADR-3): the connector has no call that needs a trade, withdrawal or transfer permission. The key
// and the secret are vault.Secret values; they and any signature never appear in a log line, an error or a returned
// value, and a signed URL in a log line has no signature (SRS — Binance §3.2 Security).
package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Constants of the transport (SRS — Binance §2.1.1; X1 D-3).
const (
	// CallTimeout bounds each request: no answer within it is unreachable.
	CallTimeout = 10 * time.Second
	// DefaultRateLimitPause is the pause of a budget after 429 or 418 without Retry-After.
	DefaultRateLimitPause = 60 * time.Second
	// maxBody bounds the body of an answer that is read.
	maxBody = 64 << 20
	// headerAPIKey carries the API key of a signed request.
	headerAPIKey = "X-MBX-APIKEY"
)

// BudgetAPI is the budget of every /api endpoint: one weight limit per IP (SRS — Binance §2.1.1 Rate limits). The
// budgets of the /sapi endpoints come with st3.
const BudgetAPI = "api"

// endpoint is a row of the endpoint catalogue (SRS — Binance §2.1.2): the request, whether it is signed, and the
// budget and weight it reserves in the limiter.
type endpoint struct {
	method, path string
	signed       bool
	budget       string
	weight       int
}

// Endpoints of st2. The others of X1 (apiRestrictions, get-funding-asset, the Earn positions) come with st4 and
// st5.
var (
	endpointTime    = endpoint{method: http.MethodGet, path: "/api/v3/time", budget: BudgetAPI, weight: 1}
	endpointAccount = endpoint{method: http.MethodGet, path: "/api/v3/account", signed: true, budget: BudgetAPI, weight: 20}
)

// Error codes of Binance mapped by the table "Answers to errors" (SRS — Binance §2.1.1; X1 D-3).
const (
	codeUnknown      = -1001 // internal error
	codeOverloaded   = -1008 // server busy
	codeTimestamp    = -1021 // timestamp outside recvWindow
	codeBadSignature = -1022
	codeKeyFormat    = -2014
	codeKeyRejected  = -2015 // invalid key, IP or permissions
)

// errTimestamp marks the answer -1021: the time offset is read again and the request repeated once (X1 D-8).
var errTimestamp = errors.New("binance: timestamp outside recvWindow")

// Connector is the connector of the binance source.
type Connector struct {
	metrics Metrics
	logger  *slog.Logger
	client  *http.Client
	now     func() time.Time
	timeout time.Duration

	mu     sync.Mutex
	clocks map[string]*clock // by base URL
}

// New returns the connector with the sink of its metrics and its logger; both may be nil. No request is sent here.
func New(metrics Metrics, logger *slog.Logger) *Connector {
	if metrics == nil {
		metrics = nopMetrics{}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Connector{
		metrics: metrics,
		logger:  logger,
		// A redirect is not followed: it would carry X-MBX-APIKEY to another URL. A 3xx answer is a plain failure.
		client:  &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		now:     time.Now,
		timeout: CallTimeout,
		clocks:  map[string]*clock{},
	}
}

// clock is the time offset of one base URL, kept per process (X1 D-8).
type clock struct {
	mu     sync.Mutex
	offset time.Duration // local clock minus Binance clock
	readAt time.Time     // zero: never read
}

func (c *Connector) clockOf(baseURL string) *clock {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.clocks[baseURL]
	if !ok {
		cl = &clock{}
		c.clocks[baseURL] = cl
	}
	return cl
}

// limits is the seam of the rate limiter (SRS — Binance §2.1.1 Rate limits; X1 D-7): reserve the weight before
// every request, observe the used-weight headers after the answer, pause the budget on 429 or 418. st3 implements
// it with the limiter of the source (Reserve, Observe, Pause); in st2 it is noLimits.
type limits interface {
	reserve(ctx context.Context, ep endpoint) error
	observe(ep endpoint, header http.Header)
	pause(ep endpoint, d time.Duration)
}

type noLimits struct{}

func (noLimits) reserve(context.Context, endpoint) error { return nil }
func (noLimits) observe(endpoint, http.Header)           {}
func (noLimits) pause(endpoint, time.Duration)           {}

// session is the calls of one run, one key check or one test: the configuration of the source, the key when the
// calls are signed, and the limiter.
type session struct {
	c   *Connector
	cfg Config
	key *connector.ExchangeKey // nil: public calls only
	lim limits
}

func (c *Connector) session(src connector.Source, key *connector.ExchangeKey) *session {
	return &session{c: c, cfg: ParseConfig(src), key: key, lim: noLimits{}}
}

// call sends one request of ep with its parameters and returns the body of a 2xx answer. A signed request reads
// the time offset first when it is older than time_sync_interval; after -1021 it reads the offset again and repeats
// the request once; a second -1021 is a plain failure (EC-221).
func (s *session) call(ctx context.Context, ep endpoint, ps ...param) ([]byte, error) {
	if !ep.signed {
		return s.send(ctx, ep, encode(ps))
	}
	if s.key == nil {
		return nil, fmt.Errorf("binance: %s %s needs a key", ep.method, ep.path)
	}
	if err := s.syncTime(ctx, false); err != nil {
		return nil, err
	}
	body, err := s.sendSigned(ctx, ep, ps)
	if !errors.Is(err, errTimestamp) {
		return body, err
	}
	if err := s.syncTime(ctx, true); err != nil {
		return nil, err
	}
	body, err = s.sendSigned(ctx, ep, ps)
	if errors.Is(err, errTimestamp) {
		return nil, fmt.Errorf("binance: %s %s: timestamp outside recvWindow after the time was read again (EC-221)", ep.method, ep.path)
	}
	return body, err
}

func (s *session) sendSigned(ctx context.Context, ep endpoint, ps []param) ([]byte, error) {
	offset := s.c.clockOf(s.cfg.BaseURL).get()
	timestamp := s.c.now().Add(-offset).UnixMilli()
	return s.send(ctx, ep, signedQuery(ps, s.cfg.RecvWindowMS, timestamp, s.key.APISecret.Value()))
}

func (cl *clock) get() time.Duration {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.offset
}

// syncTime reads GET /api/v3/time when the offset of the base URL is older than time_sync_interval, or always when
// force is set, and sets binance_time_offset_ms. The offset is the local clock at the middle of the request minus
// the server time.
func (s *session) syncTime(ctx context.Context, force bool) error {
	cl := s.c.clockOf(s.cfg.BaseURL)
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if !force && !cl.readAt.IsZero() && s.c.now().Sub(cl.readAt) < s.cfg.TimeSyncInterval {
		return nil
	}
	sent := s.c.now()
	body, err := s.send(ctx, endpointTime, "")
	if err != nil {
		return err
	}
	received := s.c.now()
	var answer struct {
		ServerTime *int64 `json:"serverTime"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.ServerTime == nil {
		return fmt.Errorf("binance: %s %s: the answer has no serverTime", endpointTime.method, endpointTime.path)
	}
	cl.offset = sent.Add(received.Sub(sent) / 2).Sub(time.UnixMilli(*answer.ServerTime))
	cl.readAt = received
	s.c.metrics.TimeOffset(cl.offset.Milliseconds())
	return nil
}

// send sends one request with a query that is already encoded and, for a signed request, signed. It reserves the
// weight first, waits at most the call timeout and maps the answer by the table "Answers to errors". Neither the
// query nor the URL goes into an error; the log line has the URL without the signature.
func (s *session) send(ctx context.Context, ep endpoint, query string) ([]byte, error) {
	if err := s.lim.reserve(ctx, ep); err != nil {
		return nil, err
	}
	target := s.cfg.BaseURL + ep.path
	logged := target
	if query != "" {
		target += "?" + query
		logged += "?" + redactSignature(query)
	}
	cctx, cancel := context.WithTimeout(ctx, s.c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, ep.method, target, nil)
	if err != nil {
		return nil, fmt.Errorf("binance: %s %s: the request cannot be built", ep.method, ep.path)
	}
	if ep.signed {
		req.Header.Set(headerAPIKey, s.key.APIKey.Value())
	}
	start := s.c.now()
	resp, err := s.c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// Stopped from outside: not an answer of Binance.
			return nil, ctx.Err()
		}
		cause := "no answer within " + s.c.timeout.String()
		if cctx.Err() == nil {
			cause = transportCause(err, query)
		}
		s.c.logger.Debug("binance request", "method", ep.method, "url", logged, "error", cause)
		return nil, fmt.Errorf("binance: %s %s: %s: %w", ep.method, ep.path, cause, connector.ErrUnreachable)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.c.logger.Debug("binance request", "method", ep.method, "url", logged, "status", resp.StatusCode, "error", "body not read")
		return nil, fmt.Errorf("binance: %s %s: HTTP %d, the body was not read: %w", ep.method, ep.path, resp.StatusCode, connector.ErrUnreachable)
	}
	s.lim.observe(ep, resp.Header)
	s.c.logger.Debug("binance request", "method", ep.method, "url", logged, "status", resp.StatusCode,
		"ms", s.c.now().Sub(start).Milliseconds())
	if len(body) > maxBody {
		return nil, fmt.Errorf("binance: %s %s: the answer is larger than %d bytes", ep.method, ep.path, maxBody)
	}
	if err := s.answer(ep, resp.StatusCode, resp.Header, body); err != nil {
		return nil, err
	}
	return body, nil
}

// answer maps an answer by the table "Answers to errors" (SRS — Binance §2.1.1; X1 D-3); nil for 2xx.
func (s *session) answer(ep endpoint, status int, header http.Header, body []byte) error {
	where := fmt.Sprintf("binance: %s %s: HTTP %d", ep.method, ep.path, status)
	switch {
	case status == http.StatusTooManyRequests || status == http.StatusTeapot:
		pause, ok := parseRetryAfter(header.Get("Retry-After"), s.c.now())
		if !ok {
			pause = DefaultRateLimitPause
		}
		s.lim.pause(ep, pause)
		return fmt.Errorf("%s: %w", where, &connector.RateLimitError{Budget: ep.budget, Pause: pause})
	case status >= 500:
		return fmt.Errorf("%s: %w", where, connector.ErrUnreachable)
	case status == http.StatusForbidden:
		// A WAF rule: "a rate limit violation or a security block". No Retry-After, so no pause.
		s.c.logger.Warn("binance: HTTP 403, a WAF rule of Binance: a rate limit violation or a security block",
			"method", ep.method, "path", ep.path)
		return fmt.Errorf("%s: %w", where, connector.ErrUnreachable)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s: %w", where, connector.ErrKeyRejected)
	case status >= 200 && status < 300:
		return nil
	}
	code, ok := errorCode(body)
	if !ok {
		return errors.New(where)
	}
	where += ", code " + strconv.Itoa(code)
	switch code {
	case codeKeyRejected, codeKeyFormat, codeBadSignature:
		return fmt.Errorf("%s: %w", where, connector.ErrKeyRejected)
	case codeUnknown, codeOverloaded:
		return fmt.Errorf("%s: %w", where, connector.ErrUnreachable)
	case codeTimestamp:
		return fmt.Errorf("%s: %w", where, errTimestamp)
	}
	return errors.New(where)
}

// errorCode reads the code of an error answer {"code": -2015, "msg": "..."}. The message is not used: it is not
// needed and stays out of errors and logs.
func errorCode(body []byte) (int, bool) {
	var answer struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Code == nil || *answer.Code == 0 {
		return 0, false
	}
	return *answer.Code, true
}

// transportCause describes a transport error without the URL: net/http quotes the whole URL, signature included,
// in a *url.Error.
func transportCause(err error, query string) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	cause := err.Error()
	if strings.Contains(cause, paramSignature) || (query != "" && strings.Contains(cause, query)) {
		return "transport error"
	}
	return cause
}

// parseRetryAfter reads Retry-After of RFC 9110: seconds, or an HTTP date. A value in the past, zero or malformed is
// not given.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(n) * time.Second, n > 0
	}
	if at, err := http.ParseTime(v); err == nil && at.After(now) {
		return at.Sub(now), true
	}
	return 0, false
}
