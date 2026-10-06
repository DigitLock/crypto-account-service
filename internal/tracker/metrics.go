package tracker

import (
	"context"
	"math/big"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Metrics are the return metrics of SRS — Card Spend §2.5.1. Prometheus takes float64: the gauges show base units,
// no amount is computed from them.
type Metrics struct {
	notConfirmed prometheus.Gauge
	capacity     prometheus.Gauge
}

// NewMetrics registers the metrics in reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		notConfirmed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "returns_not_confirmed",
			Help: "Returns in ACCEPTED, SUBMITTED, INCLUDED or RETRYING.",
		}),
		capacity: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "treasury_refund_capacity",
			Help: "min(treasury balance, treasury allowance to the controller), base units of the token.",
		}),
	}
	reg.MustRegister(m.notConfirmed, m.capacity)
	return m
}

// updateMetrics reads both gauges; a failed read keeps the last value and is logged.
func (t *Tracker) updateMetrics(ctx context.Context) {
	n, err := repository.New(t.db).CountReturnsNotConfirmed(ctx)
	if err != nil {
		t.logger.WarnContext(ctx, "tracker: count the returns not confirmed failed", "error", decision.ErrorDetail(err))
	} else {
		t.metrics.notConfirmed.Set(float64(n))
	}
	if t.treasury == ([20]byte{}) {
		return
	}
	token, err := bindings.NewMockUSDCCaller(t.cfg.Token, t.queue.Reader().Endpoint().Client)
	if err != nil {
		return
	}
	balance, err := token.BalanceOf(t.opts(ctx, nil), t.treasury)
	if err != nil {
		t.logger.WarnContext(ctx, "tracker: treasury balance read failed", "error", t.describe(err))
		return
	}
	allowance, err := token.Allowance(t.opts(ctx, nil), t.treasury, t.cfg.Controller)
	if err != nil {
		t.logger.WarnContext(ctx, "tracker: treasury allowance read failed", "error", t.describe(err))
		return
	}
	capacity := balance
	if allowance.Cmp(balance) < 0 {
		capacity = allowance
	}
	f, _ := new(big.Float).SetInt(capacity).Float64()
	t.metrics.capacity.Set(f)
}
