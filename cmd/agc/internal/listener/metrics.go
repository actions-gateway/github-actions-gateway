// Package listener implements the per-RunnerGroup listener goroutine pool.
package listener

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics holds all Prometheus metrics emitted by the listener and provisioner packages.
type Metrics struct {
	ActiveSessions          *prometheus.GaugeVec
	JobsAcquiredTotal       *prometheus.CounterVec
	JobAcquisitionErrors    *prometheus.CounterVec
	TokenRefreshesTotal     *prometheus.CounterVec
	TokenRefreshErrorsTotal *prometheus.CounterVec
	RenewJobErrorsTotal     *prometheus.CounterVec
	MessagePollErrorsTotal  *prometheus.CounterVec
	// M3: pod lifecycle metrics (emitted by provisioner package)
	JobDuration              *prometheus.HistogramVec
	EvictionRetries          *prometheus.CounterVec
	EvictionRetriesExhausted *prometheus.CounterVec
	QuotaRetries             *prometheus.CounterVec
	QuotaRetriesExhausted    *prometheus.CounterVec
	// Q95: worker pod lifecycle (emitted by the RunnerGroup reconciler's reaper)
	WorkerPodsReaped *prometheus.CounterVec
}

// NewMetrics creates and registers all listener metrics with the controller-runtime
// metrics registry. Safe to call multiple times; subsequent calls are no-ops
// because prometheus.MustRegister is idempotent for already-registered metrics.
func NewMetrics() *Metrics {
	m := &Metrics{
		ActiveSessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_active_sessions",
			Help: "Number of currently open long-poll sessions per RunnerGroup.",
		}, []string{"namespace", "runner_group"}),

		JobsAcquiredTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_jobs_acquired_total",
			Help: "Total number of jobs acquired by AcquireJob.",
		}, []string{"namespace", "runner_group"}),

		JobAcquisitionErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_job_acquisition_errors_total",
			Help: "Total number of AcquireJob failures.",
		}, []string{"namespace", "reason"}),

		TokenRefreshesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_token_refreshes_total",
			Help: "Total number of successful installation token refreshes.",
		}, []string{"namespace"}),

		TokenRefreshErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_token_refresh_errors_total",
			Help: "Total number of installation token refresh failures.",
		}, []string{"namespace"}),

		RenewJobErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_renewjob_errors_total",
			Help: "Total number of RenewJob non-OK responses.",
		}, []string{"namespace"}),

		MessagePollErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_message_poll_errors_total",
			Help: "Total number of GetMessage errors.",
		}, []string{"namespace", "reason"}),

		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "actions_gateway_job_duration_seconds",
			Help:    "Wall time from acquirejob to worker pod completion.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		}, []string{"namespace", "runner_group"}),

		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_retries_total",
			Help: "Jobs automatically re-queued after worker pod eviction.",
		}, []string{"namespace", "runner_group"}),

		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_retries_exhausted_total",
			Help: "Evicted jobs where retry budget was exhausted.",
		}, []string{"namespace", "runner_group"}),

		QuotaRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_quota_retries_total",
			Help: "Pod creation attempts retried due to namespace ResourceQuota exhaustion.",
		}, []string{"namespace", "runner_group"}),

		QuotaRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_quota_retries_exhausted_total",
			Help: "Jobs abandoned after exhausting the quota retry budget.",
		}, []string{"namespace", "runner_group"}),

		WorkerPodsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_worker_pods_reaped_total",
			Help: "Worker pods deleted by the reaper, by reason (completed_ttl, pending_deadline).",
		}, []string{"namespace", "runner_group", "reason"}),
	}

	metrics.Registry.MustRegister(
		m.ActiveSessions,
		m.JobsAcquiredTotal,
		m.JobAcquisitionErrors,
		m.TokenRefreshesTotal,
		m.TokenRefreshErrorsTotal,
		m.RenewJobErrorsTotal,
		m.MessagePollErrorsTotal,
		m.JobDuration,
		m.EvictionRetries,
		m.EvictionRetriesExhausted,
		m.QuotaRetries,
		m.QuotaRetriesExhausted,
		m.WorkerPodsReaped,
	)
	return m
}

// IncTokenRefreshes implements token.MetricsRecorder.
func (m *Metrics) IncTokenRefreshes(ns string) {
	m.TokenRefreshesTotal.WithLabelValues(ns).Inc()
}

// IncTokenRefreshErrors implements token.MetricsRecorder.
func (m *Metrics) IncTokenRefreshErrors(ns string) {
	m.TokenRefreshErrorsTotal.WithLabelValues(ns).Inc()
}
