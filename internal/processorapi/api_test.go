package processorapi_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"

	"github.com/DigitLock/crypto-account-service/internal/crs/crstest"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// Phase 4 of docs/test-plan-s2.md, the part of st5a: UC-1 steps 1 to 9, every decline, the status query.
// No code of this stage sends a transaction: every test that reaches the chain checks the operator's nonce.

// S2-T401 — Req: UC-1 step 1, SRS — Card Spend §2.1.1
func TestT401_BasicAuthentication(t *testing.T) {
	e := newEnv(t, options{})
	reg := registry.New(e.owner)
	if _, err := reg.CreateTenant(ctx, "tenant-off"); err != nil {
		t.Fatal(err)
	}
	disabled, err := reg.IssueProcessorCredential(ctx, "tenant-off")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.DisableTenant(ctx, "tenant-off"); err != nil {
		t.Fatal(err)
	}
	revoked, err := reg.IssueProcessorCredential(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RevokeProcessorCredential(ctx, revoked.Username); err != nil {
		t.Fatal(err)
	}
	token, err := reg.IssueToken(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	tokenSecret := token.Value[len(token.Value)-64:]

	pair := func(u, p string) func(*http.Request) { return func(r *http.Request) { r.SetBasicAuth(u, p) } }
	wrongPassword := strings.Repeat("0", 64)
	cases := map[string]func(*http.Request){
		"no header":                         nil,
		"unknown username":                  pair("0123456789ab", e.pairA.Password),
		"wrong password":                    pair(e.pairA.Username, wrongPassword),
		"revoked pair":                      pair(revoked.Username, revoked.Password),
		"pair of a disabled tenant":         pair(disabled.Username, disabled.Password),
		"service token key_id":              pair(token.KeyID, tokenSecret),
		"malformed header":                  func(r *http.Request) { r.Header.Set("Authorization", "Basic !!!") },
		"bearer token":                      func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token.Value) },
		"upper-case password":               pair(e.pairA.Username, strings.ToUpper(e.pairA.Password)),
		"password of another tenant's pair": pair(e.pairA.Username, disabled.Password),
	}
	body := mustJSON(t, authReq("auth-401", "card_A", "1", "USD"))
	var first []byte
	for name, setAuth := range cases {
		t.Run(name, func(t *testing.T) {
			for _, r := range []response{
				e.do(t, http.MethodPost, "/v1/authorizations", body, setAuth),
				e.do(t, http.MethodGet, "/v1/authorizations/auth-401", nil, setAuth),
			} {
				if r.status != http.StatusUnauthorized || r.str("error", "code") != "UNAUTHENTICATED" {
					t.Errorf("answer %d %s, want 401 UNAUTHENTICATED", r.status, r.raw)
				}
				if first == nil {
					first = r.raw
				} else if !bytes.Equal(first, r.raw) {
					t.Errorf("body %s differs from %s: one body for all", r.raw, first)
				}
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations`); n != 0 || e.engine.n.Load() != 0 {
		t.Errorf("%d authorizations stored, %d requests reached the engine", n, e.engine.n.Load())
	}
}

// S2-T711 — Req: SRS — Core UC-105 row 7. The "401 after revocation" part of the row.
func TestT711_RevokedPairIs401(t *testing.T) {
	e := newEnv(t, options{noCRS: true})
	if r := e.get(t, "auth-711"); r.status != http.StatusNotFound {
		t.Fatalf("before revocation: %d %s, want 404", r.status, r.raw)
	}
	if _, err := registry.New(e.owner).RevokeProcessorCredential(ctx, e.pairA.Username); err != nil {
		t.Fatal(err)
	}
	if r := e.get(t, "auth-711"); r.status != http.StatusUnauthorized {
		t.Errorf("GET after revocation: %d %s, want 401", r.status, r.raw)
	}
	if r := e.authorize(t, authReq("auth-711", "card_A", "1", "USD")); r.status != http.StatusUnauthorized {
		t.Errorf("POST after revocation: %d %s, want 401", r.status, r.raw)
	}
}

// S2-T402 — Req: UC-1 step 2, SRS — Card Spend §2.1.1
func TestT402_Validation(t *testing.T) {
	e := newEnv(t, options{noCRS: true})
	valid := func() map[string]any { return authReq("auth-402", "card_A", "25.40", "USD") }
	with := func(k string, v any) map[string]any { b := valid(); b[k] = v; return b }
	without := func(k string) map[string]any { b := valid(); delete(b, k); return b }
	cases := map[string]any{
		"auth_id empty":            with("auth_id", ""),
		"auth_id 65 characters":    with("auth_id", strings.Repeat("a", 65)),
		"auth_id not printable":    with("auth_id", "auth\t402"),
		"auth_id not ASCII":        with("auth_id", "auth-ž"),
		"auth_id a number":         with("auth_id", 402),
		"amount 0":                 with("amount", "0"),
		"amount 0.0000":            with("amount", "0.0000"),
		"amount -1":                with("amount", "-1"),
		"amount abc":               with("amount", "abc"),
		"amount 1e2":               with("amount", "1e2"),
		"amount 5 decimals":        with("amount", "1.23456"),
		"amount leading zero":      with("amount", "01.5"),
		"amount a number":          with("amount", 25.4),
		"currency eur":             with("currency", "eur"),
		"currency EU":              with("currency", "EU"),
		"currency EURO":            with("currency", "EURO"),
		"card_ref missing":         without("card_ref"),
		"card_ref empty":           with("card_ref", ""),
		"card_ref 65 characters":   with("card_ref", strings.Repeat("ž", 65)),
		"amount 15 integer digits": with("amount", "100000000000000"),
		"amount 15 integer digits and a fraction": with("amount", "123456789012345.5"),
		"card_ref null":          with("card_ref", nil),
		"merchant not an object": with("merchant", "Maxi"),
		"not JSON":               "{auth_id: 1",
		"JSON array":             "[]",
		"two JSON values":        `{"auth_id":"a","card_ref":"c","amount":"1","currency":"USD"} {}`,
		"empty body":             "",
		"over the size limit":    with("merchant", map[string]any{"name": strings.Repeat("x", 16<<10)}),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := e.authorize(t, body)
			if r.status != http.StatusUnprocessableEntity || r.str("error", "code") != "INVALID_REQUEST" {
				t.Errorf("answer %d %s, want 422 INVALID_REQUEST", r.status, r.raw)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations`); n != 0 || e.engine.n.Load() != 0 {
		t.Errorf("%d authorizations stored, %d requests reached the engine (and its lock)", n, e.engine.n.Load())
	}

	t.Run("D-17, D-19: 14 integer digits and a card_ref of 64 characters are accepted", func(t *testing.T) {
		r := e.authorize(t, authReq("auth-402-max", strings.Repeat("ž", 64), "99999999999999.9999", "USD"))
		declinedWith(t, r, decision.StatusDeclined, decision.ReasonCardNotFound)
		if got := e.row(t, e.tenantA, "auth-402-max"); got.Amount != "99999999999999.9999" {
			t.Errorf("stored amount %s", got.Amount)
		}
	})

	t.Run("unknown fields are ignored; a body of exactly 16 KiB is accepted", func(t *testing.T) {
		b := valid()
		b["auth_id"], b["card_ref"], b["extra"] = "auth-402-ok", "card_none", map[string]any{"x": 1}
		raw := mustJSON(t, b)
		pad := 16<<10 - len(raw) - len(`,"pad":""`)
		raw = append(raw[:len(raw)-1], []byte(`,"pad":"`+strings.Repeat("p", pad)+`"}`)...)
		if len(raw) != 16<<10 {
			t.Fatalf("body of %d bytes", len(raw))
		}
		declinedWith(t, e.authorize(t, raw), decision.StatusDeclined, decision.ReasonCardNotFound)
	})
}

// S2-T404 — Req: UC-1 step 7, FR-7. The stored quote; the response part is st5b.
func TestT404_StoredQuoteEUR(t *testing.T) {
	e := newEnv(t, options{})
	e.crs.Set("EUR", crstest.Answer{RateDecimal: "1.1642000000"})

	r := e.authorize(t, authReq("auth-404", "card_A", "25.40", "EUR"))
	// The binary of st5a has no Debit step: the fake fails like it, after the quote is stored.
	declinedWith(t, r, decision.StatusDeclined, decision.ReasonInternalError)

	got := e.row(t, e.tenantA, "auth-404")
	if got.TokenAmount != "29866387" || got.Rate != "1.1642000000" || got.BufferBps == nil || *got.BufferBps != 100 ||
		got.Token != "USDC" || got.Amount != "25.4000" || got.Currency != "EUR" {
		t.Errorf("stored quote: token_amount %s, rate %s, buffer_bps %v, token %s, amount %s %s; want 29866387, "+
			"1.1642000000, 100, USDC, 25.4000 EUR", got.TokenAmount, got.Rate, got.BufferBps, got.Token, got.Amount, got.Currency)
	}
	calls := e.debit.calls()
	if len(calls) != 1 || calls[0].TokenAmount.String() != "29866387" || calls[0].Rate != "1.1642" {
		t.Errorf("the Debit step got %+v, want one call with 29866387 at 1.1642", calls)
	}
	if c := e.crs.Calls(); len(c) != 1 || c[0].GetFromCurrency() != "EUR" || c[0].GetToCurrency() != "USD" {
		t.Errorf("CRS calls %v, want one GetRate(EUR → USD): the rate is used as served, no inversion", c)
	}
	e.noTransaction(t)
}

// S2-T405 — Req: SRS — Card Spend §2.1.2 Rate source, D-15
func TestT405_RateDecimalExact(t *testing.T) {
	e := newEnv(t, options{})

	t.Run("rate_decimal parsed exactly; the double is never read", func(t *testing.T) {
		// crstest sends the double 987654.321 with every answer: a quote that read it would be far off.
		e.crs.Set("RSD", crstest.Answer{RateDecimal: "0.0085123457"})
		e.authorize(t, authReq("auth-405-rsd", "card_A", "1234.5678", "RSD"))
		got := e.row(t, e.tenantA, "auth-405-rsd")
		// 1234.5678 × 0.0085123457 × 1.01 × 10^6 = 10614158.5827… exactly → 10614159.
		if got.Rate != "0.0085123457" || got.TokenAmount != "10614159" {
			t.Errorf("stored rate %s, token_amount %s; want 0.0085123457 and 10614159", got.Rate, got.TokenAmount)
		}
	})

	t.Run("quote.rate without trailing zeros", func(t *testing.T) {
		e.crs.Set("CHF", crstest.Answer{RateDecimal: "1.2500000000"})
		e.authorize(t, authReq("auth-405-chf", "card_A", "10", "CHF"))
		if got := e.row(t, e.tenantA, "auth-405-chf"); got.Rate != "1.2500000000" || got.TokenAmount != "12625000" {
			t.Errorf("stored rate %s, token_amount %s; want 1.2500000000 and 12625000", got.Rate, got.TokenAmount)
		}
		r := e.get(t, "auth-405-chf")
		if r.str("quote", "rate") != "1.25" || r.str("quote", "buffer_bps") != "100" || r.str("token_amount") != "12625000" {
			t.Errorf("quote of the status query %s, want rate 1.25, buffer_bps 100", r.raw)
		}
	})

	t.Run("a rate_decimal that does not fit NUMERIC(20,10) is RATE_UNAVAILABLE", func(t *testing.T) {
		for i, bad := range []string{"1.16420000001", "12345678901.5", "1,1642", "-1.1642", "0.0000000000", "1e0", " 1.1642"} {
			e.crs.Set("GBP", crstest.Answer{RateDecimal: bad})
			id := "auth-405-bad-" + string(rune('a'+i))
			declinedWith(t, e.authorize(t, authReq(id, "card_A", "1", "GBP")), decision.StatusDeclined, decision.ReasonRateUnavailable)
		}
	})
	e.noTransaction(t)
}

// S2-T406 — Req: EC-10, FR-9, D-14, D-15
func TestT406_CurrencyAndRateFailures(t *testing.T) {
	c := testchain.Start(t)
	e := newEnv(t, options{chain: c})
	e.crs.Set("EUR", crstest.Answer{RateDecimal: "1.1642000000"})
	e.crs.Set("CHF", crstest.Answer{RateDecimal: "1.2500000000", Outdated: true})
	e.crs.Set("RSD", crstest.Answer{RateDecimal: ""})
	e.crs.Set("GBP", crstest.Answer{RateDecimal: "1.3300000000", Delay: 300 * time.Millisecond})
	e.crs.Set("HUF", crstest.Answer{Code: codes.Unavailable})
	e.crs.Set("PLN", crstest.Answer{Code: codes.Internal})

	cases := []struct{ name, currency, reason string }{
		{"no pair (JPY): CRS NOT_FOUND", "JPY", decision.ReasonCurrencyNotSupported},
		{"is_outdated", "CHF", decision.ReasonRateUnavailable},
		{"empty rate_decimal", "RSD", decision.ReasonRateUnavailable},
		{"answer after the 100 ms budget", "GBP", decision.ReasonRateUnavailable},
		{"CRS UNAVAILABLE", "HUF", decision.ReasonRateUnavailable},
		{"CRS INTERNAL", "PLN", decision.ReasonRateUnavailable},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "auth-406-" + string(rune('a'+i))
			start := time.Now()
			declinedWith(t, e.authorize(t, authReq(id, "card_A", "5", tc.currency)), decision.StatusDeclined, tc.reason)
			if tc.currency == "GBP" && time.Since(start) > time.Second {
				t.Errorf("the answer took %s: the rate budget is 100 ms", time.Since(start))
			}
			if got := e.row(t, e.tenantA, id); got.Rate != "" || got.TokenAmount != "" {
				t.Errorf("a quote was stored: rate %q, token_amount %q", got.Rate, got.TokenAmount)
			}
		})
	}

	t.Run("CRS unreachable", func(t *testing.T) {
		u := newEnv(t, options{chain: c, crsAddr: strings.TrimPrefix(closedURL(t), "http://")})
		declinedWith(t, u.authorize(t, authReq("auth-406-down", "card_A", "5", "EUR")), decision.StatusDeclined, decision.ReasonRateUnavailable)
	})
	t.Run("CRS_ADDRESS unset", func(t *testing.T) {
		u := newEnv(t, options{chain: c, noCRS: true})
		declinedWith(t, u.authorize(t, authReq("auth-406-unset", "card_A", "5", "EUR")), decision.StatusDeclined, decision.ReasonRateUnavailable)
	})
	if n := len(e.debit.calls()); n != 0 {
		t.Errorf("the Debit step was called %d times", n)
	}
	e.noTransaction(t)
}

// S2-T407 — Req: EC-11, FR-8
func TestT407_CardChecks(t *testing.T) {
	e := newEnv(t, options{noCRS: true})
	walletB := e.chain.NewWallet(t)
	e.addCard(t, e.tenantA, "card_B", walletB, usdc(t, "200"))
	e.exec(t, `UPDATE cards SET status = 'FROZEN' WHERE card_ref = 'card_B'`)

	declinedWith(t, e.authorize(t, authReq("auth-407-frozen", "card_B", "5", "USD")), decision.StatusDeclined, decision.ReasonCardFrozen)
	frozen := e.row(t, e.tenantA, "auth-407-frozen")
	if frozen.CardID == nil || common.BytesToAddress(frozen.Wallet) != walletB || frozen.ChainID == nil || *frozen.ChainID != testchain.ChainID {
		t.Errorf("CARD_FROZEN row: card_id %v, wallet %x, chain_id %v; want the card, its wallet and 31337", frozen.CardID, frozen.Wallet, frozen.ChainID)
	}

	declinedWith(t, e.authorize(t, authReq("auth-407-unknown", "card_none", "5", "USD")), decision.StatusDeclined, decision.ReasonCardNotFound)
	if unknown := e.row(t, e.tenantA, "auth-407-unknown"); unknown.CardID != nil || unknown.Wallet != nil {
		t.Errorf("CARD_NOT_FOUND row: card_id %v, wallet %x; want null", unknown.CardID, unknown.Wallet)
	}
	if len(e.debit.calls()) != 0 {
		t.Error("the Debit step was called")
	}
	e.noTransaction(t)
}

// S2-T408 — Req: UC-1 step 8, FR-8. "Approved" of the row is "passes step 8 and reaches the Debit step" in st5a.
func TestT408_CardDailyLimit(t *testing.T) {
	e := newEnv(t, options{noCRS: true})
	e.exec(t, `UPDATE cards SET daily_limit = 30000000 WHERE id = $1`, e.cardA)
	day := time.Date(2026, 10, 5, 23, 59, 58, 0, time.UTC)
	e.clock.Set(day)
	prior := func(authID, status, amount string, at time.Time) {
		e.exec(t, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, card_id, status, token_amount, received_at)
			VALUES ($1, $2, $3, $4, $5, $6::text::numeric, $7)`,
			e.tenantA, authID, decision.ChainAuthID(e.tenantA, authID).Bytes(), e.cardA, status, amount, at)
	}
	prior("auth-408-approved", decision.StatusApproved, "25400000", day.Add(-time.Hour))
	prior("auth-408-timed-out", decision.StatusTimedOut, "10000000", day.Add(-time.Hour))
	prior("auth-408-yesterday", decision.StatusDebitConfirmed, "30000000", day.Add(-24*time.Hour))

	declinedWith(t, e.authorize(t, authReq("auth-408-a", "card_A", "5", "USD")), decision.StatusDeclined, decision.ReasonLimitExceeded)
	if n := len(e.debit.calls()); n != 0 {
		t.Fatalf("25.40 + 5 > 30 reached the Debit step")
	}

	e.authorize(t, authReq("auth-408-b", "card_A", "4", "USD"))
	if n := len(e.debit.calls()); n != 1 {
		t.Errorf("25.40 + 4 ≤ 30 (TIMED_OUT does not count): the Debit step got %d calls, want 1", n)
	}

	e.clock.Set(time.Date(2026, 10, 6, 0, 0, 0, 500_000_000, time.UTC))
	e.authorize(t, authReq("auth-408-c", "card_A", "5", "USD"))
	if n := len(e.debit.calls()); n != 2 {
		t.Errorf("after the UTC day boundary: the Debit step got %d calls, want 2", n)
	}
	e.noTransaction(t)
}

// S2-T409 — Req: EC-3, EC-11, FR-8
func TestT409_OnChainChecks(t *testing.T) {
	e := newEnv(t, options{noCRS: true})

	declinedWith(t, e.authorize(t, authReq("auth-409-funds", "card_A", "100.01", "USD")), decision.StatusDeclined, decision.ReasonInsufficientFunds)

	e.chain.Approve(t, e.wallet, usdc(t, "30"))
	declinedWith(t, e.authorize(t, authReq("auth-409-allowance", "card_A", "40", "USD")), decision.StatusDeclined, decision.ReasonInsufficientAllowance)
	e.chain.Approve(t, e.wallet, usdc(t, "100"))

	declinedWith(t, e.authorize(t, authReq("auth-409-limit", "card_A", "50.01", "USD")), decision.StatusDeclined, decision.ReasonLimitExceeded)

	e.chain.Pause(t)
	// The pause is checked first: this amount is also above the balance.
	declinedWith(t, e.authorize(t, authReq("auth-409-paused", "card_A", "150", "USD")), decision.StatusDeclined, decision.ReasonProgramPaused)
	e.chain.Unpause(t)

	e.authorize(t, authReq("auth-409-pass", "card_A", "50", "USD"))
	if calls := e.debit.calls(); len(calls) != 1 || calls[0].TokenAmount.String() != "50000000" {
		t.Errorf("50 USD within every limit: the Debit step got %+v", calls)
	}
	e.noTransaction(t)
}

// S2-T410 — Req: UC-1 step 9, SRS — Card Spend §3.2 Chain access
func TestT410_ReadBlockTag(t *testing.T) {
	c := testchain.Start(t)
	p := newRPCProxy(t, c.RPCURL)
	e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, noCRS: true})
	p.take()

	e.authorize(t, authReq("auth-410", "card_A", "5", "USD"))
	reqs := p.take()
	if len(reqs) != 1 {
		t.Fatalf("%d RPC requests for one decision, want one batch", len(reqs))
	}
	var batch []struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(reqs[0].body, &batch); err != nil || len(batch) != 4 {
		t.Fatalf("request %s: not a batch of four calls", reqs[0].body)
	}
	for _, call := range batch {
		if call.Method != "eth_call" || len(call.Params) != 2 || string(call.Params[1]) != `"pending"` {
			t.Errorf("call %s %s, want eth_call at the pending tag", call.Method, call.Params)
		}
	}
	if reqs[0].duration >= 500*time.Millisecond {
		t.Errorf("the batch took %s, rpc_read_timeout is 500 ms", reqs[0].duration)
	}
	if len(e.debit.calls()) != 1 {
		t.Error("the read did not reach the Debit step")
	}
}

// S2-T411 — Req: EC-10, FR-9
func TestT411_ChainUnavailable(t *testing.T) {
	c := testchain.Start(t)
	p := newRPCProxy(t, c.RPCURL)
	cases := []struct {
		name string
		url  func() string
		set  func()
	}{
		{"RPC returns an error", func() string { return p.srv.URL }, func() { p.set("error", 0) }},
		{"RPC answers after 600 ms", func() string { return p.srv.URL }, func() { p.set("delay", 600*time.Millisecond) }},
		{"RPC refuses the connection", func() string { return closedURL(t) }, func() {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, options{chain: c, rpcURL: tc.url(), noCRS: true})
			tc.set()
			defer p.set("", 0)
			start := time.Now()
			declinedWith(t, e.authorize(t, authReq("auth-411", "card_A", "5", "USD")), decision.StatusDeclined, decision.ReasonChainUnavailable)
			if d := time.Since(start); d > 2500*time.Millisecond {
				t.Errorf("decision after %s, the deadline is 2.5 s", d)
			}
			if logs := e.logs.String(); !strings.Contains(logs, "chain unavailable") ||
				strings.Contains(logs, p.srv.URL) || strings.Contains(logs, c.RPCURL) {
				t.Errorf("the failure is not logged, or the log contains an RPC URL:\n%s", logs)
			}
			if len(e.debit.calls()) != 0 {
				t.Error("the Debit step was called")
			}
		})
	}
}

// S2-T412 — Req: SRS — Card Spend §3.1, §3.2 Chain access. The listener part of the row is st7.
func TestT412_FallbackEndpoint(t *testing.T) {
	c := testchain.Start(t)
	primary, fallback := newRPCProxy(t, c.RPCURL), newRPCProxy(t, c.RPCURL)
	e := newEnv(t, options{chain: c, rpcURL: primary.srv.URL, fallbackURL: fallback.srv.URL, fallbackAfter: 3,
		probe: 100 * time.Millisecond, noCRS: true})
	primary.take()
	fallback.take()
	n := 0
	authorize := func() response {
		n++
		return e.authorize(t, authReq("auth-412-"+string(rune('a'+n)), "card_A", "1", "USD"))
	}

	primary.set("error", 0)
	declinedWith(t, authorize(), decision.StatusDeclined, decision.ReasonChainUnavailable)
	if len(fallback.take()) != 0 || e.reader.OnFallback() {
		t.Fatal("one failure moved the reads to the fallback")
	}
	declinedWith(t, authorize(), decision.StatusDeclined, decision.ReasonChainUnavailable)
	declinedWith(t, authorize(), decision.StatusDeclined, decision.ReasonChainUnavailable)
	if !e.reader.OnFallback() {
		t.Fatal("three consecutive failures did not move the reads to the fallback")
	}
	primary.take()

	// The primary still fails; the probe fails too, so the reads stay on the fallback.
	authorize()
	if got := len(fallback.take()); got != 1 || len(e.debit.calls()) != 1 {
		t.Errorf("after three failures: %d requests to the fallback, %d Debit calls; want 1 and 1", got, len(e.debit.calls()))
	}
	for _, req := range primary.take() {
		if strings.Contains(string(req.body), "eth_call") {
			t.Error("a read went to the failing primary")
		}
	}

	primary.set("", 0)
	deadline := time.Now().Add(5 * time.Second)
	for e.reader.OnFallback() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if e.reader.OnFallback() {
		t.Fatal("the probe did not move the reads back to the primary")
	}
	primary.take()
	authorize()
	if p, f := len(primary.take()), len(fallback.take()); p != 1 || f != 0 || len(e.debit.calls()) != 2 {
		t.Errorf("after the probe: %d requests to the primary, %d to the fallback; want 1 and 0", p, f)
	}
	if !strings.Contains(e.logs.String(), "chain reads moved to the fallback endpoint") ||
		!strings.Contains(e.logs.String(), "chain reads back on the primary endpoint") {
		t.Errorf("the switches are not logged:\n%s", e.logs.String())
	}
	e.noTransaction(t)
}

// S2-T414 — Req: FR-4, EC-2. On a declined decision; T413 (the approved one) is st5b.
func TestT414_DifferentBody(t *testing.T) {
	e := newEnv(t, options{noCRS: true})
	first := e.authorize(t, `{"auth_id":"auth-414","card_ref":"card_none","amount":"25.40","currency":"USD","merchant":{"name":"Maxi 042","mcc":"5411"}}`)
	declinedWith(t, first, decision.StatusDeclined, decision.ReasonCardNotFound)

	for name, body := range map[string]string{
		"amount 25.41":    `{"auth_id":"auth-414","card_ref":"card_none","amount":"25.41","currency":"USD","merchant":{"name":"Maxi 042","mcc":"5411"}}`,
		"other card_ref":  `{"auth_id":"auth-414","card_ref":"card_A","amount":"25.40","currency":"USD","merchant":{"name":"Maxi 042","mcc":"5411"}}`,
		"other merchant":  `{"auth_id":"auth-414","card_ref":"card_none","amount":"25.40","currency":"USD","merchant":{"name":"Maxi 043","mcc":"5411"}}`,
		"merchant absent": `{"auth_id":"auth-414","card_ref":"card_none","amount":"25.40","currency":"USD"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := e.authorize(t, body)
			if r.status != http.StatusConflict || r.str("error", "code") != "AUTH_ID_CONFLICT" {
				t.Errorf("answer %d %s, want 409 AUTH_ID_CONFLICT", r.status, r.raw)
			}
		})
	}
	for name, body := range map[string]string{
		"keys in another order": `{"currency":"USD","merchant":{"mcc":"5411","name":"Maxi 042"},"amount":"25.40","card_ref":"card_none","auth_id":"auth-414"}`,
		"25.4 for 25.40":        `{"auth_id":"auth-414","card_ref":"card_none","amount":"25.4","currency":"USD","merchant":{"name":"Maxi 042","mcc":"5411"}}`,
		"25.4000, whitespace and an unknown field": `{ "auth_id" : "auth-414", "card_ref":"card_none", "amount":"25.4000",
			"currency":"USD", "merchant":{"mcc":"5411", "name":"Maxi 042"}, "terminal":"T-9" }`,
	} {
		t.Run(name, func(t *testing.T) {
			if r := e.authorize(t, body); !bytes.Equal(r.raw, first.raw) {
				t.Errorf("answer %s, want the stored decision %s", r.raw, first.raw)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations`); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
	if got := e.decisions(t, decision.Declined, decision.ReasonCardNotFound); got != 1 || e.observations(t) != 1 {
		t.Errorf("auth_decisions_total %v, auth_decision_seconds count %d; want 1 and 1: a repeated request is not counted again",
			got, e.observations(t))
	}
}

// S2-T425 — Req: SRS — Card Spend §2.1.1. The failure at step 4; the failure at step 10 is st5b.
func TestT425_InternalError(t *testing.T) {
	e := newEnv(t, options{noCRS: true, db: func(p *pgxpool.Pool) decision.DB {
		return failingDB{Pool: p, match: "InsertReceivedAuthorization"}
	}})
	declinedWith(t, e.authorize(t, authReq("auth-425", "card_A", "5", "USD")), decision.StatusDeclined, decision.ReasonInternalError)
	if n := e.count(t, `SELECT count(*) FROM authorizations`); n != 0 {
		t.Errorf("%d rows stored", n)
	}
	if !strings.Contains(e.logs.String(), "injected failure") || strings.Contains(e.logs.String(), "postgres://") {
		t.Errorf("the failure is not logged, or the log has a connection string:\n%s", e.logs.String())
	}
	if got := e.decisions(t, decision.Declined, decision.ReasonInternalError); got != 1 {
		t.Errorf("auth_decisions_total{DECLINED,INTERNAL_ERROR} = %v, want 1", got)
	}
	e.noTransaction(t)
}

// UC-1 steps 1 to 9, FR-7, FR-8: an authorization that passes every check reaches the Debit step with the
// card's wallet, the chain, the authId of the contract and the quote; the Debit step decides from there.
func TestPassPathReachesDebit(t *testing.T) {
	approve := func(a decision.Checked) (decision.Decision, error) {
		bps := int32(100)
		return decision.Decision{Decision: decision.Approved, Status: decision.StatusApproved, Token: a.Token,
			TokenAmount: a.TokenAmount.String(), Rate: a.Rate, BufferBps: &bps, TxHash: "0x" + strings.Repeat("ab", 32)}, nil
	}
	e := newEnv(t, options{debit: &fakeDebit{reply: approve}})
	e.crs.Set("EUR", crstest.Answer{RateDecimal: "1.1642000000"})
	e.clock.Set(time.Date(2026, 10, 5, 12, 0, 0, 700_000_000, time.UTC))

	r := e.authorize(t, authReq("auth-pass", "card_A", "25.40", "EUR"))
	if r.str("decision") != decision.Approved || r.str("token_amount") != "29866387" || r.str("quote", "rate") != "1.1642" ||
		r.str("token") != "USDC" {
		t.Errorf("answer %s, want the decision of the Debit step", r.raw)
	}
	calls := e.debit.calls()
	if len(calls) != 1 {
		t.Fatalf("the Debit step got %d calls", len(calls))
	}
	got, stored := calls[0], e.row(t, e.tenantA, "auth-pass")
	want := decision.ChainAuthID(e.tenantA, "auth-pass")
	if got.Wallet != e.wallet || got.ChainID != testchain.ChainID || got.ChainAuthID != want || got.CardID != e.cardA ||
		got.Token != "USDC" || got.TenantID != e.tenantA || got.AuthID != "auth-pass" {
		t.Errorf("the Debit step got %+v", got)
	}
	if !got.ReceivedAt.Equal(stored.ReceivedAt) || stored.DeadlineAt == nil || !got.DeadlineAt.Equal(*stored.DeadlineAt) ||
		!got.DeadlineAt.Equal(got.ReceivedAt.Add(2500*time.Millisecond)) {
		t.Errorf("received_at %s, deadline_at %v of the row; the Debit step got %s and %s", stored.ReceivedAt,
			stored.DeadlineAt, got.ReceivedAt, got.DeadlineAt)
	}
	if hex.EncodeToString(stored.ChainAuthID) != hex.EncodeToString(want.Bytes()) || stored.Status != decision.StatusReceived ||
		len(stored.Events) != 1 || stored.Events[0] != ">RECEIVED:" {
		t.Errorf("row: chain_auth_id %x, status %s, events %v; the fake Debit step changed nothing", stored.ChainAuthID,
			stored.Status, stored.Events)
	}
	e.noTransaction(t)
}

// UC-1 step 3, SRS — Card Spend §3.2 Parallel work: requests with the same auth_id while it is decided wait for
// the decision; none is a conflict, one reaches the Debit step. T415 (on chain) is st5b.
func TestStep3_WaitersGetTheDecision(t *testing.T) {
	block := make(chan struct{})
	debit := &fakeDebit{block: block}
	e := newEnv(t, options{noCRS: true, debit: debit})
	body := mustJSON(t, authReq("auth-wait", "card_A", "5", "USD"))

	var wg sync.WaitGroup
	answers := make([]response, 8)
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = e.authorize(t, body)
		}()
		if i == 0 {
			waitFor(t, func() bool { return len(debit.calls()) == 1 })
		}
	}
	// Every request reached the engine; the waiters are waiting for the decision of the first.
	waitFor(t, func() bool { return e.engine.n.Load() == int64(len(answers)) })
	time.Sleep(100 * time.Millisecond)
	close(block)
	wg.Wait()
	for _, a := range answers {
		if !bytes.Equal(a.raw, answers[0].raw) || a.str("decline_reason") != decision.ReasonInternalError {
			t.Errorf("answer %s, want the one decision %s", a.raw, answers[0].raw)
		}
	}
	if n := len(debit.calls()); n != 1 {
		t.Errorf("the Debit step got %d calls, want 1", n)
	}
	if got := e.decisions(t, decision.Declined, decision.ReasonInternalError); got != 1 {
		t.Errorf("auth_decisions_total = %v, want 1", got)
	}
}

// UC-1 step 3, EC-14: a tombstone answers REVERSED_BEFORE_AUTH without a lock, a card or a read. The tombstone
// is written by UC-2 (st6); here it is inserted.
func TestStep3_Tombstone(t *testing.T) {
	e := newEnv(t, options{noCRS: true})
	e.exec(t, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, status, decline_reason, received_at, decided_at)
		VALUES ($1, 'auth-tomb', $2, 'DECLINED', 'REVERSED_BEFORE_AUTH', now(), now())`, e.tenantA,
		decision.ChainAuthID(e.tenantA, "auth-tomb").Bytes())
	declinedWith(t, e.authorize(t, authReq("auth-tomb", "card_A", "5", "USD")), decision.StatusDeclined, decision.ReasonReversedBeforeAuth)
	if len(e.debit.calls()) != 0 {
		t.Error("the tombstone reached the Debit step")
	}
	if got := e.row(t, e.tenantA, "auth-tomb"); got.CardID != nil || got.RequestHash != nil || len(got.Events) != 0 {
		t.Errorf("the tombstone changed: %+v", got)
	}
}

// UC-1 step 5, FR-10: the per-card lock not acquired by the deadline is DECLINED / TIMEOUT. T416 and T417 (with
// a debit in flight on chain) are st5b.
func TestStep5_CardLockTimeout(t *testing.T) {
	block := make(chan struct{})
	debit := &fakeDebit{block: block}
	e := newEnv(t, options{noCRS: true, debit: debit, deadline: time.Second})

	done := make(chan response, 1)
	go func() { done <- e.authorize(t, authReq("auth-lock-1", "card_A", "5", "USD")) }()
	waitFor(t, func() bool { return len(debit.calls()) == 1 })

	start := time.Now()
	r := e.authorize(t, authReq("auth-lock-2", "card_A", "5", "USD"))
	elapsed := time.Since(start)
	declinedWith(t, r, decision.StatusDeclined, decision.ReasonTimeout)
	if elapsed > time.Second+100*time.Millisecond {
		t.Errorf("answered after %s, the deadline is 1 s", elapsed)
	}
	if got := e.row(t, e.tenantA, "auth-lock-2"); got.CardID != nil || got.Events[len(got.Events)-1] != "RECEIVED>DECLINED:TIMEOUT" {
		t.Errorf("row %+v: no card read, history ends RECEIVED → DECLINED / TIMEOUT", got)
	}
	close(block)
	<-done
	if n := len(debit.calls()); n != 1 {
		t.Errorf("the Debit step got %d calls", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// S2-T203 — Req: SRS — Card Spend §2.1. Every response of every test of this package passes the validator of the
// harness; this test shows that the validator refuses what the contract refuses.
func TestT203_ResponsesMatchTheSchema(t *testing.T) {
	e := newEnv(t, options{noCRS: true})
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/authorizations", nil)
	if err != nil {
		t.Fatal(err)
	}
	check := func(status int, body string) error {
		resp := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}}
		return e.validate(req, resp, []byte(body))
	}
	if err := check(200, `{"auth_id":"a","decision":"DECLINED","status":"DECLINED","decline_reason":"CARD_FROZEN"}`); err != nil {
		t.Errorf("a valid decline is refused: %v", err)
	}
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"unknown decision":       {200, `{"auth_id":"a","decision":"MAYBE","status":"DECLINED"}`},
		"unknown decline reason": {200, `{"auth_id":"a","decision":"DECLINED","status":"DECLINED","decline_reason":"NOPE"}`},
		"status missing":         {200, `{"auth_id":"a","decision":"DECLINED"}`},
		"token amount a number":  {200, `{"auth_id":"a","decision":"APPROVED","status":"APPROVED","token_amount":5}`},
		"error code unknown":     {422, `{"error":{"code":"BAD","message":"x"}}`},
		"status not declared":    {500, `{"error":{"code":"INVALID_REQUEST","message":"x"}}`},
		"404 on POST":            {404, `{"error":{"code":"NOT_FOUND","message":"x"}}`},
	} {
		if err := check(c.status, c.body); err == nil {
			t.Errorf("%s: the validator accepted %d %s", name, c.status, c.body)
		}
	}
}

// S2-T521 — Req: SRS — Card Spend §2.1.4, FR-116, FR-105. The part of st5a: a declined authorization under
// cas_card_auth and every 404; the authorizations with returns and the tombstone of the row are st6.
func TestT521_StatusQueryOfADecline(t *testing.T) {
	e := newEnv(t, options{noCRS: true})
	e.clock.Set(time.Date(2026, 10, 2, 12, 0, 0, 120_000_000, time.UTC))
	e.authorize(t, authReq("auth-521", "card_A", "100.01", "USD"))

	r := e.get(t, "auth-521")
	want := map[string]string{
		"auth_id": "auth-521", "status": "DECLINED", "decline_reason": "INSUFFICIENT_FUNDS", "amount": "100.01",
		"currency": "USD", "token": "USDC", "token_amount": "100010000", "debited_amount": "0", "returned_amount": "0",
		"returns": "[]", "quote": "", "tx_hash": "",
	}
	for k, v := range want {
		if got := r.str(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	history, _ := r.body["history"].([]any)
	if len(history) != 2 || r.str("history") != `[{"at":"2026-10-02T12:00:00.120Z","status":"RECEIVED"},{"at":"2026-10-02T12:00:00.120Z","status":"DECLINED"}]` {
		t.Errorf("history %s, want RECEIVED and DECLINED at the clock of the engine", r.str("history"))
	}

	reg := registry.New(e.owner)
	if _, err := reg.CreateTenant(ctx, "tenant-b"); err != nil {
		t.Fatal(err)
	}
	pairB, err := reg.IssueProcessorCredential(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"unknown auth_id":          "auth-unknown",
		"auth_id of 65 characters": strings.Repeat("a", 65),
		"auth_id not printable":    "auth%09521",
		"auth_id not ASCII":        "auth-%C5%BE",
	} {
		t.Run(name, func(t *testing.T) {
			if r := e.get(t, path); r.status != http.StatusNotFound || r.str("error", "code") != "NOT_FOUND" {
				t.Errorf("answer %d %s, want 404 NOT_FOUND", r.status, r.raw)
			}
		})
	}
	t.Run("another tenant's auth_id", func(t *testing.T) {
		r := e.do(t, http.MethodGet, "/v1/authorizations/auth-521", nil, basic(pairB))
		if r.status != http.StatusNotFound || r.str("error", "code") != "NOT_FOUND" {
			t.Errorf("answer %d %s, want 404 NOT_FOUND", r.status, r.raw)
		}
	})
	t.Run("an auth_id with a slash and a space", func(t *testing.T) {
		e.authorize(t, authReq("a/b c", "card_none", "1", "USD"))
		if r := e.get(t, "a%2Fb%20c"); r.status != http.StatusOK || r.str("decline_reason") != "CARD_NOT_FOUND" {
			t.Errorf("answer %d %s, want the authorization", r.status, r.raw)
		}
	})
}

// S2-T202 — Req: SRS — Card Spend §2.1.1, D-18. An internal failure of the status query is 500 with the error
// body INTERNAL and no internal detail, as the OpenAPI states: a failed read of the authorization and a failed
// read of the credentials.
func TestT202_StatusQueryInternalFailure(t *testing.T) {
	for name, match := range map[string]string{
		"reading the authorization": "name: GetAuthorization :one",
		"reading the credentials":   "name: GetProcessorCredentialByKeyID :one",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, options{noCRS: true, readsDB: func(p *pgxpool.Pool) decision.DB {
				return failingDB{Pool: p, match: match}
			}})
			r := e.get(t, "auth-202")
			if r.status != http.StatusInternalServerError || r.str("error", "code") != "INTERNAL" ||
				r.str("error", "message") != "internal error" {
				t.Errorf("answer %d %s, want 500 INTERNAL with the message \"internal error\"", r.status, r.raw)
			}
			if strings.Contains(string(r.raw), "injected") || strings.Contains(string(r.raw), "SQLSTATE") {
				t.Errorf("the body carries an internal detail: %s", r.raw)
			}
		})
	}
}
