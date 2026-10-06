package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// S2-T425 — Req: SRS — Card Spend §2.1.1. The binary of st5a has no Debit step (steps 10 to 13 are st5b): an
// authorization that passes every check is declined as INTERNAL_ERROR, "debit path not built" is logged, and no
// transaction is sent. The decision is counted on the health port; POST …/returns is not routed yet.
func TestT425_PassPathFailsClosed(t *testing.T) {
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

	s := start(t, testEnv(t, c, testdb.CardAuthURL(t)))
	send := func(method, path, body string) (int, map[string]any) {
		req, err := http.NewRequest(method, s.httpURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(pair.Username, pair.Password)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	status, body := send(http.MethodPost, "/v1/authorizations", `{"auth_id":"auth-1","card_ref":"card_A","amount":"5","currency":"USD"}`)
	if status != http.StatusOK || body["decision"] != "DECLINED" || body["decline_reason"] != "INTERNAL_ERROR" {
		t.Errorf("POST = %d %v, want 200 DECLINED / INTERNAL_ERROR", status, body)
	}
	if !strings.Contains(s.logs.String(), `"msg":"debit path not built"`) {
		t.Errorf("no log line \"debit path not built\":\n%s", s.logs.String())
	}
	status, body = send(http.MethodGet, "/v1/authorizations/auth-1", "")
	if status != http.StatusOK || body["status"] != "DECLINED" || body["token_amount"] != "5000000" {
		t.Errorf("GET = %d %v, want the declined authorization with its quote", status, body)
	}
	if status, _ := send(http.MethodPost, "/v1/authorizations/auth-1/returns", `{"return_id":"r-1","type":"REVERSAL"}`); status != http.StatusNotFound {
		t.Errorf("POST …/returns = %d, want 404: returns are st6", status)
	}
	if n := c.TxCount(t, c.Operator); n != 0 {
		t.Errorf("the operator sent %d transactions", n)
	}

	resp, err := client.Get(s.healthURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`auth_decisions_total{decision="DECLINED",reason="INTERNAL_ERROR"} 1`,
		`auth_decision_seconds_count 1`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
}
