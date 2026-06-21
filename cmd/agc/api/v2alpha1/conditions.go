package v2alpha1

// Condition types and reasons reported on v2 AGC kinds' .status.conditions. These
// are the canonical, exported source of truth — never duplicate them as inline
// literals. The v2 status/condition contract is uniform across all five kinds
// (docs/design/appendix-h-v2-api-decomposition.md §H.7): every kind carries a
// listType=map Conditions slice keyed on type, an ObservedGeneration, and a Ready
// condition with the same polarity (normal-is-True) and a shared reason vocabulary;
// messages name the specific blocker (e.g. the missing template), never a generic
// string.
//
// The reconcilers that set these conditions land in later milestones (M2 data
// kinds, M3a control kinds); the contract is pinned here in M1 so no reconciler
// invents its own.
const (
	// ConditionReady is True when the object is fully wired and serving. For a
	// RunnerSet that means its references resolved and at least one listener is
	// running; for the data kinds it is reserved for a future validating reconciler.
	ConditionReady = "Ready"
)

// Ready=True reason.
const (
	// ReasonReady is the Ready=True reason.
	ReasonReady = "Ready"
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
)
