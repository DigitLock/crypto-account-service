package binance

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the sink of the metrics of the connector (SRS — Binance §2.5.1). server registers PromMetrics with the
// registration of the connector. binance_used_weight and binance_rate_limit_responses_total come with the limiter
// budgets (X1 D-16).
type Metrics interface {
	// TimeOffset sets binance_time_offset_ms: the local clock minus the Binance clock, in milliseconds.
	TimeOffset(ms int64)
}

// PromMetrics is the Metrics of server.
type PromMetrics struct {
	offset prometheus.Gauge
}

var _ Metrics = (*PromMetrics)(nil)

// NewPromMetrics registers the metrics of the connector in reg.
func NewPromMetrics(reg prometheus.Registerer) *PromMetrics {
	m := &PromMetrics{
		offset: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "binance_time_offset_ms", Help: "Local clock minus Binance clock, in milliseconds.",
		}),
	}
	reg.MustRegister(m.offset)
	return m
}

// TimeOffset implements Metrics.
func (m *PromMetrics) TimeOffset(ms int64) { m.offset.Set(float64(ms)) }

// nopMetrics is the sink when none is given.
type nopMetrics struct{}

func (nopMetrics) TimeOffset(int64) {}
