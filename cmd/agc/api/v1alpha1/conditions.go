package v1alpha1

// Condition types reported on a RunnerGroup's .status.conditions. These are the
// canonical, exported source of truth for the AGC reconciler, the listener
// goroutines that push conditions, the metrics collectors, tests, and any
// external consumer — never duplicate them as inline literals.
//
// Polarity follows docs/development/kubernetes-conventions.md: Ready is
// normal-is-True; problem conditions are abnormal-is-True (Degraded, RateLimited,
// RunnerVersionTooOld, CredentialUnavailable, and the two-tier quota ladder).
const (
	// ConditionReady is True when at least one listener goroutine is running.
	ConditionReady = "Ready"
	// ConditionDegraded is True when a listener session is unhealthy (e.g.
	// unauthorized) — set by the listener goroutine (abnormal-is-True).
	ConditionDegraded = "Degraded"
	// ConditionRateLimited is True when GitHub is rate-limiting the group's
	// sessions (abnormal-is-True).
	ConditionRateLimited = "RateLimited"
	// ConditionRunnerVersionTooOld is True when GitHub rejects the configured
	// runner version as too old (abnormal-is-True).
	ConditionRunnerVersionTooOld = "RunnerVersionTooOld"
	// ConditionCredentialUnavailable is True when the AGC cannot obtain a GitHub
	// App installation token to manage the group's agents (abnormal-is-True).
	ConditionCredentialUnavailable = "CredentialUnavailable" //nolint:gosec // G101: a condition type name, not a credential
	// ConditionWorkerQuotaPressure (warning) and ConditionWorkerQuotaExceeded
	// (error) are the two-tier namespace-ResourceQuota capacity ladder for worker
	// pods (Q82). See docs/development/kubernetes-conventions.md.
	ConditionWorkerQuotaPressure = "WorkerQuotaPressure"
	ConditionWorkerQuotaExceeded = "WorkerQuotaExceeded"
	// ConditionWorkersUnschedulable is True when one or more of the group's worker
	// pods have sat Pending past a scheduling grace for a non-quota reason — the
	// scheduler reports PodScheduled=False/Unschedulable (no node matches the pod's
	// resource requests, nodeSelector, affinity, or tolerations). It is distinct
	// from the WorkerQuota ladder: a ResourceQuota rejection blocks pod *admission*
	// (the pod is never created), so an unschedulable Pending pod can only reflect a
	// scheduler verdict, never quota exhaustion — the two never double-report (Q157,
	// abnormal-is-True). Impairing: capacity is not materializing, so it is rolled
	// up into the gateway's RunnerGroupsDegraded summary (see ImpairingConditionTypes).
	ConditionWorkersUnschedulable = "WorkersUnschedulable"
)

// Condition reasons reported alongside the condition types above. Reasons are
// CamelCase per Kubernetes API conventions (no spaces); contextual detail goes in
// the condition message.
const (
	// ReasonListenerActive and ReasonNoActiveSessions are Ready reasons.
	ReasonListenerActive   = "ListenerActive"
	ReasonNoActiveSessions = "NoActiveSessions"
	// ReasonTokenUnavailable is the CredentialUnavailable=True reason;
	// ReasonCredentialAvailable clears it (CredentialUnavailable=False).
	ReasonTokenUnavailable    = "TokenUnavailable"    //nolint:gosec // G101: a condition reason name, not a credential
	ReasonCredentialAvailable = "CredentialAvailable" //nolint:gosec // G101: a condition reason name, not a credential
	// ReasonPodsUnschedulable is the WorkersUnschedulable=True reason; the stuck
	// pods and the scheduler's verdict are named in the condition message.
	// ReasonWorkersSchedulable clears it (WorkersUnschedulable=False).
	ReasonPodsUnschedulable  = "PodsUnschedulable"
	ReasonWorkersSchedulable = "WorkersSchedulable"
)

// ImpairingConditionTypes returns the RunnerGroup condition types that, when
// True, mean the group cannot serve jobs — a credential failure, an unhealthy
// session, or a too-old runner version. Consumers that aggregate per-group health
// (the GMC's ActionsGateway RunnerGroupsDegraded rollup, Q158) iterate this set
// rather than hard-coding the list, so extending it here automatically widens the
// rollup. WorkersUnschedulable (Q157) is included: a group whose worker pods
// cannot be scheduled is not serving jobs.
//
// The capacity-ladder conditions (WorkerQuotaPressure/Exceeded) and RateLimited
// are deliberately excluded: they are advisory/transient throughput signals with
// their own gauges, not "the group is broken" — including them would make the
// rollup flap on normal load.
func ImpairingConditionTypes() []string {
	return []string{
		ConditionDegraded,
		ConditionCredentialUnavailable,
		ConditionRunnerVersionTooOld,
		ConditionWorkersUnschedulable,
	}
}
