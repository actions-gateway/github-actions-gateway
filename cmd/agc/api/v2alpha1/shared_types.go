package v2alpha1

// ObjectRef is a name-only reference to another v2 object in the same namespace.
//
// All v2 references share this one shape (gatewayRef, templateRef, proxyRef) so
// the API surface is uniform (docs/design/appendix-h-v2-api-decomposition.md §H.6).
// Cross-namespace references are deliberately not expressible here: a referent is
// always resolved in the referrer's own namespace, except where the referent kind
// itself grants cross-namespace use (the EgressProxy sharing handshake, a later
// milestone). Referential integrity is a runtime condition, not an admission gate
// (§H.7), so a ref naming a not-yet-applied object is well-formed — the controller
// surfaces a NotFound condition until it resolves.
type ObjectRef struct {
	// Name of the referenced object. Bounded by the same 52-char budget every v2
	// object name carries (§H.6), so a well-formed ref can always name a valid object.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=52
	Name string `json:"name"`

	// Kind optionally selects the referent kind when more than one is valid. It is
	// load-bearing only on templateRef, where it chooses between RunnerTemplate
	// (the namespaced default) and ClusterRunnerTemplate (a platform golden
	// template). gatewayRef and proxyRef each have a single valid kind, so Kind is
	// left empty there.
	//
	// +optional
	// +kubebuilder:validation:Enum=RunnerTemplate;ClusterRunnerTemplate
	Kind string `json:"kind,omitempty"`
}

// PriorityTier maps a Kubernetes PriorityClass to a cumulative pod-count
// threshold. Thresholds must be in strictly ascending order. Unchanged in meaning
// from v1alpha1; carried onto the RunnerSet (was RunnerGroup).
type PriorityTier struct {
	// PriorityClassName is the name of an existing cluster-scoped PriorityClass.
	// The platform owns which classes a tenant may reference: the GMC admission
	// path rejects any name not on the platform allowlist (--allowed-priority-classes),
	// so a tenant cannot name a high-priority, preempting class and evict other
	// tenants' worker pods. The named class — including its preemptionPolicy — is
	// platform-created; see docs/operations/security-operations.md.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	PriorityClassName string `json:"priorityClassName"`

	// Threshold is the cumulative active-pod count at which this tier is exhausted.
	//
	// +kubebuilder:validation:Minimum=1
	Threshold int32 `json:"threshold"`
}
