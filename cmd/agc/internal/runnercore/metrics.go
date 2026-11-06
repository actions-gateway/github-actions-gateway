// Package runnercore holds the contracts the AGC's runner tier shares across CRD
// versions and job-acquisition protocols: the Prometheus metric set every
// component emits into, the worker-capacity admission gate, and the non-blocking
// condition and event sinks the reconcilers drain.
//
// Nothing here is specific to one API version, so the v1 sunset (Q273) left this
// package standing.
package runnercore

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics holds all Prometheus metrics emitted by the listener and provisioner packages.
type Metrics struct {
	TokenRefreshesTotal     *prometheus.CounterVec
	TokenRefreshErrorsTotal *prometheus.CounterVec
	MessagePollErrorsTotal  *prometheus.CounterVec
	// M3: pod lifecycle metrics (emitted by provisioner package)
	PodCreationLatency *prometheus.HistogramVec
	JobDuration        *prometheus.HistogramVec
	// EvictionRetries / EvictionRetriesExhausted carry a `cause` label (eviction, preemption, node_shutdown, deletion,
	// abandoned) because the operator response differs entirely (Q497): a climbing
	// `eviction` rate means node pressure — memory or disk exhaustion on the nodes — while
	// a climbing `preemption` rate means the priorityTiers floor is displacing more
	// opportunistic work than the tenant sized for, a climbing `node_shutdown` rate means
	// Spot or preemptible nodes are reclaimed under running jobs (Q1179), and a climbing
	// `abandoned` rate means workers are not being placed at all before
	// pendingPodDeadline reaps them (Q691). A climbing `oom` rate (scale-set only, Q1183)
	// means jobs are outgrowing the memory a sizing profile gave them. No diagnosis
	// applies to another. The retry budget is NOT split by cause: it stays one hard cap
	// per run_id across every cause together.
	EvictionRetries          *prometheus.CounterVec
	EvictionRetriesExhausted *prometheus.CounterVec
	// EvictionRerunFailures counts disruption recoveries whose re-run was never
	// accepted by GitHub — the budget slot was spent and EvictionRetries
	// incremented, but the job was not re-run, so without this the metrics read
	// as a recovery that happened (Q503). Split by reason: run_never_concluded
	// (GitHub still answered "This workflow is already running" when the re-run
	// window closed) versus api_error (a terminal API failure), and on the scale-set
	// tier budget_unrecorded (the failure ledger could not record the budget decision,
	// so the re-run was withheld, Q1182).
	EvictionRerunFailures *prometheus.CounterVec
	// EvictionRerunWithheld counts disruption recoveries that deliberately did not ask
	// GitHub to re-run. Split by reason:
	//
	//   - run_cancelled (Q811; every cause on the scale-set tier, Q1181): the run
	//     concluded `cancelled` by someone other than GAG, which rerun-failed-jobs
	//     accepts, so a re-run would re-queue a job a human or a concurrency group
	//     stopped — the cancel runbook's own remedy for a worker that will not stop is to
	//     delete its pod, which supplies the deletion mark that arm keys on. It usually
	//     accompanies a spent budget slot: the reservation is taken at detection, minutes
	//     before GitHub concludes the run, so the conclusion cannot be read first.
	//   - attempt_limit, run_too_old, superseded (scale-set only, Q1181): a re-run GitHub
	//     would refuse or a newer commit has made pointless. Decided on the run read
	//     before the reservation, so no slot is spent.
	//   - own_limit (cause eviction, Q1177): the kubelet evicted the worker for exceeding
	//     its own ephemeral-storage or emptyDir limit, which a re-run with the same limit
	//     repeats. Decided at detection, so no slot is spent. With cause oom (scale-set
	//     only, Q1183), the runner was killed at a memory limit no sizing profile can
	//     raise.
	//   - same_class_again, breaker_open (cause oom, scale-set only, Q1183): the job was
	//     killed again after a retry raised its limit, or too many runs were re-run for
	//     memory at once. No slot is spent.
	//
	// It is counted so a re-run that did not happen is visible rather than
	// indistinguishable from a recovery that never armed.
	EvictionRerunWithheld *prometheus.CounterVec
	// AutoRetryPaused reads 1 while a retry class's stampede breaker is open on a set,
	// and 0 once it closes (Q1245): the pause as a state, where breaker_open withholds
	// are a rate. Scale-set only, per process.
	AutoRetryPaused *prometheus.GaugeVec
	// EvictionRerunOutcomes counts the later attempt of each job GAG re-ran, by the
	// cause it re-ran for, the run's triggering event, and result (succeeded, failed):
	// failed over the total is a cause's failure rate (Q555). It keeps this family's
	// prefix and runner_group label so it joins EvictionRetries.
	EvictionRerunOutcomes *prometheus.CounterVec
	// ScaleSetJobReruns counts a later attempt of a failed scale-set job completing, by
	// who re-ran it: gag (the job was in GAG's re-run), gag_collateral (GAG's
	// rerun-failed-jobs re-ran it beside a disrupted job), other (anyone else). other
	// over scaleset_jobs_completed_total is the manual re-run rate (Q555).
	ScaleSetJobReruns *prometheus.CounterVec
	// ScaleSetFlakyJobs counts a failed scale-set job that later succeeded with no
	// disruption attributed to it, by the same trigger (Q555).
	ScaleSetFlakyJobs *prometheus.CounterVec
	// AbandonedRunForceCancels counts the provisioner's REST force-cancels of a run
	// whose worker pod was removed before it ran (Q683). Nothing will ever report
	// such a job — no completejob value ends it honestly (Q645/Q676) — and told
	// nothing GitHub cancels run and job at its ~15-minute unstarted-job timeout;
	// a standalone force-cancel reaches the same cancelled conclusion in about a
	// second (measured 2026-08-05). By outcome: cancelled (accepted), identity_unknown
	// (the payload carried no owner/repo/run_id, or the worker pod carried no run-id
	// annotation), error (refused or API failure) — on the latter two the unstarted-job
	// timeout remains the honest backstop.
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
	// EvictionRecoveryEvidenceLost counts disrupted scale-set workers whose pod was
	// deleted before the recovery could be claimed, so no automatic re-run was
	// attempted and no later reconcile can attempt one — the pod is the only record
	// the disruption has. The preemption and deletion arms are the exposed ones: both
	// act on a pod that is already terminating (Q809). A sustained rate means
	// disrupted jobs are being dropped rather than re-run, which is otherwise
	// indistinguishable from no disruptions happening at all.
	EvictionRecoveryEvidenceLost *prometheus.CounterVec
	QuotaRetries                 *prometheus.CounterVec
	QuotaRetriesExhausted        *prometheus.CounterVec
	// Q223: worker-pod creation-rate limit (anti-stampede). Incremented when the
	// opt-in per-RunnerGroup scale-up token bucket makes a pod creation wait for a
	// token (the burst was spent). Only non-zero when a group sets spec.scaleUp; a
	// sustained rate shows the ramp is actively smoothing a burst.
	ScaleUpThrottled *prometheus.CounterVec
	// Q95: worker pod lifecycle (emitted by the RunnerSet reconciler's reaper)
	WorkerPodsReaped *prometheus.CounterVec
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

		TokenRefreshesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_token_refreshes_total",
			Help: "Total number of successful installation token refreshes.",
		}, []string{"namespace"}),

		TokenRefreshErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_token_refresh_errors_total",
			Help: "Total number of installation token refresh failures.",
		}, []string{"namespace"}),

		MessagePollErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_message_poll_errors_total",
			Help: "Total number of GetMessage errors, by reason (rate_limited, timeout, other). Credential rejection and session expiry are heal paths, not poll failures, and are not counted.",
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
			Help:    "Worker pod wall time: creation to the last container finishing, or to the deletion request for a worker removed mid-run. It is the span the cost model bills against. A pod that never started a container is not observed.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		}, []string{"namespace", "runner_group"}),

		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_retries_total",
			Help: "Jobs automatically re-queued after a worker pod disruption, by cause (eviction, preemption, node_shutdown, deletion, abandoned, vanished, oom). cause=vanished names a worker already gone when the AGC started. cause=oom is a runner killed at its memory limit, re-run with a higher limit under a right-sizing profile.",
		}, []string{"namespace", "runner_group", "cause"}),

		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_retries_exhausted_total",
			Help: "Disrupted jobs where retry budget was exhausted, by cause (eviction, preemption, node_shutdown, deletion, abandoned, vanished, oom).",
		}, []string{"namespace", "runner_group", "cause"}),

		EvictionRerunFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_rerun_failures_total",
			Help: "Disruption recoveries whose re-run was never accepted by GitHub, so the job requires a manual re-run despite the spent retry slot, by cause (eviction, preemption, node_shutdown, deletion, abandoned, vanished, oom) and reason (run_never_concluded, api_error, conclusion_unknown, budget_unrecorded). reason=conclusion_unknown is a run read that never reached a verdict inside the window, on the deletion arm's cancel check or the read before the budget is reserved; reason=budget_unrecorded is a budget decision the failure ledger could not record, so the re-run was withheld, or for cause=oom a raised memory limit it could not record.",
		}, []string{"namespace", "runner_group", "cause", "reason"}),

		EvictionRerunWithheld: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_rerun_withheld_total",
			Help: "Disruption recoveries that deliberately did not ask GitHub to re-run, by cause (eviction, preemption, node_shutdown, deletion, abandoned, vanished, oom) and reason (run_cancelled: the run concluded cancelled by someone other than GAG, so a re-run would undo that cancel — Q1181, no operator action needed; own_limit: cause eviction, the kubelet evicted the worker for exceeding its own ephemeral-storage or emptyDir limit, which a re-run repeats — Q1177, with no slot spent, and the fix is a higher limit, or with cause oom, Q1183, a runner killed at a memory limit no sizing profile can raise; same_class_again, breaker_open: cause oom, Q1183, the job was killed again after a retry raised its limit, or more than 10 runs were re-run for it within 10 minutes; attempt_limit, run_too_old, superseded: Q1181, decided before the budget is reserved so no slot is spent — the run is past GitHub's 50 re-runs, older than its 30-day re-run window, or a newer commit's run of the same workflow, event and branch has started).",
		}, []string{"namespace", "runner_group", "cause", "reason"}),

		AutoRetryPaused: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_auto_retry_paused",
			Help: "1 while a retry class's stampede breaker is open on a set, 0 once it closes, by cause (oom). The breaker opens when more than 10 runs were re-run for the cause within 10 minutes, pauses that cause's retries for 30, and is held per AGC process (Q1183, Q1245). Scale-set only.",
		}, []string{"namespace", "runner_group", "cause"}),

		AbandonedRunForceCancels: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_abandoned_run_force_cancels_total",
			Help: "REST force-cancels of the workflow run behind a worker pod removed before it ran, so the run and its job conclude cancelled in about a second instead of at GitHub's ~15-minute unstarted-job timeout (Q683). By outcome (cancelled, identity_unknown, error); on identity_unknown or error the unstarted-job timeout remains the honest backstop.",
		}, []string{"namespace", "runner_group", "outcome"}),

		AbandonedRunRerunWaits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_abandoned_run_rerun_waits_total",
			Help: "Force-cancelled abandoned runs waiting for the owner to place a worker pod again before their automatic re-run fires (Q691), by how the wait ended: capacity_returned (the re-run was handed to the shared per-run retry budget, and reports there as cause=abandoned) or expired (nothing was placed inside the wait window, so a manual re-run is required).",
		}, []string{"namespace", "runner_group", "outcome"}),

		EvictionRecoveryIdentityUnknown: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_recovery_identity_unknown_total",
			Help: "Disrupted scale-set worker pods carrying no workflow-run identity, so no automatic re-run could be attempted, by cause (eviction, preemption, node_shutdown, deletion, abandoned, oom). cause=abandoned is the worker removed before it ran, whose force-cancel had no run to address.",
		}, []string{"namespace", "runner_group", "cause"}),

		EvictionRecoveryEvidenceLost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_recovery_evidence_lost_total",
			Help: "Disrupted scale-set worker pods deleted before their recovery could be claimed, so no automatic re-run was attempted and none can be attempted later — the pod is the disruption's only record (Q809). By cause (eviction, preemption, node_shutdown, deletion, oom); preemption, node_shutdown and deletion are the exposed arms, since each can act on a pod that is already terminating.",
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
			Help: "Intake withheld by the opt-in per-owner scale-up rate limit (spec.scaleUp): the token bucket was empty (Q223, Q717). The rung is expressed as reduced advertised capacity, so this counts only a pod creation that still had to wait, which means the advertisement was stale. Only non-zero when scaleUp is set.",
		}, []string{"namespace", "runner_group"}),

		WorkerPodsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_worker_pods_reaped_total",
			Help: "Worker pods the AGC deleted, by reason (completed_ttl, pending_deadline, completed_pending, orphaned_running, lifetime_exceeded, gateway_deleted), all from the reconciler's reaper. runner_group and runner_set both carry the owning RunnerSet's name, so the series join the runner_set-labelled scaleset_* gauges (Q514).",
		}, []string{"namespace", "runner_group", "runner_set", "reason"}),

		ReapBlockingSidecarTemplates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "actions_gateway_reap_blocking_sidecar_templates",
			Help: "Number of regular (non-native) sidecar containers in a RunnerSet's resolved worker template that may block pod reaping (Q249); native sidecars or the self-exiting-sidecars acknowledgment annotation clear it.",
		}, []string{"namespace", "runner_set"}),
	}

	metrics.Registry.MustRegister(
		m.TokenRefreshesTotal,
		m.TokenRefreshErrorsTotal,
		m.MessagePollErrorsTotal,
		m.PodCreationLatency,
		m.JobDuration,
		m.EvictionRetries,
		m.EvictionRetriesExhausted,
		m.EvictionRerunFailures,
		m.EvictionRerunWithheld,
		m.AutoRetryPaused,
		m.AbandonedRunForceCancels,
		m.AbandonedRunRerunWaits,
		m.EvictionRecoveryIdentityUnknown,
		m.EvictionRecoveryEvidenceLost,
		m.QuotaRetries,
		m.QuotaRetriesExhausted,
		m.ScaleUpThrottled,
		m.WorkerPodsReaped,
		m.ReapBlockingSidecarTemplates,
	)
	m.addFlakyJobMetrics()
	return m
}

// addFlakyJobMetrics builds and registers flaky-job detection's series (Q555).
func (m *Metrics) addFlakyJobMetrics() {
	// A literal keys each series by its field, which metric-reference-check resolves
	// emission sites through.
	f := Metrics{
		EvictionRerunOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_eviction_rerun_outcomes_total",
			Help: "The later attempt of each scale-set job GAG re-ran for a disruption, by the cause it re-ran for (eviction, preemption, node_shutdown, deletion, abandoned, vanished), the run's triggering event (push, pull_request, ..., other, unknown), and result (succeeded, failed). result=failed over the total is a cause's failure rate (Q555).",
		}, []string{"namespace", "runner_group", "cause", "event", "result"}),

		ScaleSetJobReruns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_job_reruns_total",
			Help: "A later attempt of a failed scale-set job completed, by trigger: gag (the job was in GAG's own disruption re-run), gag_collateral (GAG's rerun-failed-jobs re-ran it beside a disrupted job), other (a human or another automation re-ran it). trigger=other over actions_gateway_scaleset_jobs_completed_total is the manual re-run rate (Q555).",
		}, []string{"namespace", "runner_set", "trigger"}),

		ScaleSetFlakyJobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "actions_gateway_scaleset_flaky_jobs_total",
			Help: "A failed scale-set job later succeeded with no disruption attributed to its failure, by trigger (gag_collateral, other), as on actions_gateway_scaleset_job_reruns_total (Q555).",
		}, []string{"namespace", "runner_set", "trigger"}),
	}
	m.EvictionRerunOutcomes, m.ScaleSetJobReruns, m.ScaleSetFlakyJobs = f.EvictionRerunOutcomes, f.ScaleSetJobReruns, f.ScaleSetFlakyJobs
	metrics.Registry.MustRegister(m.EvictionRerunOutcomes, m.ScaleSetJobReruns, m.ScaleSetFlakyJobs)
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
