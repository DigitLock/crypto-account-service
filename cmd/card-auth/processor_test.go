package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// S2-T105 — Req: SRS — Card Spend §3.2 Security. The part of st5b: one authorization and one failed send at
// LOG_LEVEL=debug. The RPC URL carries a path like a provider API key; neither the key, nor a URL, nor its host,
// nor the database URL appears in the log. The decisions are counted on the health port.
func TestT105_AuthorizationAndFailedSend(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	c := testchain.Start(t)
	wallet := c.NewWallet(t)
	hundred, err := decision.TokenAmount("100", "", 0, testchain.TokenDecimals)
	if err != nil {
		t.Fatal(err)
	}
	c.Mint(t, wallet, hundred)
	c.Approve(t, wallet, hundred)
	c.SetDailyLimit(t, wallet, hundred)

	reg := registry.New(owner)
	tenant, err := reg.CreateTenant(t.Context(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := reg.IssueProcessorCredential(t.Context(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `WITH conn AS (
			INSERT INTO connections (tenant_id, owner_ref, source_id, external_account)
			VALUES ($1, 'owner-a', (SELECT id FROM sources WHERE code = 'anvil'), $2) RETURNING id)
		INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, status, daily_limit)
		SELECT $1, 'card_A', 'owner-a', id, 'ACTIVE', 200000000 FROM conn`, tenant.ID, wallet.Hex()); err != nil {
		t.Fatal(err)
	}

	// The proxy stands for a provider: its path holds a key, and it can refuse eth_sendRawTransaction.
	var failSend atomic.Bool
	apiKey := randomHex(t, 16)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if failSend.Load() && bytes.Contains(body, []byte(`"eth_sendRawTransaction"`)) {
			var msg struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(body, &msg)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"error":{"code":-32000,"message":"injected send failure"}}`))
			return
		}
		resp, err := http.Post(c.RPCURL, "application/json", bytes.NewReader(body))
		if err != nil {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	env := testEnv(t, c, testdb.CardAuthURL(t))
	env["LOG_LEVEL"] = "debug"
	env["CARD_AUTH_RPC_URL"] = proxy.URL + "/v2/" + apiKey
	s := start(t, env)

	send := func(body string) map[string]any {
		req, err := http.NewRequest(http.MethodPost, s.httpURL+"/v1/authorizations", strings.NewReader(body))
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
	if r := send(`{"auth_id":"auth-1","card_ref":"card_A","amount":"5","currency":"USD"}`); r["decision"] != "APPROVED" {
		t.Errorf("first authorization: %v, want APPROVED", r)
	}
	failSend.Store(true)
	if r := send(`{"auth_id":"auth-2","card_ref":"card_A","amount":"5","currency":"USD"}`); r["status"] != "TIMED_OUT" || r["decline_reason"] != "TIMEOUT" {
		t.Errorf("failed send: %v, want DECLINED / TIMEOUT, status TIMED_OUT", r)
	}

	resp, err := client.Get(s.healthURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`auth_decisions_total{decision="APPROVED",reason=""} 1`,
		`auth_decisions_total{decision="DECLINED",reason="TIMEOUT"} 1`,
		`inclusion_signals_total{source="polling"} 1`,
		`auth_decision_seconds_count 2`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}

	logs := s.logs.String()
	for _, want := range []string{`"msg":"authorization approved"`, `"msg":"operator transaction send failed; treated as sent"`, `"level":"DEBUG"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log has no %s", want)
		}
	}
	key := strings.TrimPrefix(env["OPERATOR_PRIVATE_KEY"], "0x")
	keyBytes, _ := hex.DecodeString(key)
	forbidden := []string{key, strings.ToUpper(key), string(keyBytes), apiKey, proxy.URL, strings.TrimPrefix(proxy.URL, "http://"),
		c.RPCURL, strings.TrimPrefix(c.RPCURL, "http://"), env["CARD_AUTH_DATABASE_URL"], pair.Password}
	if u, err := url.Parse(env["CARD_AUTH_DATABASE_URL"]); err == nil {
		if p, ok := u.User.Password(); ok {
			forbidden = append(forbidden, p)
		}
		forbidden = append(forbidden, u.Host)
	}
	for _, v := range forbidden {
		if len(v) >= 4 && strings.Contains(logs, v) {
			t.Errorf("the log contains a secret, a URL or a host")
		}
	}
}
