package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/listener/listenertest"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// metricsText returns the body of /metrics on the health port.
func metricsText(t *testing.T, s *started) string {
	t.Helper()
	resp, err := client.Get(s.healthURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// authorizeUSD sends one USD authorization of card_A as the pair and returns the decoded answer.
func authorizeUSD(t *testing.T, s *started, pair registry.IssuedPair, authID string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.httpURL+"/v1/authorizations",
		strings.NewReader(`{"auth_id":"`+authID+`","card_ref":"card_A","amount":"1","currency":"USD"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(pair.Username, pair.Password)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// S2-T607 — Req: §3.2 Reliability, FR-23, §2.5.1. Without CARD_AUTH_RPC_WS_URL card-auth starts with one log line,
// chain_listener_connected 0, and decides by polling. With it, main starts the listener on the configured type.
func TestT607_NoWebSocketURL(t *testing.T) {
	c := testchain.Start(t)
	pair := seedCardA(t, c)

	t.Run("unset", func(t *testing.T) {
		s := start(t, testEnv(t, c, testdb.CardAuthURL(t)))
		if r := authorizeUSD(t, s, pair, "auth-607"); r["decision"] != "APPROVED" {
			t.Errorf("authorization: %v, want APPROVED", r)
		}
		m := metricsText(t, s)
		for _, want := range []string{
			"chain_listener_connected 0",
			`inclusion_signals_total{source="polling"} 1`,
			`inclusion_signals_total{source="subscription"} 0`,
		} {
			if !strings.Contains(m, want) {
				t.Errorf("/metrics has no %s", want)
			}
		}
		if err := s.stop(); err != nil {
			t.Fatal(err)
		}
		logs := s.logs.String()
		if n := strings.Count(logs, "CARD_AUTH_RPC_WS_URL is unset"); n != 1 {
			t.Errorf("%d log lines about the unset CARD_AUTH_RPC_WS_URL, want 1", n)
		}
		if strings.Contains(logs, `"msg":"chain listener`) {
			t.Error("a chain listener ran without CARD_AUTH_RPC_WS_URL")
		}
	})

	t.Run("set", func(t *testing.T) {
		ws := listenertest.Start(t)
		env := testEnv(t, c, testdb.CardAuthURL(t))
		env["LOG_LEVEL"] = "debug"
		env["CARD_AUTH_RPC_WS_URL"] = ws.URL
		env["CARD_AUTH_LISTENER_SUBSCRIPTION"] = "logs"
		s := start(t, env)
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(metricsText(t, s), "chain_listener_connected 1") {
			if time.Now().After(deadline) {
				t.Fatalf("chain_listener_connected is not 1 within 10 s\n%s", s.logs.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		if subs := ws.Subscriptions(); len(subs) != 1 || string(subs[0][0]) != `"logs"` {
			t.Errorf("eth_subscribe %s, want one logs subscription", subs)
		}
		if err := s.stop(); err != nil {
			t.Fatal(err)
		}
		logs := s.logs.String()
		if !strings.Contains(logs, "chain listener subscribed") || strings.Contains(logs, "CARD_AUTH_RPC_WS_URL is unset") {
			t.Errorf("the log does not show the listener:\n%s", logs)
		}
		for _, p := range append([]string{ws.URL, ws.Path}, strings.Split(ws.Path, "/")...) {
			if len(p) >= 4 && strings.Contains(logs, p) {
				t.Error("the log contains a part of the WebSocket URL path")
			}
		}
	})
}
