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
	ReasonTokenUnavailable    = "TokenUnavailable"
	ReasonCredentialAvailable = "CredentialAvailable" //nolint:gosec // G101: a condition reason name, not a credential
	// ReasonPodsUnschedulable is the WorkersUnschedulable=True reason; the stuck
	// pods and the scheduler's verdict are named in the condition message.
	// ReasonWorkersSchedulable clears it (WorkersUnschedulable=False).
	ReasonPodsUnschedulable  = "PodsUnschedulable"
	ReasonWorkersSchedulable = "WorkersSchedulable"
	// Reasons pushed by the listener goroutines (Q309). The classic acquisition
	// machinery is shared with the v2 RunnerSet, so these also land on v2 objects;
	// the v2 packages declare same-value constants (a value-parity test pins them).
	//
	// ReasonSustainedRateLimit is the RateLimited=True reason (message polling has
	// been answered 429 for over ten minutes).
	ReasonSustainedRateLimit = "SustainedRateLimit"
	// ReasonVersionTooOld is the RunnerVersionTooOld=True reason when GitHub itself
	// rejected the session as too old (classic tier only).
	ReasonVersionTooOld = "VersionTooOld"
	// ReasonVersionAccepted is the RunnerVersionTooOld=False reason the classic
	// listener publishes as its healthy baseline on session start: GitHub accepted
	// agent.version. It clears a ReasonVersionTooOld a previous instance left
	// behind, which nothing else does — the version is the AGC's own compile-time
	// pin, so the fix is a gateway upgrade and the condition survives it in status
	// (Q795), the same stale-forever shape Q332 closed for Degraded/RateLimited.
	// The reconciler drops it when a live condition stands whose reason is not the
	// listener's own, so it never overwrites a Q715 verdict.
	ReasonVersionAccepted = "VersionAccepted"
	// The reconciler's own reading of the worker image (Q715), published every
	// reconcile without asking GitHub: ReasonWorkerImageBelowMinimum is
	// RunnerVersionTooOld=True when the image's tag declares a runner version below
	// names.MinRunnerVersion, ReasonWorkerImageCurrent is False when it declares one
	// at or above it, and ReasonWorkerImageVersionUnknown is Unknown when the
	// reference declares no version at all so nothing has been checked.
	ReasonWorkerImageBelowMinimum   = "WorkerImageBelowMinimum"
	ReasonWorkerImageCurrent        = "WorkerImageCurrent"
	ReasonWorkerImageVersionUnknown = "WorkerImageVersionUnknown"
	// ReasonSessionUnauthorized is the Degraded=True reason pushed when session
	// creation is rejected as unauthorized — the agent credentials are invalid or
	// revoked.
	ReasonSessionUnauthorized = "Unauthorized"
	// ReasonPollingHealthy is the RateLimited=False reason: message polling is
	// healthy at session start or recovered after a sustained-429 episode. The
	// classic listener publishes it as the start baseline and on the first
	// successful poll after RateLimited=True, so a recovered rate-limit state does
	// not sit stale until the process restarts (Q332) — parity with the ScaleSet
	// listener's clear-on-recovery.
	ReasonPollingHealthy = "PollingHealthy"
	// ReasonSessionAuthorized is the Degraded=False reason: the listener's session
	// is authorized. The classic listener publishes it as the healthy baseline on
	// session start so a Degraded=True surfaced by a prior failed instance clears
	// once the credentials are fixed (Q332) — parity with the ScaleSet listener.
	ReasonSessionAuthorized = "SessionAuthorized"
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
