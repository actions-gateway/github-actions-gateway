package v2alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EgressPolicyMode selects how the GMC expresses the proxy pool's GitHub egress
// allowlist.
//
//   - CIDR (the default) emits a standard Kubernetes NetworkPolicy whose egress
//     allowlist is the GitHub IP ranges, refreshed from api.github.com/meta every
//     24h by the GMC's IPRangeReconciler. It works on every NetworkPolicy-enforcing
//     CNI and requires no DNS awareness.
//   - CiliumFQDN / CalicoFQDN instead emit a CNI-native, DNS-aware egress policy
//     (a CiliumNetworkPolicy with toFQDNs, or a Calico NetworkPolicy with
//     destination domains) scoped to the GitHub hostnames — no CIDR feed to keep
//     current. These REQUIRE a CNI that enforces the corresponding DNS policy
//     (Cilium with toFQDNs / Calico with DNS-based policy); see the operator docs.
//
// The FQDN modes are fail-closed: the standard NetworkPolicy still default-denies
// GitHub egress, so if the CNI cannot enforce the native policy, GitHub egress stays
// denied rather than opening wide. Selecting an FQDN mode therefore never silently
// weakens the default.
//
// +kubebuilder:validation:Enum=CIDR;CiliumFQDN;CalicoFQDN
type EgressPolicyMode string

const (
	// EgressPolicyModeCIDR is the default: a standard NetworkPolicy with the GitHub
	// IP-range allowlist, refreshed every 24h. Works on every CNI.
	EgressPolicyModeCIDR EgressPolicyMode = "CIDR"
	// EgressPolicyModeCiliumFQDN emits a CiliumNetworkPolicy with toFQDNs rules
	// scoped to the GitHub hostnames. Requires Cilium with DNS-aware policy.
	EgressPolicyModeCiliumFQDN EgressPolicyMode = "CiliumFQDN"
	// EgressPolicyModeCalicoFQDN emits a Calico (projectcalico.org/v3) NetworkPolicy
	// with destination-domain rules scoped to the GitHub hostnames. Requires Calico
	// with DNS-based policy enabled.
	EgressPolicyModeCalicoFQDN EgressPolicyMode = "CalicoFQDN"
)

// EgressProxySpec is the desired state of a standalone, optionally shared egress
// proxy pool — v1alpha1's inline ActionsGateway.spec.proxy promoted to its own kind
// so any number of RunnerSets can point at one pool (§H.4, §H.5). Reconciled by the
// GMC, which owns the proxy Deployment/Service/HPA/PDB (the reconciler lands in M2).
//
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || !has(self.maxReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type EgressProxySpec struct {
	// MinReplicas is the floor of the proxy pool's HPA.
	//
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the ceiling of the proxy pool's HPA.
	//
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// TargetCPUUtilizationPercentage is the proxy HPA's target CPU utilization. This
	// is the managed-default knob; bring-your-own autoscaler is a deferred opt-out.
	//
	// +optional
	// +kubebuilder:default=60
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// Resources are the proxy container's resource requirements.
	//
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NoProxyCIDRs lists destinations excluded from the per-tenant egress proxy
	// (appended to NO_PROXY). Entries may be CIDR prefixes, bare IPs, or NO_PROXY
	// domain suffixes for internal destinations. Never list GitHub here — an entry
	// that routes GitHub traffic around the proxy defeats per-tenant egress-IP
	// attribution and is rejected by the GMC admission path.
	//
	// +optional
	NoProxyCIDRs []string `json:"noProxyCIDRs,omitempty"`

	// ManagedNetworkPolicy controls whether the GMC manages this proxy's egress
	// NetworkPolicy. Defaults to true (secure default).
	//
	// +optional
	// +kubebuilder:default=true
	ManagedNetworkPolicy *bool `json:"managedNetworkPolicy,omitempty"`

	// EgressPolicyMode selects how the GMC expresses the proxy pool's GitHub egress
	// allowlist: the default CIDR mode (standard NetworkPolicy + 24h IP-range
	// reconcile, works on every CNI) or a CNI-native DNS-aware FQDN mode
	// (CiliumFQDN / CalicoFQDN) that requires a DNS-aware policy CNI. It has no
	// effect when managedNetworkPolicy is false. See the EgressPolicyMode docs for
	// the secure-by-default (fail-closed) guarantee.
	//
	// +optional
	// +kubebuilder:default=CIDR
	EgressPolicyMode EgressPolicyMode `json:"egressPolicyMode,omitempty"`

	// Sharing controls cross-namespace reference to this proxy. nil means
	// same-namespace only (the default, secure). Consent lives on the provider
	// (proxy owner) side: a consumer-side name alone never authorizes the reference
	// (§H.9). v2 ships the inline allowlist only.
	//
	// +optional
	Sharing *ProxySharing `json:"sharing,omitempty"`
}

// EgressProxyStatus is the observed state of an EgressProxy, following the uniform
// v2 status/condition contract (§H.7). Nothing owns an EgressProxy and it owns its
// own children; deletion degrades referrers rather than blocking, so it carries no
// finalizer (§H.8).
type EgressProxyStatus struct {
	// Conditions are the observed conditions of the proxy pool. Known types: Ready.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ReadyReplicas is the number of ready proxy pods.
	//
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedGeneration is the .metadata.generation the most recent reconcile acted on.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// EgressProxy is a namespace-scoped CRD reconciled by the GMC into a shared egress
// proxy pool. It is referenced by RunnerSets (proxyRef) and ActionsGateways
// (defaultProxyRef) by name; referrers never own it (§H.8).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ep,categories=actions-gateway
// +kubebuilder:printcolumn:name="Min",type=integer,JSONPath=`.spec.minReplicas`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxReplicas`
// +kubebuilder:printcolumn:name="ReadyReplicas",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=='Ready')].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 52",message="metadata.name must be at most 52 characters: the GMC derives the <name>-proxy Service and reserves the remainder of the 63-char Service-name budget"
type EgressProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressProxySpec   `json:"spec,omitempty"`
	Status EgressProxyStatus `json:"status,omitempty"`
}

// EgressProxyList contains a list of EgressProxy.
//
// +kubebuilder:object:root=true
type EgressProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressProxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EgressProxy{}, &EgressProxyList{})
}
