package evm

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the sink of the metrics of the connector (SRS — EVM Connector §2.5.1). server registers PromMetrics.
type Metrics interface {
	// FinalBlock sets evm_final_block{source}.
	FinalBlock(source string, number uint64)
	// ReorgBelowFinal adds 1 to evm_reorg_below_final_total{source}.
	ReorgBelowFinal(source string)
	// StartCheck sets evm_start_check_failed{source,check}: 1 while the check fails, 0 once it passed.
	StartCheck(source, check string, failed bool)
	// RPCRequest adds 1 to evm_rpc_requests_total{source,endpoint,method,result}.
	RPCRequest(source, endpoint, method, result string)
	// FallbackActive sets evm_rpc_fallback_active{source}: 1 when the last run used the fallback endpoint.
	FallbackActive(source string, active bool)
	// LogRangeBlocks sets evm_log_range_blocks{source}: the range size in use after splitting (EC-306).
	LogRangeBlocks(source string, blocks uint64)
	// IndexerLag sets evm_indexer_lag_blocks{source}: the final block minus the oldest last processed block of the
	// logs streams of the source in mode INCREMENTAL.
	IndexerLag(source string, blocks uint64)
	// UnmatchedControllerEvent adds 1 to evm_unmatched_controller_events_total{source} (EC-311).
	UnmatchedControllerEvent(source string)
	// SkippedLog adds 1 to evm_skipped_logs_total{source,reason} (EC-315).
	SkippedLog(source, reason string)
	// CompletenessSkipped adds 1 to evm_completeness_skipped_total{source}: EC-317, or EC-319 (S3 D-39).
	CompletenessSkipped(source string)
}

// Values of the label result of evm_rpc_requests_total.
const (
	ResultSuccess   = "success"
	ResultFailure   = "failure"
	ResultRateLimit = "rate_limit"
)

// PromMetrics is the Metrics of server.
type PromMetrics struct {
	final    *prometheus.GaugeVec
	reorgs   *prometheus.CounterVec
	checks   *prometheus.GaugeVec
	requests *prometheus.CounterVec
	fallback *prometheus.GaugeVec
	ranges   *prometheus.GaugeVec
	lag      *prometheus.GaugeVec
	unmatch  *prometheus.CounterVec
	skipped  *prometheus.CounterVec
	complete *prometheus.CounterVec
}

var _ Metrics = (*PromMetrics)(nil)

// NewPromMetrics registers the metrics of the connector in reg.
func NewPromMetrics(reg prometheus.Registerer) *PromMetrics {
	m := &PromMetrics{
		final: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "evm_final_block", Help: "Final block by the finality rule of the network.",
		}, []string{"source"}),
		reorgs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "evm_reorg_below_final_total", Help: "Reorg guard hits: the last processed block is missing or its hash changed.",
		}, []string{"source"}),
		checks: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "evm_start_check_failed", Help: "1 while a start check of the network fails.",
		}, []string{"source", "check"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "evm_rpc_requests_total", Help: "RPC calls by endpoint, method and outcome: success, failure or rate_limit.",
		}, []string{"source", "endpoint", "method", "result"}),
		fallback: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "evm_rpc_fallback_active", Help: "1 when the last run of the network used the fallback endpoint.",
		}, []string{"source"}),
		ranges: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "evm_log_range_blocks", Help: "Log range size in use after splitting.",
		}, []string{"source"}),
		lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "evm_indexer_lag_blocks", Help: "Final block minus the oldest last processed block of the incremental logs streams.",
		}, []string{"source"}),
		unmatch: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "evm_unmatched_controller_events_total", Help: "Controller events without their Transfer.",
		}, []string{"source"}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "evm_skipped_logs_total", Help: "Logs not imported, by reason.",
		}, []string{"source", "reason"}),
		complete: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "evm_completeness_skipped_total",
			Help: "Completeness checks skipped: state not served (EC-317) or a balance out of range (EC-319).",
		}, []string{"source"}),
	}
	reg.MustRegister(m.final, m.reorgs, m.checks, m.requests, m.fallback, m.ranges, m.lag, m.unmatch, m.skipped, m.complete)
	return m
}

// FinalBlock implements Metrics.
func (m *PromMetrics) FinalBlock(source string, number uint64) {
	m.final.WithLabelValues(source).Set(float64(number))
}

// ReorgBelowFinal implements Metrics.
func (m *PromMetrics) ReorgBelowFinal(source string) { m.reorgs.WithLabelValues(source).Inc() }

// StartCheck implements Metrics.
func (m *PromMetrics) StartCheck(source, check string, failed bool) {
	m.checks.WithLabelValues(source, check).Set(boolValue(failed))
}

// RPCRequest implements Metrics.
func (m *PromMetrics) RPCRequest(source, endpoint, method, result string) {
	m.requests.WithLabelValues(source, endpoint, method, result).Inc()
}

// FallbackActive implements Metrics.
func (m *PromMetrics) FallbackActive(source string, active bool) {
	m.fallback.WithLabelValues(source).Set(boolValue(active))
}

// LogRangeBlocks implements Metrics.
func (m *PromMetrics) LogRangeBlocks(source string, blocks uint64) {
	m.ranges.WithLabelValues(source).Set(float64(blocks))
}

// IndexerLag implements Metrics.
func (m *PromMetrics) IndexerLag(source string, blocks uint64) {
	m.lag.WithLabelValues(source).Set(float64(blocks))
}

// UnmatchedControllerEvent implements Metrics.
func (m *PromMetrics) UnmatchedControllerEvent(source string) {
	m.unmatch.WithLabelValues(source).Inc()
}

// SkippedLog implements Metrics.
func (m *PromMetrics) SkippedLog(source, reason string) {
	m.skipped.WithLabelValues(source, reason).Inc()
}

// CompletenessSkipped implements Metrics.
func (m *PromMetrics) CompletenessSkipped(source string) { m.complete.WithLabelValues(source).Inc() }

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// nopMetrics is the sink when none is given.
type nopMetrics struct{}

func (nopMetrics) FinalBlock(string, uint64)                 {}
func (nopMetrics) ReorgBelowFinal(string)                    {}
func (nopMetrics) StartCheck(string, string, bool)           {}
func (nopMetrics) RPCRequest(string, string, string, string) {}
func (nopMetrics) FallbackActive(string, bool)               {}
func (nopMetrics) LogRangeBlocks(string, uint64)             {}
func (nopMetrics) IndexerLag(string, uint64)                 {}
func (nopMetrics) UnmatchedControllerEvent(string)           {}
func (nopMetrics) SkippedLog(string, string)                 {}
func (nopMetrics) CompletenessSkipped(string)                {}
