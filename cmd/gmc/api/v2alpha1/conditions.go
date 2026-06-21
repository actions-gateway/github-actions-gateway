package v2alpha1

// Condition types and reasons reported on v2 GMC kinds' .status.conditions. These
// are the canonical, exported source of truth — never duplicate them as inline
// literals. The v2 status/condition contract is uniform across all five kinds
// (docs/design/appendix-h-v2-api-decomposition.md §H.7): every kind carries a
// listType=map Conditions slice keyed on type, an ObservedGeneration, and a Ready
// condition with normal-is-True polarity and a shared reason vocabulary; messages
// name the specific blocker, never a generic string.
//
// The reconcilers that set these conditions land in later milestones (M2 EgressProxy,
// M3a ActionsGateway); the contract is pinned here in M1.
const (
	// ConditionReady is True when every component of the object is available — for an
	// ActionsGateway, the AGC control plane; for an EgressProxy, the proxy pool.
	ConditionReady = "Ready"
	// ConditionAGCAvailable is True when the tenant's AGC Deployment is ready.
	ConditionAGCAvailable = "AGCAvailable"
	// ConditionCredentialUnavailable is True when the referenced GitHub App
	// credential Secret is missing or unusable (abnormal-is-True).
	ConditionCredentialUnavailable = "CredentialUnavailable" //nolint:gosec // G101: a condition type name, not a credential
	// ConditionDegraded is True when a reconcile could not provision or update the
	// object's children; the failing step is named in the message (abnormal-is-True).
	ConditionDegraded = "Degraded"
)

// Condition reasons. Reasons are CamelCase per Kubernetes API conventions;
// contextual detail (the failing step, the missing Secret name) goes in the message.
const (
	// ReasonReady is the Ready=True reason.
	ReasonReady = "Ready"
	// ReasonAGCReady is the AGCAvailable=True reason.
	ReasonAGCReady = "AGCReady"
	// ReasonProxyReady is the EgressProxy Ready=True reason.
	ReasonProxyReady = "ProxyReady"
	// ReasonSecretNotFound is the CredentialUnavailable=True reason.
	ReasonSecretNotFound = "SecretNotFound"
	// ReasonProvisioningFailed is the Degraded=True reason; the failing step is named
	// in the message. ReasonReconcileSucceeded clears it.
	ReasonProvisioningFailed = "ProvisioningFailed"
	ReasonReconcileSucceeded = "ReconcileSucceeded"
)
