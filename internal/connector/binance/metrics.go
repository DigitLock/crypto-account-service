package binance

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the sink of the metrics of the connector (SRS — Binance §2.5.1; X1 D-16). server registers PromMetrics
// with the registration of the connector.
type Metrics interface {
	// TimeOffset sets binance_time_offset_ms: the local clock minus the Binance clock, in milliseconds.
	TimeOffset(ms int64)
	// UsedWeight sets binance_used_weight{budget}: the last used-weight header value of the budget.
	UsedWeight(budget string, used int)
	// RateLimitResponse adds 1 to binance_rate_limit_responses_total{code}: code is 429 or 418.
	RateLimitResponse(code int)
}

// PromMetrics is the Metrics of server.
type PromMetrics struct {
	offset    prometheus.Gauge
	used      *prometheus.GaugeVec
	responses *prometheus.CounterVec
}

var _ Metrics = (*PromMetrics)(nil)

// NewPromMetrics registers the metrics of the connector in reg.
func NewPromMetrics(reg prometheus.Registerer) *PromMetrics {
	m := &PromMetrics{
		offset: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "binance_time_offset_ms", Help: "Local clock minus Binance clock, in milliseconds.",
		}),
		used: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "binance_used_weight", Help: "Last used weight reported by Binance, by budget.",
		}, []string{"budget"}),
		responses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "binance_rate_limit_responses_total", Help: "Limit answers of Binance by HTTP status: 429 or 418.",
		}, []string{"code"}),
	}
	reg.MustRegister(m.offset, m.used, m.responses)
	return m
}

// TimeOffset implements Metrics.
func (m *PromMetrics) TimeOffset(ms int64) { m.offset.Set(float64(ms)) }

// UsedWeight implements Metrics.
func (m *PromMetrics) UsedWeight(budget string, used int) {
	m.used.WithLabelValues(budget).Set(float64(used))
}

// RateLimitResponse implements Metrics.
func (m *PromMetrics) RateLimitResponse(code int) {
	m.responses.WithLabelValues(strconv.Itoa(code)).Inc()
}

// nopMetrics is the sink when none is given.
type nopMetrics struct{}

func (nopMetrics) TimeOffset(int64)       {}
func (nopMetrics) UsedWeight(string, int) {}
func (nopMetrics) RateLimitResponse(int)  {}
