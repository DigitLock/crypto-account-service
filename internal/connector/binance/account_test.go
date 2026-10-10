package binance

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
)

// X1-T302, X1-T303, X1-T304 — Req: FR-201, EC-201, EC-214; X1 D-1. The connector part of the permission rule on the
// answer of apiRestrictions: every boolean field whose name starts with enable or permits is a permission; only
// enableReading and enableFixReadOnly may be true; the names come sorted; other fields are not permissions. The
// rows through CreateConnection are in internal/grpc/api.
func TestT302_PermissionRule(t *testing.T) {
	for _, c := range []struct {
		name, body string
		reading    bool
		beyond     []string
		ip         *bool
	}{
		{"read only", `{"ipRestrict":false,"createTime":1790000000000,"enableReading":true,"enableWithdrawals":false}`, true, nil, ptr(false)},
		{"nine beyond reading", `{"enableReading":true,"enableWithdrawals":true,"enableInternalTransfer":true,
			"permitsUniversalTransfer":true,"enableSpotAndMarginTrading":true,"enableMargin":true,"enableFutures":true,
			"enableVanillaOptions":true,"enablePortfolioMarginTrading":true,"enableFixApiTrade":true}`, true,
			[]string{"enableFixApiTrade", "enableFutures", "enableInternalTransfer", "enableMargin", "enablePortfolioMarginTrading",
				"enableSpotAndMarginTrading", "enableVanillaOptions", "enableWithdrawals", "permitsUniversalTransfer"}, nil},
		{"unknown true", `{"enableReading":true,"permitsNewThing":true,"enableNewThing":true}`, true,
			[]string{"enableNewThing", "permitsNewThing"}, nil},
		{"unknown false", `{"enableReading":true,"enableNewThing":false}`, true, nil, nil},
		{"not permissions", `{"enableReading":true,"ipRestrict":true,"createTime":1,"enableNote":"yes","permitsLevel":3,
			"enableNull":null,"tradeEnabled":true,"enableFixReadOnly":true}`, true, nil, ptr(true)},
		{"reading false", `{"enableReading":false}`, false, nil, nil},
		{"reading missing", `{"enableWithdrawals":true}`, false, []string{"enableWithdrawals"}, nil},
		{"reading not a boolean", `{"enableReading":"true","ipRestrict":"no"}`, false, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := parseRestrictions([]byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if r.reading != c.reading || !slices.Equal(r.beyond, c.beyond) || (r.ipRestrict == nil) != (c.ip == nil) ||
				(c.ip != nil && *r.ipRestrict != *c.ip) {
				t.Errorf("parseRestrictions = %+v (ip %v), want reading %v, beyond %v, ip %v", r, r.ipRestrict, c.reading, c.beyond, c.ip)
			}
		})
	}
	for _, body := range []string{`[]`, `null`, `"x"`, `{`} {
		if _, err := parseRestrictions([]byte(body)); err == nil || strings.Contains(err.Error(), body) {
			t.Errorf("parseRestrictions(%s) = %v; want an error that does not quote the body", body, err)
		}
	}
	for body, want := range map[string]string{`{"uid":354937868}`: "354937868", `{"uid": 100000001, "x": 1}`: "100000001"} {
		if got, err := parseUID([]byte(body)); err != nil || got != want {
			t.Errorf("parseUID(%s) = %q, %v", body, got, err)
		}
	}
	for _, body := range []string{`{}`, `{"uid":"354937868"}`, `{"uid":-1}`, `{"uid":1.5}`, `{"uid":null}`, `[]`} {
		if _, err := parseUID([]byte(body)); err == nil {
			t.Errorf("parseUID(%s) accepted", body)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// X1-T306, X1-T107 — Req: EC-202, SRS — Binance §3.2 Security; X1 D-3. The connector part: a wallet is refused
// without a request; no limiter is an error of the caller; the errors of a rejected key check hold no key, secret,
// signature or body of the answer.
func TestT306_CheckAccountErrors(t *testing.T) {
	ctx := context.Background()
	c := New(nil, nil)
	src := source(`{}`)
	if _, err := c.CheckAccount(ctx, src, connector.Credentials{WalletAddress: "0x00000000000000000000000000000000000000aa"}, &recLimiter{}); err == nil {
		t.Error("a wallet was accepted")
	} else if invalid := (*connector.InvalidInputError)(nil); !errors.As(err, &invalid) {
		t.Errorf("wallet: %v, want InvalidInputError", err)
	}
	if _, err := c.CheckAccount(ctx, src, connector.Credentials{ExchangeKey: testKey(t)}, nil); !errors.Is(err, errNoLimiter) {
		t.Errorf("no limiter: %v", err)
	}

	for _, name := range []string{"key_rejected.json", "key_account_rejected.json", "key_reading_disabled.json", "key_not_read_only.json"} {
		srv := httpfixture.ServeFile(t, fixturesDir+"/"+name, weights)
		h := newHarness(t, srv.URL(), nil)
		_, err := h.c.CheckAccount(ctx, source(`{"base_url": "`+srv.URL()+`"}`), connector.Credentials{ExchangeKey: h.s.key}, h.lim)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		srv.AssertAllServed()
		var signatures []string
		for _, r := range srv.Received() {
			if m := signatureParam.FindStringSubmatch(r.RawQuery); m != nil {
				signatures = append(signatures, m[1])
			}
		}
		for _, s := range append([]string{h.s.key.APIKey.Value(), h.s.key.APISecret.Value(), "markerkey", "markersecret",
			"signature", "Invalid API-key", "msg", "{"}, signatures...) {
			if strings.Contains(err.Error(), s) || strings.Contains(h.log.String(), h.s.key.APIKey.Value()) {
				t.Errorf("%s: the error or the log holds %q: %v", name, s, err)
			}
		}
	}
}
