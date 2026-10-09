package binance

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// X1-T111 — Req: package st2 exit; X1 D-20. A signed GET /api/v3/account with omitZeroBalances=true on the test
// network: 200 and a uid present. Only make binance-live runs it (-live); the owner runs it. It prints the status,
// the time offset and the count of balances: never the uid, a balance, the key or a URL.
func TestT111_SignedAccountOnTestnet(t *testing.T) {
	if !*live {
		t.Skip("live test on the Binance test network: make binance-live")
	}
	base, key := testnet(t)
	reg := prometheus.NewRegistry()
	c := New(NewPromMetrics(reg), nil)
	src := source(`{"base_url": "` + base + `"}`)
	s := mustSession(t, c, src, key, realLimiter(t, c, src))

	body, err := s.call(context.Background(), endpointAccount, param{"omitZeroBalances", "true"})
	if err != nil {
		t.Fatal(err)
	}
	a := parseAccount(t, body)
	if a.UID == nil || a.UID.String() == "" {
		t.Fatal("the account answer has no uid")
	}
	t.Logf("status 200; uid present (not printed)")
	t.Logf("time offset (local minus Binance): %d ms", c.clockOf(base).get().Milliseconds())
	t.Logf("balances: %d", len(a.Balances))
	if got := gauge(t, reg, "binance_time_offset_ms"); int64(got) != c.clockOf(base).get().Milliseconds() {
		t.Errorf("binance_time_offset_ms = %v, want the offset read", got)
	}
}
