// Package runnercore holds the contracts the AGC's runner tier shares across CRD
// versions and job-acquisition protocols: the Prometheus metric set every
// component emits into, the worker-capacity admission gate, and the non-blocking
// condition and event sinks the reconcilers drain.
//
// Nothing here is specific to the classic long-poll protocol (that lives in
// internal/listener) or to one API version, so removing either — the v1 sunset
// (Q273) or the classic-machinery removal (Q264) — leaves this package standing.
package runnercore

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
	// The AGC issuing a completejob on an assignment it acquired but never ran, so
	// GitHub does not leave it dangling until its ~15-minute unstarted-job timeout: a
	// deduped sibling delivery or a late redelivery within the linger window (Q260
	// Option A), or a session's own delivery whose worker pod was removed before it
	// ran (Q628). Labelled by outcome (completed, error). Only incremented when the
	// guarded behavior is enabled (Config.FanoutCompletion).
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
	PodCreationLatency *prometheus.HistogramVec
	JobDuration        *prometheus.HistogramVec
	// EvictionRetries / EvictionRetriesExhausted carry a `tier` label (classic,
	// scaleset) because the two acquisition tiers detect a disruption by entirely
	// different machinery — an inline pod wait on classic, the owning reconciler's
	// recovery pass on scale-set (Q417). Without the split, "eviction recovery is
	// working" cannot be asserted for the tier a v2beta1 tenant actually runs on.
	//
	// They also carry a `cause` label (eviction, preemption, deletion, abandoned)
	// because the operator response differs entirely (Q497): a climbing `eviction` rate
	// means node pressure — memory or disk exhaustion on the nodes — while a climbing
	// `preemption` rate means the priorityTiers floor is displacing more opportunistic
	// work than the tenant sized for, and a climbing `abandoned` rate means workers are
	// not being placed at all before pendingPodDeadline reaps them (Q691). No diagnosis
	// applies to another. The retry budget is NOT split by cause: it stays one hard cap
	// per run_id across every cause together.
	EvictionRetries          *prometheus.CounterVec
	EvictionRetriesExhausted *prometheus.CounterVec
	// EvictionRerunFailures counts disruption recoveries whose re-run was never
	// accepted by GitHub — the budget slot was spent and EvictionRetries
	// incremented, but the job was not re-run, so without this the metrics read
	// as a recovery that happened (Q503). Split by reason: run_never_concluded
	// (GitHub still answered "This workflow is already running" when the re-run
	// window closed) versus api_error (a terminal API failure).
	EvictionRerunFailures *prometheus.CounterVec
	// AbandonedRunForceCancels counts the provisioner's REST force-cancels of a run
	// whose worker pod was removed before it ran (Q683). Nothing will ever report
	// such a job — no completejob value ends it honestly (Q645/Q676) — and told
	// nothing GitHub cancels run and job at its ~15-minute unstarted-job timeout;
	// a standalone force-cancel reaches the same cancelled conclusion in about a
	// second (measured 2026-08-05). By outcome: cancelled (accepted), identity_unknown
	// (the payload carried no owner/repo/run_id, or the worker pod carried no run-id
	// annotation), error (refused or API failure) — on the latter two the unstarted-job
	// timeout remains the honest backstop. The tier label splits the two detections
	// (Q766): classic reads the identity from the payload it holds, scaleset from the
	// worker pod's annotations.
	AbandonedRunForceCancels *prometheus.CounterVec
	// AbandonedRunRerunWaits counts how the wait for capacity ended for a run that
	// was force-cancelled after its worker was removed before it ran (Q691). The
	// re-run is deliberately deferred: the job was abandoned because its worker could
	// not be placed, so re-queueing it at once puts it back into the starved pool. By
	// outcome: capacity_returned (a worker pod bound for the owner, and the re-run was
	// handed to the shared per-run retry budget, where it shows up as
	// EvictionRetries/EvictionRetriesExhausted with cause=abandoned) or expired
	// (nothing was placed inside the wait window, so the run stays cancelled and needs
	// a manual re-run).
	AbandonedRunRerunWaits *prometheus.CounterVec
	// EvictionRecoveryIdentityUnknown counts scale-set workers found disrupted that
	// carried no workflow-run identity, so no automatic re-run could be attempted.
	// It is the one failure mode that makes scale-set disruption recovery silently
	// inert, so it is counted separately from an exhausted budget rather than folded
	// into it: a sustained non-zero rate means GitHub is not sending the assignment
	// fields the mechanism depends on, not that a tenant is being disrupted too often.
	EvictionRecoveryIdentityUnknown *prometheus.CounterVec
	QuotaRetries                    *prometheus.CounterVec
	QuotaRetriesExhausted           *prometheus.CounterVec
	// Q223: worker-pod creation-rate limit (anti-stampede). Incremented when the
	// opt-in per-RunnerGroup scale-up token bucket makes a pod creation wait for a
	// token (the burst was spent). Only non-zero when a group sets spec.scaleUp; a
	// sustained rate shows the ramp is actively smoothing a burst.
	ScaleUpThrottled *prometheus.CounterVec
	// Q95: worker pod lifecycle (emitted by the RunnerGroup reconciler's reaper, and
	// by the provisioner for the job-abandoned reclaim it owns — Q501)
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
	// Q436: broker sessions the AGC gave up deleting — every DELETE attempt
	// failed, so the session survives until GitHub expires it server-side. Each
	// one holds a slice of the tenant's server-side session budget and delays
	// redelivery of any job delivered to it, and until now it was visible only as
	// a log line.
	BrokerSessionLeaksTotal *prometheus.CounterVec
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
			Help: "Jobs left queued at GitHub because the pre-acquisition capacity gate refused (acquire skipped for redelivery). reason=ceiling: the owner is at its configured worker ceiling. reason=quota: the namespace ResourceQuota has no headroom for another worker pod.",
		}, []string{"namespace", "runner_group", "reason"}),

		JobsDuplicateDeliveryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_jobs_duplicate_delivery_total",
			Help: "Duplicate job deliveries deduplicated: the job's planID was already claimed by a sibling session in this AGC, so provisioning was skipped (and the runner recycled) to avoid colliding on the shared per-job worker Secret or the winner's not-yet-reaped worker pod. Covers both a concurrent burst and a late redelivery after completion (Q260).",
		}, []string{"namespace", "runner_group"}),

		AbandonedDeliveryCompletionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_abandoned_delivery_completions_total",
			Help: "Job assignments the AGC acquired but never ran and released via completejob, so GitHub does not cancel the job at its ~15-minute unstarted-job timeout: a deduped sibling delivery of a fanned-out job (on the winner's completion, or on a late redelivery within the linger window), or a session's own delivery whose worker pod was removed before it started. By outcome (completed, error). Only emitted when fan-out completion (Q260 Option A) is enabled.",
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
			Help:    "Worker pod wall time: creation to the last container finishing, or to the deletion request for a worker removed mid-run. Emitted on both acquisition tiers, and the span the cost model bills against. A pod that never started a container is not observed.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		}, []string{"namespace", "runner_group"}),

		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_retries_total",
			Help: "Jobs automatically re-queued after a worker pod disruption, by acquisition tier (classic, scaleset) and cause (eviction, preemption, deletion, abandoned).",
		}, []string{"namespace", "runner_group", "tier", "cause"}),

		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_retries_exhausted_total",
			Help: "Disrupted jobs where retry budget was exhausted, by acquisition tier (classic, scaleset) and cause (eviction, preemption, deletion, abandoned).",
		}, []string{"namespace", "runner_group", "tier", "cause"}),

		EvictionRerunFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_rerun_failures_total",
			Help: "Disruption recoveries whose re-run was never accepted by GitHub, so the job requires a manual re-run despite the spent retry slot, by acquisition tier (classic, scaleset), cause (eviction, preemption, deletion, abandoned), and reason (run_never_concluded, api_error).",
		}, []string{"namespace", "runner_group", "tier", "cause", "reason"}),

		AbandonedRunForceCancels: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_abandoned_run_force_cancels_total",
			Help: "REST force-cancels of the workflow run behind a worker pod removed before it ran, so the run and its job conclude cancelled in about a second instead of at GitHub's ~15-minute unstarted-job timeout (Q683). By acquisition tier (classic, scaleset) and outcome (cancelled, identity_unknown, error); on identity_unknown or error the unstarted-job timeout remains the honest backstop.",
		}, []string{"namespace", "runner_group", "tier", "outcome"}),

		AbandonedRunRerunWaits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_abandoned_run_rerun_waits_total",
			Help: "Force-cancelled abandoned runs waiting for the owner to place a worker pod again before their automatic re-run fires (Q691), by acquisition tier (classic, scaleset) and how the wait ended: capacity_returned (the re-run was handed to the shared per-run retry budget, and reports there as cause=abandoned) or expired (nothing was placed inside the wait window, so a manual re-run is required).",
		}, []string{"namespace", "runner_group", "tier", "outcome"}),

		EvictionRecoveryIdentityUnknown: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_recovery_identity_unknown_total",
			Help: "Disrupted scale-set worker pods carrying no workflow-run identity, so no automatic re-run could be attempted, by cause (eviction, preemption, deletion).",
		}, []string{"namespace", "runner_group", "cause"}),

		QuotaRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_quota_retries_total",
			Help: "Pod creation attempts retried due to namespace ResourceQuota exhaustion.",
		}, []string{"namespace", "runner_group"}),

		QuotaRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_quota_retries_exhausted_total",
			Help: "Jobs abandoned after exhausting the quota retry budget.",
		}, []string{"namespace", "runner_group"}),

		ScaleUpThrottled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_worker_scaleup_throttled_total",
			Help: "Worker-pod creations delayed by the opt-in per-RunnerGroup scale-up rate limit (spec.scaleUp): the token bucket was empty so the acquired job waited for a token before its pod was created (Q223). Only non-zero when scaleUp is set; a sustained rate means the ramp is actively smoothing a cold-start burst on a shared egress path.",
		}, []string{"namespace", "runner_group"}),

		WorkerPodsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_worker_pods_reaped_total",
			Help: "Worker pods the AGC deleted, by reason (completed_ttl, pending_deadline, orphaned_running, lifetime_exceeded, job_abandoned). All but job_abandoned come from the reconciler's reaper; job_abandoned is the provisioner reclaiming the worker of a job the listener gave up on (Q501). runner_group carries the owning CR's name on both acquisition tiers; runner_set additionally carries it on scale-set reaps (empty on classic) so the series join the runner_set-labelled scaleset_* gauges (Q514).",
		}, []string{"namespace", "runner_group", "runner_set", "reason"}),

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

		BrokerSessionLeaksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_broker_session_leaks_total",
			Help: "Broker sessions abandoned because every DELETE attempt failed, leaving the session registered until GitHub expires it server-side (Q436). The listener recovers — it recycles into a fresh session either way — so this is not an availability alarm on its own; a sustained rate means the tenant is accumulating server-side sessions it no longer polls, and points at a slow or unreachable broker on the control-plane path.",
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
		m.EvictionRerunFailures,
		m.AbandonedRunForceCancels,
		m.AbandonedRunRerunWaits,
		m.EvictionRecoveryIdentityUnknown,
		m.QuotaRetries,
		m.QuotaRetriesExhausted,
		m.ScaleUpThrottled,
		m.WorkerPodsReaped,
		m.AgentRecyclesTotal,
		m.AgentRecycleErrorsTotal,
		m.BrokerTokenPropagationRetriesTotal,
		m.BrokerSessionLeaksTotal,
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

// PollErrors returns a recorder that increments MessagePollErrorsTotal under the
// given namespace, for a caller that owns one session and therefore knows only its
// reason. It is the cross-tier seam for the counter (Q446): the classic listener
// writes the vector directly because its config already carries the namespace, while
// the scale-set Listener takes this recorder through its Config.PollErrors, so both
// tiers land on the same (namespace, reason) series and an operator's existing
// dashboards and alerts keep working after the classic machinery is removed (Q264).
//
// Nil-receiver-safe, and the returned recorder is safe to call on a nil *Metrics, so
// a caller that never wired NewMetrics needs no guard.
func (m *Metrics) PollErrors(namespace string) *PollErrorRecorder {
	return &PollErrorRecorder{metrics: m, namespace: namespace}
}

// PollErrorRecorder binds a Metrics to one namespace's poll-error series. Build it
// with Metrics.PollErrors.
type PollErrorRecorder struct {
	metrics   *Metrics
	namespace string
}

// IncPollError counts one GetMessage failure under the bound namespace and the given
// reason. The reason vocabulary is shared across acquisition tiers: "rate_limited"
// (429), "timeout" (a long poll the server accepted but never answered), "other"
// (any remaining transport or decode error). Session expiry and credential rejection
// are deliberately absent — both are heal paths, not poll failures.
func (r *PollErrorRecorder) IncPollError(reason string) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.MessagePollErrorsTotal.WithLabelValues(r.namespace, reason).Inc()
}

// IncTokenRefreshes implements token.MetricsRecorder.
func (m *Metrics) IncTokenRefreshes(ns string) {
	m.TokenRefreshesTotal.WithLabelValues(ns).Inc()
}

// IncTokenRefreshErrors implements token.MetricsRecorder.
func (m *Metrics) IncTokenRefreshErrors(ns string) {
	m.TokenRefreshErrorsTotal.WithLabelValues(ns).Inc()
}
