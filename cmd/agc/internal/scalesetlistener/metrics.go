package scalesetlistener

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics holds the Prometheus counters the scale-set acquisition tier emits, mirroring
// the classic listener.Metrics for the Option E (Q264) path. The scale-set tier runs
// one Listener session per ScaleSet-protocol RunnerSet, so every counter is labelled by
// (namespace, runner_set); job-completion additionally carries the server-reported
// result. These are distinct from the classic actions_gateway_* series — a Classic
// RunnerSet never touches them — so an operator can watch either tier independently
// during the P4 dogfood validation (the Q224 acceptance gate).
//
// The Listener's MetricsRecorder interface is label-free (one Listener owns one set):
// RecorderFor binds a per-RunnerSet adapter over these vectors and is what the
// reconciler passes into scalesetlistener.Config.Metrics.
type Metrics struct {
	// JobsAssignedTotal counts each JobAssigned the scale set's queue delivers.
	JobsAssignedTotal *prometheus.CounterVec
	// JobsProvisionedTotal counts each worker pod successfully provisioned for an
	// assigned job.
	JobsProvisionedTotal *prometheus.CounterVec
	// ProvisionErrorsTotal counts failed provision attempts (JIT-config mint or pod
	// create); the job is left un-provisioned so a later poll retries it.
	ProvisionErrorsTotal *prometheus.CounterVec
	// JobsCompletedTotal counts terminal JobCompleted messages by result — the
	// completion signal the classic many-acquirers protocol never delivered (§2b-6).
	JobsCompletedTotal *prometheus.CounterVec
}

// NewMetrics creates and registers the scale-set tier's metrics with the
// controller-runtime metrics registry. Call it once per process (at startup): it
// registers fresh collectors, and prometheus.MustRegister panics if a collector with
// the same name is already registered, so a second call would panic on the duplicate.
func NewMetrics() *Metrics {
	m := &Metrics{
		JobsAssignedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_jobs_assigned_total",
			Help: "Total jobs the scale set's queue delivered as JobAssigned to the acquisition-tier listener (Q264 Option E).",
		}, []string{"namespace", "runner_set"}),

		JobsProvisionedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_jobs_provisioned_total",
			Help: "Total worker pods successfully provisioned by the scale-set acquisition tier, one per assigned job.",
		}, []string{"namespace", "runner_set"}),

		ProvisionErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_provision_errors_total",
			Help: "Total failed provision attempts (JIT-config mint or worker pod create) by the scale-set acquisition tier; the job is retried on a later poll.",
		}, []string{"namespace", "runner_set"}),

		JobsCompletedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_jobs_completed_total",
			Help: "Total terminal JobCompleted messages the scale set's queue delivered, by result (the completion signal the classic protocol never delivered).",
		}, []string{"namespace", "runner_set", "result"}),
	}
	metrics.Registry.MustRegister(
		m.JobsAssignedTotal,
		m.JobsProvisionedTotal,
		m.ProvisionErrorsTotal,
		m.JobsCompletedTotal,
	)
	return m
}

// RecorderFor returns a MetricsRecorder that records into m's vectors under the given
// RunnerSet's (namespace, runner_set) labels. A nil *Metrics returns a nil recorder,
// which scalesetlistener.Config accepts (metrics disabled) — so an operator who never
// wires NewMetrics keeps default-Classic behavior with no observability side effects.
func (m *Metrics) RecorderFor(namespace, runnerSet string) MetricsRecorder {
	if m == nil {
		return nil
	}
	return &recorder{m: m, namespace: namespace, runnerSet: runnerSet}
}

// recorder binds a Metrics to one RunnerSet's labels, implementing MetricsRecorder.
type recorder struct {
	m         *Metrics
	namespace string
	runnerSet string
}

func (r *recorder) IncJobAssigned() {
	r.m.JobsAssignedTotal.WithLabelValues(r.namespace, r.runnerSet).Inc()
}

func (r *recorder) IncJobProvisioned() {
	r.m.JobsProvisionedTotal.WithLabelValues(r.namespace, r.runnerSet).Inc()
}

func (r *recorder) IncProvisionError() {
	r.m.ProvisionErrorsTotal.WithLabelValues(r.namespace, r.runnerSet).Inc()
}

func (r *recorder) IncJobCompleted(result string) {
	r.m.JobsCompletedTotal.WithLabelValues(r.namespace, r.runnerSet, result).Inc()
}
