package v2alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EgressPolicyMode selects how the GMC expresses the proxy pool's GitHub egress
// allowlist. It is TENANT INTENT: the tenant says how egress should be expressed (by
// CIDR or by hostname); when hostname (FQDN) intent is chosen the platform operator —
// not the tenant — selects the enforcement mechanism via the GMC --fqdn-policy-backend
// flag (cilium, calico, or gke). This split keeps the tenant API stable as CNI/platform
// FQDN mechanisms proliferate (Q245).
//
//   - CIDR (the default) emits a standard Kubernetes NetworkPolicy whose egress
//     allowlist is the GitHub IP ranges, refreshed from api.github.com/meta every
//     24h by the GMC's IPRangeReconciler. It works on every NetworkPolicy-enforcing
//     CNI and requires no DNS awareness or operator backend.
//   - FQDN expresses the GitHub (and any Q242 destinationFQDNs) allowlist by hostname.
//     The GMC emits a CNI-native, DNS-aware egress policy whose kind is chosen by the
//     operator's --fqdn-policy-backend. FQDN intent is REJECTED at admission when the
//     cluster declares no backend (--fqdn-policy-backend=none, the default) — fail-
//     closed and loud, never a silent runtime Degraded.
//   - CiliumFQDN / CalicoFQDN are DEPRECATED aliases retained for backward
//     compatibility. Each pins its namesake mechanism (a CiliumNetworkPolicy with
//     toFQDNs, or a Calico NetworkPolicy with destination domains) regardless of the
//     operator backend. Prefer FQDN + --fqdn-policy-backend; these values still work
//     but will be removed in a future release, on the classic/v1alpha1 deprecation
//     clock.
//
// All FQDN-family modes are fail-closed: the standard NetworkPolicy still
// default-denies GitHub egress (it drops the GitHub-CIDR rule but keeps a DNS-only
// allow), so if the CNI cannot enforce the native policy — or the additive-allow GKE
// FQDNNetworkPolicy is absent — GitHub egress stays denied rather than opening wide.
// Selecting an FQDN mode therefore never silently weakens the default.
//
// +kubebuilder:validation:Enum=CIDR;FQDN;CiliumFQDN;CalicoFQDN
type EgressPolicyMode string

const (
	// EgressPolicyModeCIDR is the default: a standard NetworkPolicy with the GitHub
	// IP-range allowlist, refreshed every 24h. Works on every CNI.
	EgressPolicyModeCIDR EgressPolicyMode = "CIDR"
	// EgressPolicyModeFQDN expresses the GitHub egress allowlist by hostname. The
	// concrete CNI-native policy kind is chosen by the operator's --fqdn-policy-backend
	// (cilium/calico/gke); FQDN intent with no backend is rejected at admission.
	EgressPolicyModeFQDN EgressPolicyMode = "FQDN"
	// EgressPolicyModeCiliumFQDN is a DEPRECATED alias for FQDN that pins the Cilium
	// backend (a CiliumNetworkPolicy with toFQDNs) regardless of --fqdn-policy-backend.
	// Prefer FQDN + --fqdn-policy-backend=cilium.
	EgressPolicyModeCiliumFQDN EgressPolicyMode = "CiliumFQDN"
	// EgressPolicyModeCalicoFQDN is a DEPRECATED alias for FQDN that pins the Calico
	// backend (a projectcalico.org/v3 NetworkPolicy with destination domains) regardless
	// of --fqdn-policy-backend. Prefer FQDN + --fqdn-policy-backend=calico.
	EgressPolicyModeCalicoFQDN EgressPolicyMode = "CalicoFQDN"
)

// EgressProxySpec is the desired state of a standalone, optionally shared egress
// proxy pool — v1alpha1's inline ActionsGateway.spec.proxy promoted to its own kind
// so any number of RunnerSets can point at one pool (§H.4, §H.5). Reconciled by the
// GMC, which owns the proxy Deployment/Service/HPA/PDB (the reconciler lands in M2).
//
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || !has(self.maxReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
// +kubebuilder:validation:XValidation:rule="!has(self.destinationFQDNs) || self.destinationFQDNs.size() == 0 || (has(self.egressPolicyMode) && (self.egressPolicyMode == 'FQDN' || self.egressPolicyMode == 'CiliumFQDN' || self.egressPolicyMode == 'CalicoFQDN'))",message="destinationFQDNs requires an FQDN egressPolicyMode (FQDN, or deprecated CiliumFQDN/CalicoFQDN)"
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
	// attribution and is rejected by the GMC admission path. The admission check
	// covers the public GitHub hosts only: an entry matching a referring gateway's
	// GitHub Enterprise Server host is not yet detected (Q322) — never list that
	// either.
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
	// reconcile, works on every CNI) or an FQDN intent (a CNI-native DNS-aware policy
	// whose mechanism the operator picks via --fqdn-policy-backend). The deprecated
	// CiliumFQDN/CalicoFQDN aliases pin their namesake backend. It has no effect when
	// managedNetworkPolicy is false. See the EgressPolicyMode docs for the
	// secure-by-default (fail-closed) guarantee.
	//
	// +optional
	// +kubebuilder:default=CIDR
	EgressPolicyMode EgressPolicyMode `json:"egressPolicyMode,omitempty"`

	// DestinationFQDNs lists EXTRA, non-GitHub DNS host suffixes the proxy may
	// forward worker CONNECT traffic to (e.g. proxy.golang.org). GitHub is always
	// allowed and need not be listed; empty (the default) means GitHub-only.
	// Host-suffix entries REQUIRE an FQDN egressPolicyMode (FQDN, or the deprecated
	// CiliumFQDN/CalicoFQDN), since the pod-egress layer expresses them as toFQDNs
	// rules. Opening egress
	// beyond GitHub is an admin decision: the GMC rejects any entry not on its
	// --allowed-egress-fqdns platform allowlist (empty allowlist denies all).
	// G.1 / Q242; see design Appendix G.1 (Proxy-Enforced Destination Allowlist).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=64
	DestinationFQDNs []string `json:"destinationFQDNs,omitempty"`

	// DestinationCIDRs lists EXTRA, non-GitHub IP ranges the proxy may forward to
	// (e.g. an internal 10.x subnet with no DNS, or a cloud private-API range).
	// CIDR entries work in ANY egressPolicyMode — they become ipBlock egress peers
	// (CIDR mode) or toCIDR peers (FQDN mode). The GMC rejects any entry not
	// contained in its --allowed-egress-cidrs platform allowlist (empty denies all).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=64
	DestinationCIDRs []string `json:"destinationCIDRs,omitempty"`

	// Sharing controls cross-namespace reference to this proxy. nil means
	// same-namespace only (the default, secure). Consent lives on the provider
	// (proxy owner) side: a consumer-side name alone never authorizes the reference
	// (§H.9). v2 ships the inline allowlist only.
	//
	// +optional
	Sharing *ProxySharing `json:"sharing,omitempty"`

	// Scheduling pins this proxy pool's pods to specific nodes — the mechanism that
	// binds a tenant to a per-tenant egress IP (Q243/Q282), since the pod's node
	// determines which cloud NAT / egress path its traffic leaves by. A
	// podAntiAffinity set here replaces the built-in required cross-node spread; see
	// PodScheduling for the full precedence rules and the tenant-settable-by-design
	// rationale.
	//
	// +optional
	Scheduling *PodScheduling `json:"scheduling,omitempty"`
}

// EgressProxyStatus is the observed state of an EgressProxy, following the uniform
// v2 status/condition contract (§H.7). Nothing owns an EgressProxy and it owns its
// own children; deletion degrades referrers rather than blocking, so it carries no
// finalizer (§H.8).
type EgressProxyStatus struct {
	// Conditions are the observed conditions of the proxy pool. Known types: Ready,
	// Degraded, ProxyQuotaPressure, ProxyQuotaExceeded, EgressRulesStale (Q320).
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
