package processorapi

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/decision"
)

// Metrics are the decision metrics of SRS — Card Spend §2.5.1.
type Metrics struct {
	seconds   prometheus.Histogram
	decisions *prometheus.CounterVec
}

// NewMetrics registers the metrics in reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		seconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "auth_decision_seconds",
			Help:    "Time from an authorization request received to its decision sent.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 2.5, 3, 5},
		}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "auth_decisions_total",
			Help: "Authorization decisions by decision and decline reason; reason is empty for an approval.",
		}, []string{"decision", "reason"}),
	}
	reg.MustRegister(m.seconds, m.decisions)
	return m
}

// observe records a decision this request made, after its response was sent. A repeated request answered from
// the stored decision is not counted again.
func (m *Metrics) observe(start time.Time, res decision.Result) {
	if m == nil || !res.Fresh {
		return
	}
	m.seconds.Observe(time.Since(start).Seconds())
	m.decisions.WithLabelValues(res.Decision.Decision, res.DeclineReason).Inc()
}
