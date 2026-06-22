package v2alpha1

// Condition types and reasons reported on v2 kinds' .status.conditions. These are
// the canonical, exported source of truth — never duplicate them as inline literals.
// The v2 status/condition contract is uniform across all five kinds
// (docs/design/appendix-h-v2-api-decomposition.md §H.7): every kind carries a
// listType=map Conditions slice keyed on type, an ObservedGeneration, and a Ready
// condition with normal-is-True polarity and a shared reason vocabulary; messages
// name the specific blocker (e.g. the missing template or Secret), never a generic
// string.
//
// The reconcilers that set these conditions land in later milestones (M2 data kinds
// + EgressProxy, M3a control kinds); the contract is pinned here so no reconciler
// invents its own.
const (
	// ConditionReady is True when every component of the object is available — for an
	// ActionsGateway, the AGC control plane; for an EgressProxy, the proxy pool; for a
	// RunnerSet, that its references resolved and at least one listener is running; for
	// the data kinds it is reserved for a future validating reconciler.
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
	// ReasonProxyNotReady is the EgressProxy Ready=False reason while the proxy
	// pool is provisioned but has fewer than minReplicas ready pods.
	ReasonProxyNotReady = "ProxyNotReady"
	// ReasonSecretNotFound is the CredentialUnavailable=True reason.
	ReasonSecretNotFound = "SecretNotFound"
	// ReasonProvisioningFailed is the Degraded=True reason; the failing step is named
	// in the message. ReasonReconcileSucceeded clears it.
	ReasonProvisioningFailed = "ProvisioningFailed"
	ReasonReconcileSucceeded = "ReconcileSucceeded"
)

// Ready=False reasons for RunnerSet reference resolution (§H.7). Resolution is a
// runtime concern, not an admission gate, so a set pointing at a not-yet-applied
// referent reports one of these (fail-closed: no worker pods until it resolves)
// and flips to Ready the moment the referent syncs.
const (
	// ReasonGatewayNotFound — the referenced ActionsGateway does not exist.
	ReasonGatewayNotFound = "GatewayNotFound"
	// ReasonTemplateNotFound — the referenced RunnerTemplate/ClusterRunnerTemplate does not exist.
	ReasonTemplateNotFound = "TemplateNotFound"
	// ReasonTemplateDeleted — a previously-resolved template was deleted (degrade-not-block, §H.8).
	ReasonTemplateDeleted = "TemplateDeleted"
	// ReasonProxyNotFound — the referenced EgressProxy does not exist.
	ReasonProxyNotFound = "ProxyNotFound"
	// ReasonProxyDeleted — a previously-resolved proxy was deleted.
	ReasonProxyDeleted = "ProxyDeleted"
	// ReasonProxyShareNotGranted — a cross-namespace EgressProxy has not granted
	// this namespace (the provider-side consent handshake, §H.9).
	ReasonProxyShareNotGranted = "ProxyShareNotGranted"
	// ReasonNoActiveSessions — a RunnerSet's references all resolved but no
	// listener goroutine is running yet (Ready=False until one comes up).
	ReasonNoActiveSessions = "NoActiveSessions"
	// ReasonListenerActive — a RunnerSet's references resolved and at least one
	// listener goroutine is running (Ready=True).
	ReasonListenerActive = "ListenerActive"
	// ReasonTokenUnavailable — the AGC could not obtain a GitHub App installation
	// token, so the RunnerSet cannot register runners (Ready=False).
	ReasonTokenUnavailable = "TokenUnavailable" //nolint:gosec // G101: a condition reason, not a credential
)
