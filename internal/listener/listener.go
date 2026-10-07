// Package listener is the chain listener of card-auth (SRS — Card Spend UC-1 step 12, ADR-13): one WebSocket
// subscription to the Debited logs of the controller, whose logs are inclusion signals for the waiting debits.
// Nothing of a log is stored: the log only signals (ADR-13); the tracker fills the block from a sealed receipt.
//
// The listener is optional at run time (§3.2 Reliability): while it is down, receipt polling decides (EC-19).
// No log line, error or metric label of this package contains the WebSocket URL: its path carries the provider
// key. The endpoint is named by its variable, as internal/chain names the HTTP endpoints.
package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/debit"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Endpoint is the name of the WebSocket endpoint in logs and errors (SRS — Card Spend §3.1 rpc_ws_url).
const Endpoint = "CARD_AUTH_RPC_WS_URL"

// Reconnect backoff (SRS — Card Spend §3.2 Reliability, ADR-13 common rules): the n-th consecutive failed attempt
// waits BackoffFirst × 2^n, at most BackoffMax, of which a random share up to BackoffJitter is taken off. A
// subscription that was established resets the sequence. Constants of the code, not variables of §3.1 (owner's
// decision, 2026-10-06).
const (
	BackoffFirst  = time.Second
	BackoffMax    = 30 * time.Second
	BackoffJitter = 0.5
)

// ConnectTimeout bounds the dial, eth_chainId and eth_subscribe of one attempt (SRS — Card Spend §3.2 Reliability).
const ConnectTimeout = 10 * time.Second

// alert prefixes a log line that needs an operator's attention, as in internal/tracker.
const alert = "ALERT: "

// Deliverer receives the inclusion signals. *debit.Signals satisfies it.
type Deliverer interface {
	Deliver(authID common.Hash, sig debit.Signal) bool
}

// Metrics is chain_listener_connected of SRS — Card Spend §2.5.1.
type Metrics struct {
	connected prometheus.Gauge
}

// NewMetrics registers chain_listener_connected in reg at 0. card-auth registers it with or without a listener.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{connected: prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "chain_listener_connected",
		Help: "1 while the chain listener holds its WebSocket subscription, 0 otherwise.",
	})}
	reg.MustRegister(m.connected)
	return m
}

// Config holds the values of SRS — Card Spend §3.1 the listener uses.
type Config struct {
	Subscription string // listener_subscription: pendingLogs or logs
	Controller   common.Address
	ChainID      uint64 // chain_id: the WebSocket endpoint must serve it
}

// Listener holds one subscription and delivers its signals.
type Listener struct {
	wsURL   vault.Secret[string]
	cfg     Config
	topic   common.Hash
	signals Deliverer
	metrics *Metrics
	logger  *slog.Logger
	// rand returns a number in [0, 1) for the jitter.
	rand func() float64
}

// New returns the listener of url. A subscription type other than pendingLogs or logs is refused: the listener
// never subscribes to new blocks, to unfiltered Flashblocks or without the filter (ADR-13, budget of §4 issue 1).
func New(wsURL vault.Secret[string], cfg Config, signals Deliverer, metrics *Metrics, logger *slog.Logger) (*Listener, error) {
	if cfg.Subscription != config.SubscriptionPendingLogs && cfg.Subscription != config.SubscriptionLogs {
		return nil, errors.New("listener: CARD_AUTH_LISTENER_SUBSCRIPTION must be pendingLogs or logs")
	}
	if wsURL.Value() == "" {
		return nil, errors.New("listener: " + Endpoint + " is unset")
	}
	a, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("listener: controller ABI: %w", err)
	}
	ev, ok := a.Events["Debited"]
	if !ok {
		return nil, errors.New("listener: the controller ABI has no Debited event")
	}
	return &Listener{wsURL: wsURL, cfg: cfg, topic: ev.ID, signals: signals, metrics: metrics,
		logger: logger.With("endpoint", Endpoint, "subscription", cfg.Subscription), rand: rand.Float64}, nil
}

// Run holds the subscription until ctx ends: it connects, checks the chain ID, subscribes, delivers the signals, and
// after a loss reconnects with backoff and jitter. It never fails: a listener that is down changes nothing but its
// metric. At shutdown nothing is logged as a failure: the failures are the cancellation itself.
func (l *Listener) Run(ctx context.Context) {
	l.metrics.connected.Set(0)
	failures := 0
	for {
		if l.session(ctx) {
			failures = 0
		}
		if ctx.Err() != nil {
			return
		}
		wait := Backoff(failures, l.rand())
		failures++
		l.logger.InfoContext(ctx, "chain listener reconnecting", "in", wait.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Backoff is the wait before the next attempt after failures consecutive failed attempts; r in [0, 1) is the
// random draw of the jitter.
func Backoff(failures int, r float64) time.Duration {
	d := BackoffMax
	if failures < 32 {
		d = min(BackoffFirst<<failures, BackoffMax)
	}
	return d - time.Duration(float64(d)*BackoffJitter*r)
}

// session is one connection: it reports whether the subscription was established. eth_chainId is read on the same
// connection before eth_subscribe; another chain is an alert, no subscription is made, and the attempt counts as a
// failed connect (SRS — Card Spend §3.2 Reliability).
func (l *Listener) session(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	client, err := rpc.DialContext(cctx, l.wsURL.Value())
	if err != nil {
		if ctx.Err() == nil {
			l.logger.WarnContext(ctx, "chain listener cannot connect", "error", l.describe(err))
		}
		return false
	}
	// Close ends the subscription too; no eth_unsubscribe is sent on a connection that may be dead.
	defer client.Close()

	var served hexutil.Uint64
	if err := client.CallContext(cctx, &served, "eth_chainId"); err != nil {
		if ctx.Err() == nil {
			l.logger.WarnContext(ctx, "chain listener cannot read the chain ID", "error", l.describe(err))
		}
		return false
	}
	if uint64(served) != l.cfg.ChainID {
		l.logger.ErrorContext(ctx, alert+"the WebSocket endpoint of the chain listener serves another chain; no subscription",
			"endpoint_chain_id", uint64(served), "chain_id", l.cfg.ChainID)
		return false
	}

	logs := make(chan json.RawMessage, 64)
	filter := map[string]any{"address": l.cfg.Controller, "topics": []common.Hash{l.topic}}
	sub, err := client.EthSubscribe(cctx, logs, l.cfg.Subscription, filter)
	if err != nil {
		if ctx.Err() == nil {
			l.logger.WarnContext(ctx, "chain listener cannot subscribe", "error", l.describe(err))
		}
		return false
	}
	cancel()
	l.metrics.connected.Set(1)
	defer l.metrics.connected.Set(0)
	l.logger.InfoContext(ctx, "chain listener subscribed")

	for {
		select {
		case <-ctx.Done():
			return true
		case err := <-sub.Err():
			if ctx.Err() != nil {
				return true
			}
			l.logger.WarnContext(ctx, "chain listener disconnected; decisions rely on receipt polling", "error", l.describe(err))
			return true
		case raw := <-logs:
			l.handle(ctx, raw)
		}
	}
}

// debitedLog is the part of a log the filter needs. Nothing else of the payload is read (ADR-13).
type debitedLog struct {
	Address common.Address `json:"address"`
	Topics  []common.Hash  `json:"topics"`
	Removed bool           `json:"removed"`
}

// handle delivers a log as a signal when its address is the controller, topic0 is Debited and removed is not true.
// Signals delivers it only to a debit waiting for its authId; every other log changes nothing (ADR-13).
func (l *Listener) handle(ctx context.Context, raw json.RawMessage) {
	var lg debitedLog
	if err := json.Unmarshal(raw, &lg); err != nil {
		l.logger.DebugContext(ctx, "chain listener: malformed log ignored")
		return
	}
	if lg.Address != l.cfg.Controller || len(lg.Topics) < 2 || lg.Topics[0] != l.topic || lg.Removed {
		l.logger.DebugContext(ctx, "chain listener: log ignored", "address", lg.Address.Hex(), "removed", lg.Removed)
		return
	}
	if l.signals.Deliver(lg.Topics[1], debit.Signal{Source: debit.SourceSubscription}) {
		l.logger.DebugContext(ctx, "chain listener: inclusion signal delivered", "chain_auth_id", lg.Topics[1].Hex())
	}
}

// describe turns an error of the RPC client into text without the URL. Only a JSON-RPC error of the endpoint is
// kept; transport errors may quote the URL and are reduced to a fixed text.
func (l *Listener) describe(err error) string {
	var rpcErr rpc.Error
	var s string
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s = fmt.Sprintf("no answer within %s", ConnectTimeout)
	case errors.Is(err, context.Canceled):
		s = "cancelled"
	case errors.As(err, &rpcErr):
		s = fmt.Sprintf("JSON-RPC error %d: %s", rpcErr.ErrorCode(), rpcErr.Error())
	default:
		s = "the endpoint is unreachable or the connection was lost"
	}
	if u := l.wsURL.Value(); u != "" {
		s = strings.ReplaceAll(s, u, "[redacted]")
		if parsed, err := url.Parse(u); err == nil && len(parsed.Path) > 1 {
			s = strings.ReplaceAll(s, strings.Trim(parsed.Path, "/"), "[redacted]")
		}
	}
	return s
}
