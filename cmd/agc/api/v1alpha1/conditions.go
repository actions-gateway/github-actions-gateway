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
)
