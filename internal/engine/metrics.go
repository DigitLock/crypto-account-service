package engine

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PromReporter is the Reporter of server: the engine metrics of SRS — Core §2.5.1.
type PromReporter struct {
	runs        *prometheus.CounterVec
	inserted    *prometheus.CounterVec
	skipped     *prometheus.CounterVec
	unmapped    *prometheus.CounterVec
	rejections  *prometheus.CounterVec
	wait        *prometheus.HistogramVec
	connections *prometheus.GaugeVec
	staleness   *prometheus.GaugeVec
	gaps        *prometheus.GaugeVec
	keyChecks   *prometheus.CounterVec
}

var _ Reporter = (*PromReporter)(nil)

// NewPromReporter registers the engine metrics in reg.
func NewPromReporter(reg prometheus.Registerer) *PromReporter {
	r := &PromReporter{
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sync_runs_total", Help: "Sync runs by outcome: success or failure; stream is the stream family.",
		}, []string{"source", "stream", "result"}),
		inserted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_entries_inserted_total", Help: "New ledger entries.",
		}, []string{"source"}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_duplicates_skipped_total", Help: "Records skipped by the idempotency key.",
		}, []string{"source"}),
		unmapped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unmapped_assets_total", Help: "Native codes without an alias, stored under the native code.",
		}, []string{"source"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rate_limit_rejections_total", Help: "Rate limit exceeded answers from the source.",
		}, []string{"source"}),
		wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "rate_limit_wait_seconds", Help: "Time spent waiting for the budget of the rate limiter.",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"source"}),
		connections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "connections", Help: "Connections by state.",
		}, []string{"status"}),
		staleness: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sync_staleness_seconds", Help: "Now minus the oldest last_success_at among the streams the engine runs.",
		}, []string{"source"}),
		gaps: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ledger_gap", Help: "Gap of the last balance checkpoint: balance at the source minus the ledger total.",
		}, []string{"source", "connection", "asset"}),
		keyChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "key_checks_total", Help: "Periodic key checks of the engine by result: success, invalid or failure.",
		}, []string{"source", "result"}),
	}
	reg.MustRegister(r.runs, r.inserted, r.skipped, r.unmapped, r.rejections, r.wait, r.connections, r.staleness, r.gaps,
		r.keyChecks)
	return r
}

// RunFinished implements Reporter.
func (r *PromReporter) RunFinished(source, family string, success bool) {
	result := "failure"
	if success {
		result = "success"
	}
	r.runs.WithLabelValues(source, family, result).Inc()
}

// EntriesInserted implements Reporter.
func (r *PromReporter) EntriesInserted(source string, n int) {
	r.inserted.WithLabelValues(source).Add(float64(n))
}

// DuplicatesSkipped implements Reporter.
func (r *PromReporter) DuplicatesSkipped(source string, n int) {
	r.skipped.WithLabelValues(source).Add(float64(n))
}

// UnmappedAsset implements Reporter.
func (r *PromReporter) UnmappedAsset(source, _ string) { r.unmapped.WithLabelValues(source).Inc() }

// BudgetWaited implements Reporter.
func (r *PromReporter) BudgetWaited(source string, d time.Duration) {
	r.wait.WithLabelValues(source).Observe(d.Seconds())
}

// RateLimited implements Reporter.
func (r *PromReporter) RateLimited(source string) { r.rejections.WithLabelValues(source).Inc() }

// Connections implements Reporter.
func (r *PromReporter) Connections(byStatus map[string]int) {
	r.connections.Reset()
	for status, n := range byStatus {
		r.connections.WithLabelValues(status).Set(float64(n))
	}
}

// LedgerGap implements Reporter. The gap is a plain decimal; a metric value is a float, so it is parsed here, at
// the edge, and nowhere on the path of amounts.
func (r *PromReporter) LedgerGap(source, connection, asset, gap string) {
	v, err := strconv.ParseFloat(gap, 64)
	if err != nil {
		return
	}
	r.gaps.WithLabelValues(source, connection, asset).Set(v)
}

// KeyCheckFinished implements Reporter.
func (r *PromReporter) KeyCheckFinished(source, result string) {
	r.keyChecks.WithLabelValues(source, result).Inc()
}

// Staleness implements Reporter.
func (r *PromReporter) Staleness(bySource map[string]time.Duration) {
	r.staleness.Reset()
	for source, d := range bySource {
		r.staleness.WithLabelValues(source).Set(d.Seconds())
	}
}
