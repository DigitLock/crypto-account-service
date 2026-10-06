package processorapi_test

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/listener/listenertest"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// Phase 6 of docs/test-plan-s2.md, the part with decisions: the chain listener of card-auth on Anvil, with the fake
// WebSocket server of listenertest, which emits the Debited logs the test chooses, and once with the logs
// subscription of Anvil itself. Receipt polling is silenced through the RPC proxy where the subscription must
// decide. Every transaction goes to the local chain 31337. Every scenario ends with the check of the one nonce
// sequence, and every test with a fake server checks that the log holds no part of its URL path (the provider key).

var listenerSubscriptions = []string{config.SubscriptionPendingLogs, config.SubscriptionLogs}

// failing makes the proxy answer every call of method with a JSON-RPC error; "" ends it.
func (p *rpcProxy) failing(method string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failMethod = method
}

// listenerEnv is an env with the Debit step of card-auth, the chain reached through a proxy and the listener on a
// fake WebSocket server.
func listenerEnv(t *testing.T, c *testchain.Chain, sub string) (*env, *rpcProxy, *listenertest.Server) {
	t.Helper()
	ws := listenertest.Start(t)
	p := newRPCProxy(t, c.RPCURL)
	e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true, wsURL: ws.URL, listenerSubscription: sub})
	t.Cleanup(func() { noWSPath(t, e.logs.String(), ws) })
	e.waitListener(t, 1)
	return e, p, ws
}

// noWSPath fails when out contains the URL of ws or any part of its path.
func noWSPath(t *testing.T, out string, ws *listenertest.Server) {
	t.Helper()
	for _, p := range append([]string{ws.URL, ws.Path}, strings.Split(ws.Path, "/")...) {
		if len(p) >= 4 && strings.Contains(out, p) {
			t.Error("the log contains a part of the WebSocket URL path")
		}
	}
}

func (e *env) waitListener(t *testing.T, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.gauge(t, "chain_listener_connected") != want {
		if time.Now().After(deadline) {
			t.Fatalf("chain_listener_connected is not %v within 10 s\n%s", want, e.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// authorizeAsync sends the authorization in a goroutine and returns its answer channel.
func (e *env) authorizeAsync(t *testing.T, authID, amount string) <-chan response {
	t.Helper()
	out := make(chan response, 1)
	go func() { out <- e.authorize(t, authReq(authID, "card_A", amount, "USD")) }()
	return out
}

// minedLog waits until the Debited log of authID is on Anvil and returns it as Anvil serves it.
func (e *env) minedLog(t *testing.T, authID string) map[string]any {
	t.Helper()
	want := decision.ChainAuthID(e.tenantA, authID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var logs []map[string]any
		e.chain.Call(t, &logs, "eth_getLogs", map[string]any{
			"address": e.chain.Controller, "topics": []any{debitedTopic, want}, "fromBlock": "0x0", "toBlock": "latest",
		})
		if len(logs) == 1 {
			return logs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Debited log of %s on chain", authID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// preconfirmed is the log as pendingLogs delivers it: a zero block hash (§4 issue 1). For logs it is unchanged.
func preconfirmed(l map[string]any, sub string) map[string]any {
	out := map[string]any{}
	for k, v := range l {
		out[k] = v
	}
	if sub == config.SubscriptionPendingLogs {
		out["blockHash"] = common.Hash{}
	}
	return out
}

func (e *env) events(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM authorization_events`)
}

func waitAnswer(t *testing.T, ch <-chan response) response {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no answer within 10 s")
		return response{}
	}
}

// S2-T601 — Req: ADR-13, FR-23, decision 3. The subscription with the env of card-auth: one eth_subscribe with
// the configured type and the filter of the controller and Debited; the unit part is in internal/listener.
func TestT601_SubscriptionOfCardAuth(t *testing.T) {
	c := testchain.Start(t)
	for _, sub := range listenerSubscriptions {
		t.Run(sub, func(t *testing.T) {
			e, _, ws := listenerEnv(t, c, sub)
			subs := ws.Subscriptions()
			if len(subs) != 1 || string(subs[0][0]) != `"`+sub+`"` ||
				!strings.Contains(strings.ToLower(string(subs[0][1])), strings.ToLower(e.chain.Controller.Hex())) ||
				!strings.Contains(string(subs[0][1]), debitedTopic.Hex()) {
				t.Errorf("eth_subscribe %s, want one %s with the controller and the Debited topic", subs, sub)
			}
			if calls := ws.Calls(); len(calls) != 2 || calls[0].Method != "eth_chainId" || calls[1].Method != "eth_subscribe" {
				t.Errorf("calls %+v, want eth_chainId, then eth_subscribe", calls)
			}
		})
	}
}

// S2-T602, S2-T608 — Req: FR-2, FR-23, ADR-13, UC-3 row 10. Polling answers no receipt; the log decides at once.
// The log carries a wrong hash, block and amount: the row keeps those of the sent debit, and the block comes later
// from the sealed receipt the tracker reads. The later receipt and a repeated log change nothing.
func TestT602_T608_SignalFromTheSubscription(t *testing.T) {
	c := testchain.Start(t)
	for _, sub := range listenerSubscriptions {
		t.Run(sub, func(t *testing.T) {
			e, p, ws := listenerEnv(t, c, sub)
			p.failing("eth_getTransactionReceipt")
			authID := "auth-602-" + sub
			answer := e.authorizeAsync(t, authID, "3")
			mined := e.minedLog(t, authID)

			fake := preconfirmed(mined, sub)
			fake["transactionHash"] = common.Hash{0x60, 0x8}
			fake["blockNumber"] = "0x9999"
			fake["data"] = hexutil.Encode(common.BigToHash(big.NewInt(999)).Bytes())
			if ws.Emit(fake) != 1 {
				t.Fatal("the log reached no subscription")
			}
			r := waitAnswer(t, answer)
			approvedWith(t, r, "3000000")
			if got := e.signals(t, "subscription"); got != 1 {
				t.Errorf("inclusion_signals_total{subscription} = %v, want 1", got)
			}
			if got := e.signals(t, "polling"); got != 0 {
				t.Errorf("inclusion_signals_total{polling} = %v, want 0", got)
			}
			// S2-T608: the hash is the sent one, the amount the request's, no block from the log.
			txs := e.opTxs(t, authID)
			if len(txs) != 1 || txs[0].Status != "INCLUDED" || "0x"+hex.EncodeToString(txs[0].TxHash) != r.str("tx_hash") ||
				txs[0].BlockNumber != nil || txs[0].BlockHash != nil {
				t.Fatalf("operator_txs %+v, want INCLUDED with the sent hash %s and no block", txs, r.str("tx_hash"))
			}
			if r.str("tx_hash") != mined["transactionHash"] {
				t.Errorf("answer tx_hash %s, the mined transaction %v", r.str("tx_hash"), mined["transactionHash"])
			}
			if n := e.count(t, `SELECT count(*) FROM authorizations WHERE auth_id = $1 AND debited_amount = 3000000`, authID); n != 1 {
				t.Error("debited_amount is not the amount of the sent debit")
			}
			events := e.events(t)

			// The later receipt and the same log again change nothing.
			p.failing("")
			ws.Emit(fake)
			time.Sleep(300 * time.Millisecond)
			if got := e.signals(t, "polling"); got != 0 {
				t.Errorf("inclusion_signals_total{polling} = %v after the receipt, want 0", got)
			}
			if got := e.events(t); got != events {
				t.Errorf("%d events after the later signals, want %d", got, events)
			}

			// UC-3 row 10: the tracker fills the block from the sealed receipt.
			e.tracker.Cycle(ctx)
			txs = e.opTxs(t, authID)
			wantBlock, _ := hexutil.DecodeUint64(mined["blockNumber"].(string))
			if len(txs) != 1 || txs[0].BlockNumber == nil || uint64(*txs[0].BlockNumber) != wantBlock ||
				common.BytesToHash(txs[0].BlockHash) != common.HexToHash(mined["blockHash"].(string)) {
				t.Errorf("operator_txs %+v, want the block %d of the sealed receipt", txs, wantBlock)
			}
			if row := e.row(t, e.tenantA, authID); row.Status != decision.StatusApproved {
				t.Errorf("row %s, want APPROVED", row.Status)
			}
			e.noNonceGap(t)
		})
	}
}

// S2-T603 — Req: FR-23. The fake server stays silent: the receipt decides; a log arriving later is ignored.
func TestT603_SignalFromPolling(t *testing.T) {
	c := testchain.Start(t)
	for _, sub := range listenerSubscriptions {
		t.Run(sub, func(t *testing.T) {
			e, _, ws := listenerEnv(t, c, sub)
			authID := "auth-603-" + sub
			approvedWith(t, e.authorize(t, authReq(authID, "card_A", "2", "USD")), "2000000")
			if p, s := e.signals(t, "polling"), e.signals(t, "subscription"); p != 1 || s != 0 {
				t.Errorf("inclusion_signals_total polling %v, subscription %v; want 1 and 0", p, s)
			}
			txs := e.opTxs(t, authID)
			if len(txs) != 1 || txs[0].Status != "INCLUDED" || txs[0].BlockNumber == nil {
				t.Errorf("operator_txs %+v, want INCLUDED with the block of the receipt", txs)
			}
			events := e.events(t)
			ws.Emit(preconfirmed(e.minedLog(t, authID), sub))
			time.Sleep(300 * time.Millisecond)
			if got := e.events(t); got != events || e.signals(t, "subscription") != 0 {
				t.Errorf("the later log changed the state: %d events, want %d", got, events)
			}
			e.noNonceGap(t)
		})
	}
}

// S2-T604 — Req: ADR-13. Logs of another authId, of another contract, of another event, removed, and of an authId
// with no waiting debit are ignored without a state change; the right log then decides.
func TestT604_OtherLogsIgnored(t *testing.T) {
	c := testchain.Start(t)
	for _, sub := range listenerSubscriptions {
		t.Run(sub, func(t *testing.T) {
			e, p, ws := listenerEnv(t, c, sub)

			// No debit waits: an earlier authorization, decided by polling.
			done := "auth-604-done-" + sub
			approvedWith(t, e.authorize(t, authReq(done, "card_A", "1", "USD")), "1000000")
			doneLog := preconfirmed(e.minedLog(t, done), sub)

			p.failing("eth_getTransactionReceipt")
			authID := "auth-604-" + sub
			answer := e.authorizeAsync(t, authID, "1")
			mined := preconfirmed(e.minedLog(t, authID), sub)
			events := e.events(t)

			otherAuth := preconfirmed(mined, sub)
			topics := append([]any(nil), mined["topics"].([]any)...)
			topics[1] = common.Hash{0x60, 0x4}.Hex()
			otherAuth["topics"] = topics
			otherAddress := preconfirmed(mined, sub)
			otherAddress["address"] = e.chain.Token
			otherEvent := preconfirmed(mined, sub)
			otherEvent["topics"] = append([]any{common.Hash{0x01}.Hex()}, mined["topics"].([]any)[1:]...)
			removed := preconfirmed(mined, sub)
			removed["removed"] = true
			for _, l := range []map[string]any{otherAuth, otherAddress, otherEvent, removed, doneLog} {
				ws.Emit(l)
			}
			time.Sleep(300 * time.Millisecond)
			if row := e.row(t, e.tenantA, authID); row.Status != decision.StatusDebitSubmitted {
				t.Fatalf("after the other logs: %s, want DEBIT_SUBMITTED", row.Status)
			}
			if got := e.events(t); got != events {
				t.Errorf("%d events after the other logs, want %d", got, events)
			}
			if s := e.signals(t, "subscription"); s != 0 {
				t.Errorf("inclusion_signals_total{subscription} = %v after the other logs", s)
			}

			ws.Emit(mined)
			approvedWith(t, waitAnswer(t, answer), "1000000")
			if s := e.signals(t, "subscription"); s != 1 {
				t.Errorf("inclusion_signals_total{subscription} = %v, want 1", s)
			}
			p.failing("")
			e.noNonceGap(t)
		})
	}
}

// S2-T605, S2-T606 — Req: EC-19, FR-23, ADR-13 common rules. The server goes away: the metric is 0 and polling
// decides without an error. The server comes back on the same address: the listener resubscribes and decides again.
func TestT605_T606_ListenerDownAndBack(t *testing.T) {
	c := testchain.Start(t)
	for _, sub := range listenerSubscriptions {
		t.Run(sub, func(t *testing.T) {
			e, p, ws := listenerEnv(t, c, sub)
			ws.Close()
			e.waitListener(t, 0)

			down := "auth-605-" + sub
			approvedWith(t, e.authorize(t, authReq(down, "card_A", "1", "USD")), "1000000")
			if p, s := e.signals(t, "polling"), e.signals(t, "subscription"); p != 1 || s != 0 {
				t.Errorf("listener down: inclusion_signals_total polling %v, subscription %v; want 1 and 0", p, s)
			}

			ws.Restart()
			e.waitListener(t, 1)
			if n := len(ws.Subscriptions()); n != 2 {
				t.Errorf("%d eth_subscribe calls, want 2: the first and the one after the reconnect", n)
			}
			if ws.Ping() != 1 {
				t.Fatal("no connection to ping")
			}
			deadline := time.Now().Add(5 * time.Second)
			for ws.Pongs() != 1 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if ws.Pongs() != 1 {
				t.Error("the ping was not answered")
			}

			p.failing("eth_getTransactionReceipt")
			back := "auth-606-" + sub
			answer := e.authorizeAsync(t, back, "1")
			ws.Emit(preconfirmed(e.minedLog(t, back), sub))
			approvedWith(t, waitAnswer(t, answer), "1000000")
			if s := e.signals(t, "subscription"); s != 1 {
				t.Errorf("after the reconnect: inclusion_signals_total{subscription} = %v, want 1", s)
			}
			p.failing("")
			e.noNonceGap(t)
		})
	}
}

// S2-T412 — Req: §3.1, §3.2 Chain access. The rest of the row: after three failures of the primary the reads and
// sends go to the fallback; the listener never moves: it keeps its one subscription on rpc_ws_url and decides
// the debit sent through the fallback.
func TestT412_ListenerNeverMoves(t *testing.T) {
	c := testchain.Start(t)
	primary, fallback := newRPCProxy(t, c.RPCURL), newRPCProxy(t, c.RPCURL)
	ws := listenertest.Start(t)
	e := newEnv(t, options{chain: c, rpcURL: primary.srv.URL, fallbackURL: fallback.srv.URL, fallbackAfter: 3,
		probe: 100 * time.Millisecond, noCRS: true, realDebit: true, wsURL: ws.URL})
	t.Cleanup(func() { noWSPath(t, e.logs.String(), ws) })
	e.waitListener(t, 1)

	primary.set("error", 0)
	for i := range 3 {
		declinedWith(t, e.authorize(t, authReq("auth-412-fail-"+string(rune('a'+i)), "card_A", "1", "USD")),
			decision.StatusDeclined, decision.ReasonChainUnavailable)
	}
	if !e.reader.OnFallback() {
		t.Fatal("three consecutive failures did not move the reads to the fallback")
	}
	if e.gauge(t, "chain_listener_connected") != 1 || len(ws.Subscriptions()) != 1 {
		t.Errorf("the listener moved: connected %v, %d eth_subscribe calls", e.gauge(t, "chain_listener_connected"), len(ws.Subscriptions()))
	}

	// Polling on the fallback answers no receipt: the listener on rpc_ws_url decides.
	fallback.failing("eth_getTransactionReceipt")
	fallback.take()
	answer := e.authorizeAsync(t, "auth-412-ws", "1")
	ws.Emit(e.minedLog(t, "auth-412-ws"))
	approvedWith(t, waitAnswer(t, answer), "1000000")
	if !e.reader.OnFallback() {
		t.Error("the reads left the fallback while the primary fails")
	}
	sent := false
	for _, req := range fallback.take() {
		sent = sent || strings.Contains(string(req.body), "eth_sendRawTransaction")
	}
	if !sent {
		t.Error("the debit was not sent through the fallback")
	}
	if s := e.signals(t, "subscription"); s != 1 {
		t.Errorf("inclusion_signals_total{subscription} = %v, want 1", s)
	}
	if e.gauge(t, "chain_listener_connected") != 1 || len(ws.Subscriptions()) != 1 {
		t.Errorf("the listener moved: connected %v, %d eth_subscribe calls", e.gauge(t, "chain_listener_connected"), len(ws.Subscriptions()))
	}
	fallback.failing("")
	primary.set("", 0)
	e.noNonceGap(t)
}

// End to end with the logs subscription of Anvil's own WebSocket endpoint: receipt polling is made long and its
// reads fail, so the subscription decides; the tracker fills the block later from the sealed receipt.
func TestListenerOnAnvil(t *testing.T) {
	c := testchain.Start(t)
	p := newRPCProxy(t, c.RPCURL)
	e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true, pollInterval: time.Minute,
		wsURL: "ws://" + strings.TrimPrefix(c.RPCURL, "http://"), listenerSubscription: config.SubscriptionLogs})
	e.waitListener(t, 1)

	p.failing("eth_getTransactionReceipt")
	r := e.authorize(t, authReq("auth-anvil-ws", "card_A", "4", "USD"))
	approvedWith(t, r, "4000000")
	if s, pl := e.signals(t, "subscription"), e.signals(t, "polling"); s != 1 || pl != 0 {
		t.Errorf("inclusion_signals_total subscription %v, polling %v; want 1 and 0", s, pl)
	}
	txs := e.opTxs(t, "auth-anvil-ws")
	if len(txs) != 1 || txs[0].Status != "INCLUDED" || txs[0].BlockNumber != nil {
		t.Fatalf("operator_txs %+v, want INCLUDED without a block", txs)
	}

	p.failing("")
	e.tracker.Cycle(ctx)
	mined := e.minedLog(t, "auth-anvil-ws")
	wantBlock, _ := hexutil.DecodeUint64(mined["blockNumber"].(string))
	txs = e.opTxs(t, "auth-anvil-ws")
	if len(txs) != 1 || txs[0].BlockNumber == nil || uint64(*txs[0].BlockNumber) != wantBlock || len(txs[0].BlockHash) != 32 {
		t.Errorf("operator_txs %+v, want the block %d filled by the tracker", txs, wantBlock)
	}
	e.noNonceGap(t)
}

// S2-T609 — Req: §3.2 Reliability, ADR-13, FR-23. The WebSocket endpoint serves another chain: the alert is logged,
// no eth_subscribe is made, chain_listener_connected stays 0 and polling decides. Then it serves chain_id: the
// listener subscribes and the metric is 1.
func TestT609_WebSocketChainID(t *testing.T) {
	c := testchain.Start(t)
	for _, sub := range listenerSubscriptions {
		t.Run(sub, func(t *testing.T) {
			ws := listenertest.Start(t)
			ws.SetChainID(84532)
			e := newEnv(t, options{chain: c, realDebit: true, noCRS: true, wsURL: ws.URL, listenerSubscription: sub})
			t.Cleanup(func() { noWSPath(t, e.logs.String(), ws) })
			deadline := time.Now().Add(10 * time.Second)
			for !strings.Contains(e.logs.String(), "ALERT: the WebSocket endpoint of the chain listener serves another chain") {
				if time.Now().After(deadline) {
					t.Fatalf("no alert within 10 s\n%s", e.logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}

			authID := "auth-609-" + sub
			approvedWith(t, e.authorize(t, authReq(authID, "card_A", "1", "USD")), "1000000")
			if p, s := e.signals(t, "polling"), e.signals(t, "subscription"); p != 1 || s != 0 {
				t.Errorf("inclusion_signals_total polling %v, subscription %v; want 1 and 0", p, s)
			}
			if subs := ws.Subscriptions(); len(subs) != 0 {
				t.Errorf("eth_subscribe %s on another chain", subs)
			}
			if got := e.gauge(t, "chain_listener_connected"); got != 0 {
				t.Errorf("chain_listener_connected %v on another chain", got)
			}

			ws.SetChainID(testchain.ChainID)
			e.waitListener(t, 1)
			if subs := ws.Subscriptions(); len(subs) != 1 || string(subs[0][0]) != `"`+sub+`"` {
				t.Errorf("eth_subscribe %s, want one %s", subs, sub)
			}
			e.noNonceGap(t)
		})
	}
}
