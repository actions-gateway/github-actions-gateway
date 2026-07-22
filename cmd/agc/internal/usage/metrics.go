// Package usage samples CPU/memory usage of the worker pods an AGC owns and
// aggregates it per RunnerSet × container, so operators can right-size worker
// requests/limits from measured demand instead of guesses (Q359 Phase 1 —
// operator recipe in docs/operations/worker-rightsizing.md).
//
// The Sampler polls the metrics.k8s.io API (metrics-server) on a fixed
// interval, keeps the running peak per worker pod × container while the pod
// runs, and folds each pod's peaks into per-RunnerSet Prometheus series when
// the pod reaches a terminal phase (one worker pod == one job, so a per-pod
// peak is a per-job peak). Histograms of per-job peaks let an operator derive
// p50/p95 over any PromQL window; gauges carry the max peak seen since AGC
// start. Cardinality is bounded: one series per RunnerSet × container name.
package usage

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the Prometheus collectors the worker-usage sampler emits.
type Metrics struct {
	// CPUPeak is the highest per-job CPU peak (cores) observed for a
	// RunnerSet × container since AGC start.
	CPUPeak *prometheus.GaugeVec
	// MemoryPeak is the highest per-job memory peak (bytes) observed for a
	// RunnerSet × container since AGC start.
	MemoryPeak *prometheus.GaugeVec
	// JobCPUPeak is a histogram of per-job CPU peaks (cores), one observation
	// per sampled worker pod × container.
	JobCPUPeak *prometheus.HistogramVec
	// JobMemoryPeak is a histogram of per-job memory peaks (bytes), one
	// observation per sampled worker pod × container.
	JobMemoryPeak *prometheus.HistogramVec
	// JobsSampled counts worker pods that finished with at least one usage
	// sample folded into the histograms above.
	JobsSampled *prometheus.CounterVec
	// JobsUnsampled counts worker pods that finished before any usage sample
	// landed (jobs shorter than roughly one sampling interval). A high ratio of
	// unsampled to sampled jobs means the peaks under-represent short jobs.
	JobsUnsampled *prometheus.CounterVec
	// PollErrors counts failed metrics.k8s.io list calls (e.g. metrics-server
	// not installed). The sampler keeps retrying; a constant rate here means
	// usage metrics are not being collected at all.
	PollErrors *prometheus.CounterVec
}

// NewMetrics creates the worker-usage collectors and registers them with reg,
// which must be non-nil. Production passes the controller-runtime registry;
// tests pass a throwaway prometheus.NewRegistry() so each call is isolated
// (MustRegister panics on duplicate registration by design — see
// scalesetlistener.NewMetrics for the rationale).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	setContainer := []string{"namespace", "runner_set", "container"}
	set := []string{"namespace", "runner_set"}
	m := &Metrics{
		CPUPeak: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_worker_usage_cpu_peak_cores",
			Help: "Highest per-job CPU peak (cores) observed for the RunnerSet's worker pods, per container, since AGC start (Q359).",
		}, setContainer),

		MemoryPeak: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_worker_usage_memory_peak_bytes",
			Help: "Highest per-job memory peak (bytes) observed for the RunnerSet's worker pods, per container, since AGC start (Q359).",
		}, setContainer),

		JobCPUPeak: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "actions_gateway_worker_usage_job_cpu_peak_cores",
			Help: "Per-job CPU peak (cores) of the RunnerSet's worker pods, per container — one observation per sampled job. Use histogram_quantile for p50/p95 over a chosen window (Q359).",
			// Shared with the in-memory aggregates (aggregate.go): per-job
			// peaks above the top edge land in +Inf, which still ranks
			// correctly for sizing decisions.
			Buckets: cpuBucketEdges,
		}, setContainer),

		JobMemoryPeak: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "actions_gateway_worker_usage_job_memory_peak_bytes",
			Help: "Per-job memory peak (bytes) of the RunnerSet's worker pods, per container — one observation per sampled job. Use histogram_quantile for p50/p95 over a chosen window (Q359).",
			// Shared with the in-memory aggregates (aggregate.go).
			Buckets: memBucketEdges,
		}, setContainer),

		JobsSampled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_worker_usage_jobs_sampled_total",
			Help: "Worker pods that finished with at least one usage sample folded into the per-job peak histograms (Q359).",
		}, set),

		JobsUnsampled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_worker_usage_jobs_unsampled_total",
			Help: "Worker pods that finished before any usage sample landed (job shorter than ~one sampling interval); their peaks are missing from the histograms (Q359).",
		}, set),

		PollErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_worker_usage_poll_errors_total",
			Help: "Failed metrics.k8s.io PodMetrics list calls; a constant rate means worker usage is not being sampled (metrics-server missing or RBAC denied) (Q359).",
		}, []string{"namespace"}),
	}
	reg.MustRegister(
		m.CPUPeak,
		m.MemoryPeak,
		m.JobCPUPeak,
		m.JobMemoryPeak,
		m.JobsSampled,
		m.JobsUnsampled,
		m.PollErrors,
	)
	return m
}
