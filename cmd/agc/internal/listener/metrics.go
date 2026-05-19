// Package listener implements the per-RunnerGroup listener goroutine pool.
package listener

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics holds all Prometheus metrics emitted by the listener package.
type Metrics struct {
	ActiveSessions          *prometheus.GaugeVec
	JobsAcquiredTotal       *prometheus.CounterVec
	JobAcquisitionErrors    *prometheus.CounterVec
	TokenRefreshesTotal     *prometheus.CounterVec
	TokenRefreshErrorsTotal *prometheus.CounterVec
	RenewJobErrorsTotal     *prometheus.CounterVec
	MessagePollErrorsTotal  *prometheus.CounterVec
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
	}

	metrics.Registry.MustRegister(
		m.ActiveSessions,
		m.JobsAcquiredTotal,
		m.JobAcquisitionErrors,
		m.TokenRefreshesTotal,
		m.TokenRefreshErrorsTotal,
		m.RenewJobErrorsTotal,
		m.MessagePollErrorsTotal,
	)
	return m
}
