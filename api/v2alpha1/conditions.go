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
	// ConditionEgressUnattributed is an advisory condition (abnormal-is-True) set True
	// on a proxy-less object: its egress reaches GitHub directly, so it has no
	// per-tenant egress IP identity. It does NOT gate Ready — direct egress is a
	// supported, NetworkPolicy-restricted mode (§H.10); the condition surfaces the
	// attribution trade-off an operator opted into by not attaching an EgressProxy, so
	// "no proxy" is an auditable state rather than an inferred one.
	ConditionEgressUnattributed = "EgressUnattributed"
)

// Egress proxy mode reported in status.proxyMode (§H.10). It makes "no proxy" an
// explicit, auditable state instead of an absent field to be inferred. Dropping the
// proxy drops egress *identity* (per-tenant IP attribution), never egress
// *restriction*: Direct egress is still default-deny egress allowing only DNS +
// GitHub CIDRs (+ the kube API server for the AGC control plane).
const (
	// ProxyModeProxied means egress flows through a resolved EgressProxy, giving the
	// workload stable per-tenant egress IPs (attribution).
	ProxyModeProxied = "Proxied"
	// ProxyModeDirect means no proxy resolved, so egress reaches GitHub directly —
	// still NetworkPolicy-restricted to GitHub CIDRs + DNS (+ kube API for the AGC),
	// but without per-tenant egress IP identity.
	ProxyModeDirect = "Direct"
)

// Template-resolution source reported in RunnerSet status.templateSource (Q172, §H.4):
// which rung of the optional-templateRef fallback chain supplied the worker pod shape.
// It makes "where did this set's template come from" an auditable status value rather
// than something an operator has to re-derive from the gateway and cluster state. It
// mirrors the proxyMode precedent: an explicit field for a fallback an unset reference
// resolves through.
const (
	// TemplateSourceRef means the RunnerSet's own spec.templateRef resolved the
	// template — the explicit, unchanged-from-required path.
	TemplateSourceRef = "TemplateRef"
	// TemplateSourceGatewayDefault means templateRef was unset and the gateway's
	// spec.defaultTemplateRef resolved the template.
	TemplateSourceGatewayDefault = "GatewayDefault"
	// TemplateSourceClusterDefault means neither templateRef nor the gateway's
	// defaultTemplateRef was set and the single cluster-default ClusterRunnerTemplate
	// (IsDefaultTemplateAnnotation) resolved the template.
	TemplateSourceClusterDefault = "ClusterDefault"
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
	// ReasonTemplateNotFound — no template resolved: the referenced
	// RunnerTemplate/ClusterRunnerTemplate does not exist, or (when templateRef and
	// gateway.defaultTemplateRef are both unset) no cluster-default ClusterRunnerTemplate
	// is marked. Fail-closed: no worker pod is ever synthesized without a pod shape (§H.4).
	ReasonTemplateNotFound = "TemplateNotFound"
	// ReasonAmbiguousDefault — the RunnerSet fell through to the cluster-default rung of
	// the template-resolution chain (Q172) but more than one ClusterRunnerTemplate is
	// marked the cluster default (IsDefaultTemplateAnnotation). Fail-closed: rather than
	// silently picking one, the set sits Ready=False until exactly one default remains.
	// At-most-one is enforced here, at runtime, not at admission (cross-object, GitOps-
	// safe; §H.7) — see docs/design/appendix-h-v2-api-decomposition.md §H.4.
	ReasonAmbiguousDefault = "AmbiguousDefault"
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
	// ReasonDirectEgress is the EgressUnattributed=True reason (and the proxyMode=Direct
	// rationale): no proxyRef/defaultProxyRef resolved, so egress is direct and
	// unattributed (still NetworkPolicy-restricted to GitHub).
	ReasonDirectEgress = "DirectEgress"
	// ReasonProxiedEgress is the EgressUnattributed=False reason: a proxy resolved, so
	// egress is attributed to the proxy's stable per-tenant IPs.
	ReasonProxiedEgress = "ProxiedEgress"
)
