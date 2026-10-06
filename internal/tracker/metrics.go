package tracker

import (
	"context"
	"log/slog"
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
	lateDebits   prometheus.Counter
	debitsLost   prometheus.Counter
	pending      prometheus.Gauge
	gasBalance   prometheus.Gauge
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
	m.lateDebits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "late_debits_total", Help: "Debits that landed after a decline (UC-3 row 4).",
	})
	m.debitsLost = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "debits_lost_total", Help: "Approved debits that were dropped and could not be repeated (UC-3 row 3).",
	})
	m.pending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "operator_tx_pending_seconds", Help: "Age of the oldest operator transaction not mined; 0 when none.",
	})
	m.gasBalance = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "operator_gas_balance", Help: "Native balance of the operator, wei.",
	})
	reg.MustRegister(m.notConfirmed, m.capacity, m.lateDebits, m.debitsLost, m.pending, m.gasBalance)
	return m
}

// updateMetrics reads both gauges; a failed read keeps the last value and is logged.
func (t *Tracker) updateMetrics(ctx context.Context) {
	n, err := repository.New(t.db).CountReturnsNotConfirmed(ctx, t.chainID())
	if err != nil {
		t.failed(ctx, slog.LevelWarn, "tracker: count the returns not confirmed failed", "error", decision.ErrorDetail(err))
	} else {
		t.metrics.notConfirmed.Set(float64(n))
	}
	if oldest, err := repository.New(t.db).OldestUnminedOperatorTx(ctx, repository.OldestUnminedOperatorTxParams{
		ChainID: int64(t.queue.ChainID()), OperatorAddress: t.operator(),
	}); err != nil {
		t.failed(ctx, slog.LevelWarn, "tracker: the oldest unmined transaction not read", "error", decision.ErrorDetail(err))
	} else if oldest.Unix() == 0 {
		t.metrics.pending.Set(0)
	} else {
		t.metrics.pending.Set(t.now().Sub(oldest).Seconds())
	}
	if wei, err := t.queue.Balance(ctx); err != nil {
		t.failed(ctx, slog.LevelWarn, "tracker: the operator balance not read", "error", err.Error())
	} else {
		f, _ := new(big.Float).SetInt(wei).Float64()
		t.metrics.gasBalance.Set(f)
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
		t.failed(ctx, slog.LevelWarn, "tracker: treasury balance read failed", "error", t.describe(err))
		return
	}
	allowance, err := token.Allowance(t.opts(ctx, nil), t.treasury, t.cfg.Controller)
	if err != nil {
		t.failed(ctx, slog.LevelWarn, "tracker: treasury allowance read failed", "error", t.describe(err))
		return
	}
	capacity := balance
	if allowance.Cmp(balance) < 0 {
		capacity = allowance
	}
	f, _ := new(big.Float).SetInt(capacity).Float64()
	t.metrics.capacity.Set(f)
}
