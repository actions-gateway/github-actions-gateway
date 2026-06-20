package v1alpha1

// Condition types reported on an ActionsGateway's .status.conditions. These are
// the canonical, exported source of truth for the GMC reconciler, its metrics
// collectors, tests, and any external consumer — never duplicate them as inline
// literals.
//
// Polarity follows docs/development/kubernetes-conventions.md: availability
// conditions are normal-is-True (Ready, ProxyAvailable, AGCAvailable); problem
// conditions are abnormal-is-True (Degraded, CredentialUnavailable, and the
// two-tier quota ladder).
const (
	// ConditionReady is True when every component of the gateway is available.
	ConditionReady = "Ready"
	// ConditionProxyAvailable is True when the egress proxy pool has at least its
	// configured minimum ready replicas.
	ConditionProxyAvailable = "ProxyAvailable"
	// ConditionAGCAvailable is True when the tenant's AGC Deployment is ready.
	ConditionAGCAvailable = "AGCAvailable"
	// ConditionCredentialUnavailable is True when the referenced GitHub App
	// credential Secret is missing or unusable (abnormal-is-True).
	ConditionCredentialUnavailable = "CredentialUnavailable" //nolint:gosec // G101: a condition type name, not a credential
	// ConditionDegraded is True when a reconcile could not provision or update the
	// tenant's child resources; the failing step is named in the condition message
	// (abnormal-is-True).
	ConditionDegraded = "Degraded"
	// ConditionProxyQuotaPressure (warning) and ConditionProxyQuotaExceeded (error)
	// are the two-tier namespace-ResourceQuota capacity ladder for the proxy pool
	// (Q82). See docs/development/kubernetes-conventions.md.
	ConditionProxyQuotaPressure = "ProxyQuotaPressure"
	ConditionProxyQuotaExceeded = "ProxyQuotaExceeded"
	// ConditionRunnerGroupsDegraded is True when one or more of the gateway's owned
	// RunnerGroups reports an impairing condition (Q158), rolling child health up to
	// the ActionsGateway — the operator's single pane. Abnormal-is-True; advisory,
	// it does NOT gate Ready (the gateway infrastructure can be healthy while a
	// single tenant group is impaired). See agcv1alpha1.ImpairingConditionTypes.
	ConditionRunnerGroupsDegraded = "RunnerGroupsDegraded"
)

// Condition reasons reported alongside the condition types above. Reasons are
// CamelCase per Kubernetes API conventions (no spaces); contextual detail (the
// failing step, the missing Secret name) goes in the condition message.
const (
	// ReasonAllAvailable is the Ready=True reason.
	ReasonAllAvailable = "AllAvailable"
	// ReasonProxyReady and ReasonAGCReady describe the component-availability
	// conditions.
	ReasonProxyReady = "ProxyReady"
	ReasonAGCReady   = "AGCReady"
	// ReasonSecretFound and ReasonSecretNotFound are CredentialUnavailable reasons.
	ReasonSecretFound    = "SecretFound"
	ReasonSecretNotFound = "SecretNotFound"
	// ReasonProvisioningFailed is the Degraded=True reason; the failing step is
	// named in the message. ReasonReconcileSucceeded clears it (Degraded=False).
	ReasonProvisioningFailed = "ProvisioningFailed"
	ReasonReconcileSucceeded = "ReconcileSucceeded"
	// ReasonCredentialUnavailable and ReasonDegraded are Ready=False reasons that
	// attribute the not-ready state to a credential or provisioning failure.
	ReasonCredentialUnavailable = "CredentialUnavailable" //nolint:gosec // G101: a condition reason name, not a credential
	ReasonDegraded              = "Degraded"
	// ReasonRunnerGroupsImpaired is the RunnerGroupsDegraded=True reason (one or
	// more owned RunnerGroups report an impairing condition); the impaired groups
	// are named in the message. ReasonAllRunnerGroupsHealthy clears it
	// (RunnerGroupsDegraded=False), including when the gateway owns no RunnerGroups.
	ReasonRunnerGroupsImpaired   = "RunnerGroupsImpaired"
	ReasonAllRunnerGroupsHealthy = "AllRunnerGroupsHealthy"
)
