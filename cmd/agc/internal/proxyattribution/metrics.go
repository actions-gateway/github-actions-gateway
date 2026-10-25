package proxyattribution

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the collectors the attribution reader emits.
type Metrics struct {
	// Records counts each record read, by outcome, as attributed to one of this
	// gateway's worker pods or discarded.
	Records *prometheus.CounterVec
	// Reads counts each read of one pool replica, as ok or failed. A run of
	// failures while the pool rolls is the attribution CA changing.
	Reads *prometheus.CounterVec
}

// NewMetrics creates the collectors and registers them with reg, which must be
// non-nil.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Records: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_proxy_failure_records_total",
			Help: "Egress proxy failure records read by the AGC, by outcome (denied, dial-failed, or other for a value the proxy does not write) and result (attributed to one of this gateway's worker pods, or discarded) (Q1184).",
		}, []string{"namespace", "outcome", "result"}),
		Reads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_proxy_failure_reads_total",
			Help: "Reads of one egress proxy replica's failure records, by result (ok, failed) (Q1184).",
		}, []string{"namespace", "result"}),
	}
	reg.MustRegister(m.Records, m.Reads)
	return m
}
