package v2alpha1

import "github.com/actions-gateway/github-actions-gateway/api/apiconditions"

// The v2 condition/reason vocabulary is declared once, version-neutrally, in
// api/apiconditions — a condition type or reason is a runtime .status.conditions[]
// value, not schema, and the storage/hub conversion contract requires both served
// versions to name the same states identically (§H.7). This file re-exports that
// vocabulary under this package so every `v2alpha1.ConditionX` / `v2alpha1.ReasonX`
// call site keeps compiling; api/v2beta1/conditions.go carries the identical block.
//
// Read apiconditions for what each name means and when a reconciler sets it — the
// doc comments live there, with the values, so they cannot diverge between versions.
//
// Adding a condition or reason: declare it in apiconditions, then add the one-line
// re-export HERE and in api/v2beta1/conditions.go. The two files must stay
// byte-identical except the package clause; scripts/go/check-v2-api-sync.sh fails the
// build on a one-sided add (Q345, widened in Q374).
const (
	ConditionReady                       = apiconditions.ConditionReady
	ConditionAGCAvailable                = apiconditions.ConditionAGCAvailable
	ConditionCredentialUnavailable       = apiconditions.ConditionCredentialUnavailable
	ConditionDegraded                    = apiconditions.ConditionDegraded
	ConditionEgressUnattributed          = apiconditions.ConditionEgressUnattributed
	ConditionAGCAutoscalingUnavailable   = apiconditions.ConditionAGCAutoscalingUnavailable
	ConditionScaleSetNameCollision       = apiconditions.ConditionScaleSetNameCollision
	ConditionPossibleReapBlockingSidecar = apiconditions.ConditionPossibleReapBlockingSidecar
	ConditionWorkerQuotaPressure         = apiconditions.ConditionWorkerQuotaPressure
	ConditionWorkerQuotaExceeded         = apiconditions.ConditionWorkerQuotaExceeded
	ConditionWorkersUnschedulable        = apiconditions.ConditionWorkersUnschedulable
	ConditionWorkerCapacityDeclined      = apiconditions.ConditionWorkerCapacityDeclined
	ConditionRunnerSetsDegraded          = apiconditions.ConditionRunnerSetsDegraded
	ConditionProxyQuotaPressure          = apiconditions.ConditionProxyQuotaPressure
	ConditionProxyQuotaExceeded          = apiconditions.ConditionProxyQuotaExceeded
	ConditionEgressRulesStale            = apiconditions.ConditionEgressRulesStale
	ConditionGitHubEgressIncomplete      = apiconditions.ConditionGitHubEgressIncomplete
	ConditionRateLimited                 = apiconditions.ConditionRateLimited
	ConditionRunnerVersionTooOld         = apiconditions.ConditionRunnerVersionTooOld
	ConditionSizingDrift                 = apiconditions.ConditionSizingDrift
	ConditionSizingProfileOverridden     = apiconditions.ConditionSizingProfileOverridden
	ConditionJobProvisionStalled         = apiconditions.ConditionJobProvisionStalled
	ConditionRunnerLabelsIncomplete      = apiconditions.ConditionRunnerLabelsIncomplete
)

// Egress proxy mode (status.proxyMode) and RunnerSet template-resolution source
// (status.templateSource) — status enums that share the vocabulary's version
// neutrality.
const (
	ProxyModeProxied = apiconditions.ProxyModeProxied
	ProxyModeDirect  = apiconditions.ProxyModeDirect

	TemplateSourceRef            = apiconditions.TemplateSourceRef
	TemplateSourceGatewayDefault = apiconditions.TemplateSourceGatewayDefault
	TemplateSourceClusterDefault = apiconditions.TemplateSourceClusterDefault
)

// Condition reasons.
const (
	ReasonReady                   = apiconditions.ReasonReady
	ReasonAGCReady                = apiconditions.ReasonAGCReady
	ReasonAGCNotReady             = apiconditions.ReasonAGCNotReady
	ReasonProxyReady              = apiconditions.ReasonProxyReady
	ReasonProxyNotReady           = apiconditions.ReasonProxyNotReady
	ReasonSecretNotFound          = apiconditions.ReasonSecretNotFound
	ReasonProvisioningFailed      = apiconditions.ReasonProvisioningFailed
	ReasonReconcileSucceeded      = apiconditions.ReasonReconcileSucceeded
	ReasonGatewayNotFound         = apiconditions.ReasonGatewayNotFound
	ReasonGatewayTerminating      = apiconditions.ReasonGatewayTerminating
	ReasonTemplateNotFound        = apiconditions.ReasonTemplateNotFound
	ReasonAmbiguousDefault        = apiconditions.ReasonAmbiguousDefault
	ReasonTemplateDeleted         = apiconditions.ReasonTemplateDeleted
	ReasonProxyNotFound           = apiconditions.ReasonProxyNotFound
	ReasonProxyDeleted            = apiconditions.ReasonProxyDeleted
	ReasonProxyShareNotGranted    = apiconditions.ReasonProxyShareNotGranted
	ReasonCABundleNotFound        = apiconditions.ReasonCABundleNotFound
	ReasonCABundleInvalid         = apiconditions.ReasonCABundleInvalid
	ReasonNoActiveSessions        = apiconditions.ReasonNoActiveSessions
	ReasonRunnerGroupNotFound     = apiconditions.ReasonRunnerGroupNotFound
	ReasonListenerActive          = apiconditions.ReasonListenerActive
	ReasonTokenUnavailable        = apiconditions.ReasonTokenUnavailable
	ReasonDirectEgress            = apiconditions.ReasonDirectEgress
	ReasonProxiedEgress           = apiconditions.ReasonProxiedEgress
	ReasonVPACRDNotInstalled      = apiconditions.ReasonVPACRDNotInstalled
	ReasonAGCAutoscalingActive    = apiconditions.ReasonAGCAutoscalingActive
	ReasonAGCAutoscalingDisabled  = apiconditions.ReasonAGCAutoscalingDisabled
	ReasonReapBlockingSidecar     = apiconditions.ReasonReapBlockingSidecar
	ReasonNoReapBlockingSidecar   = apiconditions.ReasonNoReapBlockingSidecar
	ReasonPodsUnschedulable       = apiconditions.ReasonPodsUnschedulable
	ReasonWorkersSchedulable      = apiconditions.ReasonWorkersSchedulable
	ReasonCapacityAvailable       = apiconditions.ReasonCapacityAvailable
	ReasonScaleUpDeclined         = apiconditions.ReasonScaleUpDeclined
	ReasonAwaitingProbe           = apiconditions.ReasonAwaitingProbe
	ReasonGateModeUnsupported     = apiconditions.ReasonGateModeUnsupported
	ReasonAgentProvisioningFailed = apiconditions.ReasonAgentProvisioningFailed
	ReasonListenerStartFailed     = apiconditions.ReasonListenerStartFailed
	ReasonRunnerSetsImpaired      = apiconditions.ReasonRunnerSetsImpaired
	ReasonAllRunnerSetsHealthy    = apiconditions.ReasonAllRunnerSetsHealthy
	ReasonRefreshStalled          = apiconditions.ReasonRefreshStalled
	ReasonRefreshCurrent          = apiconditions.ReasonRefreshCurrent
	ReasonRefreshPending          = apiconditions.ReasonRefreshPending
	ReasonSustainedRateLimit      = apiconditions.ReasonSustainedRateLimit
	ReasonVersionTooOld           = apiconditions.ReasonVersionTooOld
	ReasonSessionUnauthorized     = apiconditions.ReasonSessionUnauthorized
	ReasonPollingHealthy          = apiconditions.ReasonPollingHealthy
	ReasonSessionAuthorized       = apiconditions.ReasonSessionAuthorized
	ReasonSizingDriftDetected     = apiconditions.ReasonSizingDriftDetected
	ReasonSizingWithinRange       = apiconditions.ReasonSizingWithinRange
	ReasonInsufficientSamples     = apiconditions.ReasonInsufficientSamples
	ReasonSizingProfileActive     = apiconditions.ReasonSizingProfileActive
	ReasonCPULimitInjected        = apiconditions.ReasonCPULimitInjected
	ReasonNoCPULimitInjected      = apiconditions.ReasonNoCPULimitInjected
	ReasonAwaitingWorkerPods      = apiconditions.ReasonAwaitingWorkerPods
	ReasonLabelsNotRegistered     = apiconditions.ReasonLabelsNotRegistered
	ReasonLabelsRegistered        = apiconditions.ReasonLabelsRegistered
	ReasonApplianceRangesRequired = apiconditions.ReasonApplianceRangesRequired
	ReasonGitHubEgressAllowed     = apiconditions.ReasonGitHubEgressAllowed
	ReasonRunnerNameConflict      = apiconditions.ReasonRunnerNameConflict
	ReasonWorkerCeilingReached    = apiconditions.ReasonWorkerCeilingReached
	ReasonJobsProvisioning        = apiconditions.ReasonJobsProvisioning
	ReasonScaleSetNameShared      = apiconditions.ReasonScaleSetNameShared
	ReasonScaleSetNamesUnique     = apiconditions.ReasonScaleSetNamesUnique

	ReasonWorkerImageBelowMinimum   = apiconditions.ReasonWorkerImageBelowMinimum
	ReasonWorkerImageCurrent        = apiconditions.ReasonWorkerImageCurrent
	ReasonWorkerImageVersionUnknown = apiconditions.ReasonWorkerImageVersionUnknown
)

// ImpairingConditionTypes returns the abnormal-is-True RunnerSet condition types that,
// when True, mean the set cannot serve jobs — the set the GMC's ActionsGateway
// RunnerSetsDegraded rollup iterates (Q304, Q330). See apiconditions for the rationale
// and for why the advisory/transient conditions are excluded.
func ImpairingConditionTypes() []string { return apiconditions.ImpairingConditionTypes() }
