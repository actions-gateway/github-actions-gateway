// Package listener implements the per-RunnerGroup listener goroutine pool.
package listener

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics holds all Prometheus metrics emitted by the listener and provisioner packages.
type Metrics struct {
	ActiveSessions       *prometheus.GaugeVec
	JobsAcquiredTotal    *prometheus.CounterVec
	JobAcquisitionErrors *prometheus.CounterVec
	// Q59: pre-acquisition admission control. Incremented when the capacity gate
	// rejects a delivered job (acquire skipped, job left queued for redelivery).
	JobsAdmissionRejectedTotal *prometheus.CounterVec
	// Q260: duplicate broker delivery. Incremented when an acquired job's planID is
	// already claimed by a sibling session in this AGC, so this session skips
	// provisioning (and recycles its runner) rather than colliding on the shared
	// "job-<planID>" worker Secret. Covers both a concurrent burst (a sibling is
	// provisioning the planID right now) and a LATE redelivery (the winner already
	// completed but its terminal worker pod has not yet been reaped, so the claim
	// still lingers and the redelivery would otherwise collide on `create Pod`).
	JobsDuplicateDeliveryTotal *prometheus.CounterVec
	// Q260 Option A: the winner of a fanned-out job issuing a completejob on a
	// deduped sibling delivery (or on a late redelivery within the linger window) so
	// GitHub does not leave the acquired-but-unrun assignment dangling until its
	// ~15-minute unstarted-job timeout. Labelled by outcome (completed, error). Only
	// incremented when the guarded behavior is enabled (Config.FanoutCompletion).
	AbandonedDeliveryCompletionsTotal *prometheus.CounterVec
	// Q266: a deduped fan-out loser deferred its slot recycle until its winner
	// concluded, rather than recycling eagerly into a 422 that cannot clear for the
	// winner's whole runtime (which would exhaust the bounded Q259 backoff and exit
	// the listener, collapsing the pool under sustained burst). Labelled by outcome:
	// winner_concluded (the normal path — the winner finished and released this
	// runner's assignment), fallback_timeout (the winner never concluded within the
	// bound; GitHub's unstarted-job timeout released the assignment instead — worth
	// alerting on), or context_cancelled (AGC shutdown). Only emitted when fan-out
	// completion (Q260 Option A) is enabled.
	FanoutLoserRecycleDeferredTotal *prometheus.CounterVec
	TokenRefreshesTotal             *prometheus.CounterVec
	TokenRefreshErrorsTotal         *prometheus.CounterVec
	RenewJobErrorsTotal             *prometheus.CounterVec
	// Q254: incremented when the per-job renew loop cancels the worker because the
	// job's lock is definitively lost, by reason (job_not_found, consecutive_failures).
	RenewJobTeardownsTotal *prometheus.CounterVec
	MessagePollErrorsTotal *prometheus.CounterVec
	// M3: pod lifecycle metrics (emitted by provisioner package)
	PodCreationLatency       *prometheus.HistogramVec
	JobDuration              *prometheus.HistogramVec
	EvictionRetries          *prometheus.CounterVec
	EvictionRetriesExhausted *prometheus.CounterVec
	QuotaRetries             *prometheus.CounterVec
	QuotaRetriesExhausted    *prometheus.CounterVec
	// Q95: worker pod lifecycle (emitted by the RunnerGroup reconciler's reaper)
	WorkerPodsReaped *prometheus.CounterVec
	// Q114: single-use JIT agent recycling (emitted by listener goroutines and
	// the agent pool's reconcile repair pass)
	AgentRecyclesTotal      *prometheus.CounterVec
	AgentRecycleErrorsTotal *prometheus.CounterVec
	// Q267: broker OAuth token-exchange retries a freshly recycled agent made
	// while GitHub's token endpoint still reported its just-created registration as
	// "not found" (the generate-jitconfig → OAuth-service propagation window). A
	// sustained non-zero rate signals wide-pool recycle churn feeding this seam.
	BrokerTokenPropagationRetriesTotal *prometheus.CounterVec
	// Q249: number of regular (non-native) sidecar containers in a RunnerSet's
	// resolved worker template that may block pod reaping (emitted by the RunnerSet
	// reconciler). A non-zero value warns of the Q247 stranding class; native
	// sidecars or the self-exiting-sidecars acknowledgment annotation clear it.
	ReapBlockingSidecarTemplates *prometheus.GaugeVec
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

		JobsAdmissionRejectedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_jobs_admission_rejected_total",
			Help: "Jobs left queued at GitHub because the pre-acquisition capacity gate was full (acquire skipped for redelivery).",
		}, []string{"namespace", "runner_group"}),

		JobsDuplicateDeliveryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_jobs_duplicate_delivery_total",
			Help: "Duplicate job deliveries deduplicated: the job's planID was already claimed by a sibling session in this AGC, so provisioning was skipped (and the runner recycled) to avoid colliding on the shared per-job worker Secret or the winner's not-yet-reaped worker pod. Covers both a concurrent burst and a late redelivery after completion (Q260).",
		}, []string{"namespace", "runner_group"}),

		AbandonedDeliveryCompletionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_abandoned_delivery_completions_total",
			Help: "Deduped sibling deliveries of a fanned-out job whose acquired-but-unrun assignment the winner released via completejob (on completion, or on a late redelivery within the linger window) so GitHub does not cancel the job at its ~15-minute unstarted-job timeout, by outcome (completed, error). Only emitted when fan-out completion (Q260 Option A) is enabled.",
		}, []string{"namespace", "runner_group", "outcome"}),

		FanoutLoserRecycleDeferredTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_fanout_loser_recycle_deferred_total",
			Help: "Deduped fan-out losers that deferred their slot recycle until their winner concluded, rather than recycling into a 422 that persists for the winner's whole runtime and exits the listener (which collapses the pool under sustained burst). By outcome: winner_concluded (normal), fallback_timeout (winner never concluded within the bound — GitHub's unstarted-job timeout released the assignment instead; alert-worthy), context_cancelled (AGC shutdown). Only emitted when fan-out completion (Q260 Option A) is enabled (Q266).",
		}, []string{"namespace", "runner_group", "outcome"}),

		TokenRefreshesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_token_refreshes_total",
			Help: "Total number of successful installation token refreshes.",
		}, []string{"namespace"}),

		TokenRefreshErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_token_refresh_errors_total",
			Help: "Total number of installation token refresh failures.",
		}, []string{"namespace"}),

		RenewJobErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_renew_job_errors_total",
			Help: "Total number of RenewJob non-OK responses.",
		}, []string{"namespace"}),

		RenewJobTeardownsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_renew_job_teardowns_total",
			Help: "Workers cancelled by the renew loop because the job lock was definitively lost, by reason (job_not_found, consecutive_failures).",
		}, []string{"namespace", "reason"}),

		MessagePollErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_message_poll_errors_total",
			Help: "Total number of GetMessage errors.",
		}, []string{"namespace", "reason"}),

		PodCreationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "actions_gateway_pod_creation_latency_seconds",
			Help: "Time from worker pod creation to the runner container starting (includes scheduling and image pull).",
			// Bracket the Appendix A SLO (p95 ≤ 15s, p99 ≤ 60s): sub-second on warm
			// nodes, tens of seconds when a cold node must pull the runner image.
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 15, 30, 60, 120, 300},
		}, []string{"namespace"}),

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

		AgentRecyclesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_agent_recycles_total",
			Help: "Single-use JIT agents re-registered, by trigger (post_job, stale_session, startup, reconcile_repair).",
		}, []string{"namespace", "runner_group", "trigger"}),

		AgentRecycleErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_agent_recycle_errors_total",
			Help: "Failed attempts to re-register a single-use JIT agent.",
		}, []string{"namespace", "runner_group"}),

		BrokerTokenPropagationRetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_broker_token_propagation_retries_total",
			Help: "Broker OAuth token-exchange retries a freshly recycled agent made while GitHub's token endpoint still returned a transient \"Registration … was not found\" 400 for its just-created runner record (the generate-jitconfig → OAuth-service propagation window). The listener rides these out instead of exiting and churning a new record; a sustained non-zero rate signals wide-pool recycle churn feeding this seam (Q267, the Q259/Q114 family).",
		}, []string{"namespace", "runner_group"}),

		ReapBlockingSidecarTemplates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_reap_blocking_sidecar_templates",
			Help: "Number of regular (non-native) sidecar containers in a RunnerSet's resolved worker template that may block pod reaping (Q249); native sidecars or the self-exiting-sidecars acknowledgment annotation clear it.",
		}, []string{"namespace", "runner_set"}),
	}

	metrics.Registry.MustRegister(
		m.ActiveSessions,
		m.JobsAcquiredTotal,
		m.JobAcquisitionErrors,
		m.JobsAdmissionRejectedTotal,
		m.JobsDuplicateDeliveryTotal,
		m.AbandonedDeliveryCompletionsTotal,
		m.FanoutLoserRecycleDeferredTotal,
		m.TokenRefreshesTotal,
		m.TokenRefreshErrorsTotal,
		m.RenewJobErrorsTotal,
		m.RenewJobTeardownsTotal,
		m.MessagePollErrorsTotal,
		m.PodCreationLatency,
		m.JobDuration,
		m.EvictionRetries,
		m.EvictionRetriesExhausted,
		m.QuotaRetries,
		m.QuotaRetriesExhausted,
		m.WorkerPodsReaped,
		m.AgentRecyclesTotal,
		m.AgentRecycleErrorsTotal,
		m.BrokerTokenPropagationRetriesTotal,
		m.ReapBlockingSidecarTemplates,
	)
	return m
}

// IncAgentRecycle implements agentpool.RecycleMetrics.
func (m *Metrics) IncAgentRecycle(namespace, group, trigger string) {
	m.AgentRecyclesTotal.WithLabelValues(namespace, group, trigger).Inc()
}

// IncAgentRecycleError implements agentpool.RecycleMetrics.
func (m *Metrics) IncAgentRecycleError(namespace, group string) {
	m.AgentRecycleErrorsTotal.WithLabelValues(namespace, group).Inc()
}

// IncTokenRefreshes implements token.MetricsRecorder.
func (m *Metrics) IncTokenRefreshes(ns string) {
	m.TokenRefreshesTotal.WithLabelValues(ns).Inc()
}

// IncTokenRefreshErrors implements token.MetricsRecorder.
func (m *Metrics) IncTokenRefreshErrors(ns string) {
	m.TokenRefreshErrorsTotal.WithLabelValues(ns).Inc()
}
