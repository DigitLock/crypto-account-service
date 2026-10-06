package listener_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/debit"
	"github.com/DigitLock/crypto-account-service/internal/listener"
	"github.com/DigitLock/crypto-account-service/internal/listener/listenertest"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Phase 6 of docs/test-plan-s2.md, the part of the listener alone: the fake WebSocket server of listenertest and a
// recording Deliverer. The rows that need a decision run in internal/processorapi on Anvil. Every test checks at its
// end that the captured log contains no part of the WebSocket URL path, which stands for the provider key.

var debitedTopic = crypto.Keccak256Hash([]byte("Debited(bytes32,address,uint256)"))

var subscriptions = []string{config.SubscriptionPendingLogs, config.SubscriptionLogs}

// delivered is one call of Deliver.
type delivered struct {
	authID common.Hash
	sig    debit.Signal
}

// recorder is a Deliverer that records every signal.
type recorder struct {
	mu  sync.Mutex
	got []delivered
}

func (r *recorder) Deliver(authID common.Hash, sig debit.Signal) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, delivered{authID, sig})
	return true
}

func (r *recorder) all() []delivered {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivered(nil), r.got...)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// running is a listener in its goroutine with its fake server.
type running struct {
	srv        *listenertest.Server
	controller common.Address
	signals    *recorder
	reg        *prometheus.Registry
	logs       *syncBuffer
}

// start runs a listener of subscription against a new fake server, prepared by before. On cleanup it stops the
// listener, checks that the shutdown logged no failure, and checks the log for the URL path.
func start(t *testing.T, subscription string, before ...func(*listenertest.Server)) *running {
	t.Helper()
	r := &running{srv: listenertest.Start(t), controller: common.BytesToAddress(crypto.Keccak256([]byte(t.Name()))),
		signals: &recorder{}, reg: prometheus.NewRegistry(), logs: &syncBuffer{}}
	for _, f := range before {
		f(r.srv)
	}
	logger := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l, err := listener.New(vault.NewSecret(r.srv.URL), listener.Config{Subscription: subscription, Controller: r.controller,
		ChainID: listenertest.ChainID}, r.signals, listener.NewMetrics(r.reg), logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		before := len(r.logs.String())
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after the context ended")
		}
		// Shutdown is quiet: no failure is logged for the cancellation.
		if tail := r.logs.String()[before:]; strings.Contains(tail, `"level":"WARN"`) || strings.Contains(tail, `"level":"ERROR"`) {
			t.Errorf("the shutdown logged a failure:\n%s", tail)
		}
		noURLPath(t, r.logs.String(), r.srv)
	})
	return r
}

// noURLPath fails when out contains the URL of srv or any part of its path.
func noURLPath(t *testing.T, out string, srv *listenertest.Server) {
	t.Helper()
	parts := append([]string{srv.URL, srv.Path}, strings.Split(srv.Path, "/")...)
	for _, p := range parts {
		if len(p) >= 4 && strings.Contains(out, p) {
			t.Error("the log contains a part of the WebSocket URL path")
		}
	}
}

// connected returns chain_listener_connected.
func (r *running) connected(t *testing.T) float64 {
	t.Helper()
	families, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "chain_listener_connected" {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("chain_listener_connected is not registered")
	return 0
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *running) waitConnected(t *testing.T, want float64) {
	t.Helper()
	waitFor(t, 10*time.Second, "chain_listener_connected "+map[float64]string{0: "0", 1: "1"}[want], func() bool {
		return r.connected(t) == want
	})
}

// debitedLog is a Debited log of the controller for authID, as a provider sends it.
func (r *running) debitedLog(authID common.Hash) map[string]any {
	return map[string]any{
		"address":          r.controller,
		"topics":           []common.Hash{debitedTopic, authID, common.BytesToHash(r.controller.Bytes())},
		"data":             "0x" + strings.Repeat("0", 63) + "1",
		"blockNumber":      "0x10",
		"blockHash":        common.Hash{},
		"transactionHash":  common.Hash{1},
		"transactionIndex": "0x0",
		"logIndex":         "0x0",
		"removed":          false,
	}
}

// S2-T601 — Req: ADR-13, FR-23. The subscription: one eth_subscribe with the configured type and the filter.
func TestT601_Subscription(t *testing.T) {
	for _, sub := range subscriptions {
		t.Run(sub, func(t *testing.T) {
			r := start(t, sub)
			r.waitConnected(t, 1)
			calls := r.srv.Calls()
			if len(calls) != 2 || calls[0].Method != "eth_chainId" || calls[1].Method != "eth_subscribe" {
				t.Fatalf("calls %+v, want eth_chainId, then one eth_subscribe", calls)
			}
			var typ string
			var filter struct {
				Address common.Address `json:"address"`
				Topics  []common.Hash  `json:"topics"`
			}
			params := calls[1].Params
			if len(params) != 2 || json.Unmarshal(params[0], &typ) != nil || json.Unmarshal(params[1], &filter) != nil {
				t.Fatalf("eth_subscribe params %s", params)
			}
			if typ != sub || filter.Address != r.controller || len(filter.Topics) != 1 || filter.Topics[0] != debitedTopic {
				t.Errorf("eth_subscribe %q %+v, want %q with the controller and the Debited topic", typ, filter, sub)
			}
			for _, c := range calls {
				for _, p := range c.Params {
					if s := string(p); strings.Contains(s, "newHeads") || strings.Contains(s, "newFlashblocks") {
						t.Errorf("subscription to %s", s)
					}
				}
			}
		})
	}

	t.Run("any other type is refused at start", func(t *testing.T) {
		for _, sub := range []string{"newHeads", "newFlashblocks", "newPendingTransactions", "", "LOGS"} {
			_, err := listener.New(vault.NewSecret("ws://127.0.0.1:1/key"), listener.Config{Subscription: sub},
				&recorder{}, listener.NewMetrics(prometheus.NewRegistry()), slog.New(slog.DiscardHandler))
			if err == nil {
				t.Errorf("subscription %q accepted", sub)
			}
		}
	})
}

// S2-T604 — Req: ADR-13. A log is a signal only from the controller, with topic0 Debited and removed not true.
// The part of Signals (an authId without a waiting debit) runs in internal/processorapi.
func TestT604_OtherLogsIgnored(t *testing.T) {
	for _, sub := range subscriptions {
		t.Run(sub, func(t *testing.T) {
			r := start(t, sub)
			r.waitConnected(t, 1)
			authID := common.Hash{0xAA}

			other := r.debitedLog(authID)
			other["address"] = common.Address{0x01}
			otherTopic := r.debitedLog(authID)
			otherTopic["topics"] = []common.Hash{crypto.Keccak256Hash([]byte("Refunded(bytes32,bytes32,address,uint256)")), authID}
			removed := r.debitedLog(authID)
			removed["removed"] = true
			short := r.debitedLog(authID)
			short["topics"] = []common.Hash{debitedTopic}
			for _, l := range []any{other, otherTopic, removed, short, "not a log", 42} {
				if n := r.srv.Emit(l); n != 1 {
					t.Fatalf("emitted to %d subscriptions", n)
				}
			}
			// The signal after them shows that they were all read.
			r.srv.Emit(r.debitedLog(authID))
			waitFor(t, 5*time.Second, "the signal", func() bool { return len(r.signals.all()) > 0 })
			time.Sleep(50 * time.Millisecond)
			got := r.signals.all()
			if len(got) != 1 || got[0].authID != authID || got[0].sig.Source != debit.SourceSubscription || got[0].sig.Receipt != nil {
				t.Errorf("delivered %+v, want one subscription signal of %s without a receipt", got, authID.Hex())
			}
			if r.connected(t) != 1 {
				t.Error("an ignored log dropped the subscription")
			}
		})
	}
}

// Removed logs are ignored (a log of a reorganised block, ADR-13).
func TestRemovedLogIgnored(t *testing.T) {
	r := start(t, config.SubscriptionLogs)
	r.waitConnected(t, 1)
	removed := r.debitedLog(common.Hash{0xBB})
	removed["removed"] = true
	r.srv.Emit(removed)
	r.srv.Emit(r.debitedLog(common.Hash{0xCC}))
	waitFor(t, 5*time.Second, "the signal", func() bool { return len(r.signals.all()) > 0 })
	if got := r.signals.all(); len(got) != 1 || got[0].authID != (common.Hash{0xCC}) {
		t.Errorf("delivered %+v, want only the log that is not removed", got)
	}
}

// S2-T605 — Req: EC-19. The part of the listener: a lost server sets the metric to 0 and Run goes on.
// The decision by polling while it is down runs in internal/processorapi.
func TestT605_ListenerDown(t *testing.T) {
	for _, sub := range subscriptions {
		t.Run(sub, func(t *testing.T) {
			r := start(t, sub)
			r.waitConnected(t, 1)
			r.srv.Close()
			r.waitConnected(t, 0)
			if out := r.logs.String(); !strings.Contains(out, "chain listener disconnected") {
				t.Errorf("the loss is not logged:\n%s", out)
			}
		})
	}
}

// S2-T606 — Req: ADR-13 common rules. Reconnect with backoff and jitter, resubscribe, ping answered.
func TestT606_Reconnect(t *testing.T) {
	for _, sub := range subscriptions {
		t.Run(sub, func(t *testing.T) {
			r := start(t, sub)
			r.waitConnected(t, 1)
			lost := time.Now()
			r.srv.Close()
			r.waitConnected(t, 0)
			r.srv.Restart()
			r.waitConnected(t, 1)
			if waited := time.Since(lost); waited < time.Duration(float64(listener.BackoffFirst)*(1-listener.BackoffJitter)) {
				t.Errorf("resubscribed %s after the loss: no backoff", waited)
			}
			subs := r.srv.Subscriptions()
			if len(subs) != 2 || string(subs[0][0]) != string(subs[1][0]) || string(subs[0][1]) != string(subs[1][1]) {
				t.Errorf("eth_subscribe calls %s, want the same subscription twice", subs)
			}
			if out := r.logs.String(); !strings.Contains(out, "chain listener reconnecting") {
				t.Errorf("the reconnect is not logged:\n%s", out)
			}

			if r.srv.Ping() != 1 {
				t.Fatal("no connection to ping")
			}
			waitFor(t, 5*time.Second, "the pong", func() bool { return r.srv.Pongs() == 1 })

			r.srv.Emit(r.debitedLog(common.Hash{0xDD}))
			waitFor(t, 5*time.Second, "the signal after the reconnect", func() bool { return len(r.signals.all()) == 1 })
		})
	}

	t.Run("refused subscription is retried", func(t *testing.T) {
		r := start(t, config.SubscriptionLogs)
		r.waitConnected(t, 1)
		r.srv.Refuse(true)
		r.srv.Close()
		r.srv.Restart()
		waitFor(t, 10*time.Second, "a refused eth_subscribe", func() bool { return len(r.srv.Subscriptions()) >= 2 })
		if r.connected(t) != 0 {
			t.Error("chain_listener_connected 1 after a refused subscription")
		}
		r.srv.Refuse(false)
		r.waitConnected(t, 1)
		if out := r.logs.String(); !strings.Contains(out, "chain listener cannot subscribe") || !strings.Contains(out, "JSON-RPC error -32601") {
			t.Errorf("the refusal is not logged:\n%s", out)
		}
	})
}

// The backoff constants: exponential from BackoffFirst, capped at BackoffMax, minus a random share up to
// BackoffJitter.
func TestBackoff(t *testing.T) {
	for _, c := range []struct {
		failures int
		r        float64
		want     time.Duration
	}{
		{0, 0, time.Second},
		{0, 0.5, 750 * time.Millisecond},
		{1, 0, 2 * time.Second},
		{4, 0, 16 * time.Second},
		{5, 0, 30 * time.Second},
		{5, 0.999999, 15*time.Second + 15*time.Microsecond},
		{1000, 0, 30 * time.Second},
	} {
		if got := listener.Backoff(c.failures, c.r); got.Round(time.Microsecond) != c.want {
			t.Errorf("Backoff(%d, %v) = %s, want %s", c.failures, c.r, got, c.want)
		}
	}
}

// S2-T609 — Req: §3.2 Reliability, ADR-13. The listener part: an endpoint of another chain is an alert with both
// chain IDs, no eth_subscribe, the metric stays 0, and the listener retries; the right chain ID then subscribes.
func TestT609_WebSocketChainID(t *testing.T) {
	r := start(t, config.SubscriptionLogs, func(s *listenertest.Server) { s.SetChainID(84532) })
	waitFor(t, 10*time.Second, "the alert", func() bool { return strings.Contains(r.logs.String(), "ALERT: ") })
	out := r.logs.String()
	if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, `"endpoint":"CARD_AUTH_RPC_WS_URL"`) ||
		!strings.Contains(out, `"endpoint_chain_id":84532`) || !strings.Contains(out, `"chain_id":31337`) {
		t.Errorf("the alert has not the level, the endpoint or both chain IDs:\n%s", out)
	}
	if subs := r.srv.Subscriptions(); len(subs) != 0 {
		t.Errorf("eth_subscribe %s on another chain", subs)
	}
	if r.connected(t) != 0 {
		t.Error("chain_listener_connected 1 on another chain")
	}
	waitFor(t, 10*time.Second, "a retry", func() bool { return strings.Count(r.logs.String(), "ALERT: ") >= 2 })
	if subs := r.srv.Subscriptions(); len(subs) != 0 {
		t.Errorf("eth_subscribe %s on another chain", subs)
	}
	r.srv.SetChainID(listenertest.ChainID)
	r.waitConnected(t, 1)
	if subs := r.srv.Subscriptions(); len(subs) != 1 {
		t.Errorf("%d eth_subscribe calls, want 1", len(subs))
	}
}
