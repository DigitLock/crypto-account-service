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
	}
	reg.MustRegister(m.final, m.reorgs, m.checks, m.requests, m.fallback, m.ranges)
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
