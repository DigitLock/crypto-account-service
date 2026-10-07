package evm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Endpoints of a network. Each has its own budget in the limiter of the source, named as the endpoint (FR-110).
const (
	EndpointPrimary  = "primary"
	EndpointFallback = "fallback"
)

// Constants of the code (S3 D-34), as the backoff of the listener of card-auth, until a network needs others.
const (
	// CallTimeout bounds each RPC call.
	CallTimeout = 10 * time.Second
	// DefaultRateLimitPause is the pause of a budget after a rate-limit answer without Retry-After.
	DefaultRateLimitPause = 60 * time.Second
)

// network is the state of one source in memory: its clients, the endpoint of the next run (S3 D-33) and the start
// checks that passed (S3 D-23). It is kept until the process stops.
type network struct {
	mu             sync.Mutex
	client         *chain.Client
	onFallback     bool            // the next run uses the fallback
	chainChecked   map[string]bool // endpoint → the chain ID check passed
	tokenChecked   bool
	token          common.Address // token() of the controller, once the token check passed
	treasury       common.Address // treasury() of the controller, once the treasury check passed or read for pairing
	treasuryOK     bool
	treasuryKnown  bool                 // treasury holds treasury() of the controller
	treasuryWarned bool                 // the WARN line of an unset treasury_connection is written once per start
	rangeSize      uint64               // log range size after splitting (EC-306); 0: log_range_max
	processed      map[string]processed // connection ID → last processed block of its INCREMENTAL logs stream
}

// processed is the last processed block of an INCREMENTAL logs stream and when the connector last ran it.
type processed struct {
	block uint64
	at    time.Time
}

func (c *Connector) network(code string) *network {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.networks[code]
	if !ok {
		n = &network{chainChecked: map[string]bool{}, processed: map[string]processed{}}
		c.networks[code] = n
	}
	return n
}

// clientOf dials the clients of a source on first use, without a request. Errors name the variables only.
func (c *Connector) clientOf(ctx context.Context, code string, n *network) (*chain.Client, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.client != nil {
		return n.client, nil
	}
	ep := c.endpoints[code]
	primary, fallback := PrimaryVariable(code), FallbackVariable(code)
	if ep.Primary.Value() == "" {
		return nil, fmt.Errorf("evm: %s is not set: the source has no endpoint", primary)
	}
	client, err := chain.Dial(ctx, primary, ep.Primary, fallback, ep.Fallback,
		rpc.WithHTTPClient(&http.Client{Transport: retryAfterTransport{base: http.DefaultTransport}}))
	if err != nil {
		return nil, err
	}
	n.client = client
	return client, nil
}

// session is one run of one stream of one connection: one endpoint for all its calls (Common rules).
type session struct {
	c        *Connector
	conn     connector.Connection
	src      connector.Source
	cfg      Config
	net      *network
	client   *chain.Client
	endpoint string // EndpointPrimary or EndpointFallback; empty before the endpoint is chosen
	ep       chain.Endpoint
	final    uint64 // the final block of the run, when finalOK
	finalOK  bool
}

// open chooses the endpoint of the run and runs the start checks. A failure of open fails the run.
func (c *Connector) open(ctx context.Context, conn connector.Connection) (*session, error) {
	src := conn.Source
	s := &session{c: c, conn: conn, src: src, net: c.network(src.Code)}
	cfg, err := ParseConfig(src)
	if err != nil {
		return s, s.checkFailed(ctx, CheckConfig, err.Error())
	}
	c.metrics.StartCheck(src.Code, CheckConfig, false)
	s.cfg = cfg

	if s.client, err = c.clientOf(ctx, src.Code, s.net); err != nil {
		return s, err
	}
	s.net.mu.Lock()
	useFallback := s.net.onFallback && s.client.Fallback != nil
	s.net.mu.Unlock()
	s.endpoint, s.ep = EndpointPrimary, s.client.Primary
	if useFallback {
		s.endpoint, s.ep = EndpointFallback, *s.client.Fallback
	}
	c.metrics.FallbackActive(src.Code, useFallback)
	return s, s.startChecks(ctx)
}

// close records the endpoint of the next run of the source (S3 D-33): the fallback after a run that failed on the
// primary with an RPC error, the primary after any other run. A reorg guard hit or a failed start check is not an
// RPC error. No fallback configured: always the primary.
func (s *session) close(err error) {
	if s.endpoint == "" {
		return
	}
	s.net.mu.Lock()
	defer s.net.mu.Unlock()
	s.net.onFallback = s.endpoint == EndpointPrimary && isRPCFailure(err) && s.client.Fallback != nil
}

// rpcFailure is an RPC error of a call: transport, HTTP status, JSON-RPC error, timeout or rate limit. Its text
// names the variable of the endpoint, never its URL. A rate limit also unwraps to *connector.RateLimitError.
type rpcFailure struct {
	variable  string
	method    string
	cause     error // a chain.DescribedError: its text has no URL
	rateLimit *connector.RateLimitError
}

func (e *rpcFailure) Error() string {
	s := "evm: " + e.variable + ": " + e.method + ": " + e.cause.Error()
	if e.rateLimit != nil {
		s += "; budget paused for " + e.rateLimit.Pause.String()
	}
	return s
}

func (e *rpcFailure) Unwrap() []error {
	if e.rateLimit != nil {
		return []error{e.rateLimit, e.cause}
	}
	return []error{e.cause}
}

func isRPCFailure(err error) bool {
	var f *rpcFailure
	return errors.As(err, &f)
}

func (s *session) variable() string {
	if s.endpoint == EndpointFallback {
		return FallbackVariable(s.src.Code)
	}
	return PrimaryVariable(s.src.Code)
}

// call makes one JSON-RPC call on the endpoint of the run: it reserves cost 1 in the budget of the endpoint first
// (FR-110) and waits at most CallTimeout. A rate-limit answer pauses the budget of the endpoint for Retry-After,
// else DefaultRateLimitPause (S3 D-34).
func (s *session) call(ctx context.Context, result any, method string, args ...any) error {
	if lim := s.conn.Limiter; lim != nil {
		if err := lim.Reserve(ctx, s.endpoint, 1); err != nil {
			return err
		}
	}
	retry := &retryAfter{}
	cctx, cancel := context.WithTimeout(context.WithValue(ctx, retryAfterKey{}, retry), CallTimeout)
	defer cancel()
	err := s.ep.Client.Client().CallContext(cctx, result, method, args...)
	switch {
	case err == nil:
		s.c.metrics.RPCRequest(s.src.Code, s.endpoint, method, ResultSuccess)
		return nil
	case ctx.Err() != nil:
		// Stopped from outside: not a failure of the endpoint.
		return ctx.Err()
	case method == "eth_getLogs" && isTooLarge(err):
		// Before the rate limit: some providers answer -32005 for both (EC-306).
		s.c.metrics.RPCRequest(s.src.Code, s.endpoint, method, ResultFailure)
		return &tooLargeError{&rpcFailure{variable: s.variable(), method: method, cause: s.client.DescribeErr(err, CallTimeout)}}
	case chain.IsRateLimit(err):
		pause, ok := retry.get()
		if !ok {
			pause = DefaultRateLimitPause
		}
		if lim := s.conn.Limiter; lim != nil {
			lim.Pause(s.endpoint, pause)
		}
		s.c.metrics.RPCRequest(s.src.Code, s.endpoint, method, ResultRateLimit)
		return &rpcFailure{variable: s.variable(), method: method, cause: s.client.DescribeErr(err, CallTimeout),
			rateLimit: &connector.RateLimitError{Budget: s.endpoint, Pause: pause}}
	default:
		s.c.metrics.RPCRequest(s.src.Code, s.endpoint, method, ResultFailure)
		return &rpcFailure{variable: s.variable(), method: method, cause: s.client.DescribeErr(err, CallTimeout)}
	}
}

// header is the part of a block the connector reads: number, hash and time.
type header struct {
	Number hexutil.Uint64 `json:"number"`
	Hash   common.Hash    `json:"hash"`
	Time   hexutil.Uint64 `json:"timestamp"`
}

// headerAt reads the header of a block tag or number, without transactions; nil when the node has no such block.
func (s *session) headerAt(ctx context.Context, block string) (*header, error) {
	var h *header
	if err := s.call(ctx, &h, "eth_getBlockByNumber", block, false); err != nil {
		return nil, err
	}
	return h, nil
}

// finalBlock resolves the final block by the shared function of internal/chain (S3 D-4) and sets evm_final_block.
// ok is false while no block is final.
func (s *session) finalBlock(ctx context.Context) (uint64, bool, error) {
	n, ok, err := chain.FinalBlock(ctx, s.cfg.Rule(), chain.FinalReads{
		TagNumber: func(ctx context.Context, tag string) (uint64, error) {
			h, err := s.headerAt(ctx, tag)
			if err != nil {
				return 0, err
			}
			if h == nil {
				return 0, fmt.Errorf("evm: %s: the endpoint has no %s block", s.variable(), tag)
			}
			return uint64(h.Number), nil
		},
		Head: func(ctx context.Context) (uint64, error) {
			var head hexutil.Uint64
			err := s.call(ctx, &head, "eth_blockNumber")
			return uint64(head), err
		},
	})
	if err == nil && ok {
		s.c.metrics.FinalBlock(s.src.Code, n)
	}
	return n, ok, err
}

// retryAfterKey carries a *retryAfter in the context of a call; retryAfterTransport fills it from the Retry-After
// header of a 429 answer. go-ethereum does not expose the headers of an HTTP error. No header is logged.
type retryAfterKey struct{}

type retryAfter struct{ d atomic.Int64 } // nanoseconds; 0: not given

func (r *retryAfter) get() (time.Duration, bool) {
	d := time.Duration(r.d.Load())
	return d, d > 0
}

type retryAfterTransport struct{ base http.RoundTripper }

func (t retryAfterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		return resp, err
	}
	if r, ok := req.Context().Value(retryAfterKey{}).(*retryAfter); ok {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			r.d.Store(int64(d))
		}
	}
	return resp, nil
}

// parseRetryAfter reads Retry-After of RFC 9110: seconds, or an HTTP date. A value in the past or malformed is
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
