package v2beta1

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
//     operator backend. Prefer FQDN + --fqdn-policy-backend; these values still work,
//     and the earliest release that may remove them is v3.0.0 — NOT the v2.0.0 that
//     removes v1alpha1, v2alpha1, and classic acquisition. They are enum members of
//     the served beta version v2beta1, which v2.0.0 keeps serving; an API element is
//     removable only by incrementing the version, so the aliases live exactly as long
//     as v2beta1 does, and v3.0.0 is the earliest major tag that can retire it. See
//     docs/operations/v1alpha1-deprecation.md.
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
	// Prefer FQDN + --fqdn-policy-backend=cilium. Removable no earlier than v3.0.0; see
	// the EgressPolicyMode doc comment for why v2.0.0 cannot remove it.
	EgressPolicyModeCiliumFQDN EgressPolicyMode = "CiliumFQDN"
	// EgressPolicyModeCalicoFQDN is a DEPRECATED alias for FQDN that pins the Calico
	// backend (a projectcalico.org/v3 NetworkPolicy with destination domains) regardless
	// of --fqdn-policy-backend. Prefer FQDN + --fqdn-policy-backend=calico. Removable no
	// earlier than v3.0.0; see the EgressPolicyMode doc comment for why v2.0.0 cannot
	// remove it.
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
	// is the managed-default knob; set managedAutoscaling: false to bring your own
	// autoscaler instead.
	//
	// +optional
	// +kubebuilder:default=60
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// ManagedAutoscaling controls whether the GMC manages this proxy pool's
	// HorizontalPodAutoscaler (Q173). Defaults to true: the GMC provisions an HPA
	// scaling the pool on CPU between minReplicas and maxReplicas. Set to false to
	// bring your own autoscaler — the GMC then creates only the proxy Deployment
	// (stable "<name>-proxy" name and labels, replicas left to the external scaler)
	// and deletes any HPA it previously managed, so KEDA, VPA, or a custom HPA can
	// target the Deployment without fighting the managed one. While false:
	// maxReplicas and targetCPUUtilizationPercentage are inert; minReplicas seeds
	// only the Deployment's initial replica count; the Ready condition compares
	// ready pods against the Deployment's own desired replicas (an intentional
	// external scale-to-zero is Ready, not a wedge); and the managed
	// "<name>-proxy" HPA name stays reserved. Mirrors the managedNetworkPolicy
	// opt-out pattern; it shifts autoscaling ownership only and relaxes no
	// security property.
	//
	// +optional
	// +kubebuilder:default=true
	ManagedAutoscaling *bool `json:"managedAutoscaling,omitempty"`

	// Resources are the proxy container's resource requirements.
	//
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// LogLevel controls the log verbosity of this proxy pool. Allowed values: info
	// (default), debug. The GMC threads it to the proxy container as the LOG_LEVEL
	// environment variable; changing it is a rolling restart of the proxy pool (not
	// a hot reload), so the new level takes effect once the pods roll. Use debug
	// only for a bug repro — per-CONNECT debug lines dominate log volume under
	// load. The default is info so a pool never silently runs at debug verbosity.
	//
	// +optional
	// +kubebuilder:validation:Enum=info;debug
	// +kubebuilder:default=info
	LogLevel string `json:"logLevel,omitempty"`

	// AuditLogging selects the per-connection egress audit record this proxy pool
	// writes to its log stream. Off (the default) writes none. Connections writes
	// one structured line per ACCEPTED CONNECT, at tunnel close, carrying the
	// tenant namespace, the destination host and port, the bytes transferred each
	// way, and the tunnel duration. It carries nothing from the request headers
	// or the tunneled bytes, which the proxy never inspects.
	//
	// It is opt-in per pool because the record is data about a tenant's egress:
	// which destination their workers reached and when. Turning it on is a
	// deliberate decision to retain that, and to size the log pipeline for one
	// line per connection. Off is what an unset field, an unrecognized value, and
	// a proxy image older than this field all resolve to.
	//
	// Changing it is a rolling restart of the pool, not a hot reload, the same as
	// logLevel. Design appendix G.3 / Q564.
	//
	// +optional
	// +kubebuilder:validation:Enum=Off;Connections
	// +kubebuilder:default=Off
	AuditLogging string `json:"auditLogging,omitempty"`

	// NoProxyCIDRs lists destinations excluded from the per-tenant egress proxy
	// (appended to NO_PROXY). Entries may be CIDR prefixes, bare IPs, or NO_PROXY
	// domain suffixes for internal destinations. Never list GitHub here — an entry
	// that routes GitHub traffic around the proxy defeats per-tenant egress-IP
	// attribution and is rejected by the GMC admission path. The admission check
	// covers the public GitHub hosts plus the gitHubURL host — a GitHub Enterprise
	// Server host included — of every ActionsGateway or RunnerSet that references
	// this proxy, on both the proxy write and the referrer write.
	//
	// The cluster-internal destinations are appended automatically and need not be
	// listed: svc.cluster.local, localhost and 127.0.0.1 for both the AGC and its
	// workers, plus kubernetes.default.svc and this cluster's API server ClusterIP
	// (read from KUBERNETES_SERVICE_HOST) for the AGC, which dials the API server
	// by IP. Every entry added here bypasses the proxy and so escapes the
	// per-tenant egress-IP attribution: exempt specific internal destinations,
	// never a broad range.
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
	// CiliumFQDN/CalicoFQDN aliases pin their namesake backend and stay accepted until
	// v3.0.0 at the earliest. It has no effect when managedNetworkPolicy is false. See
	// the EgressPolicyMode docs for the secure-by-default (fail-closed) guarantee.
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
// +kubebuilder:storageversion
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
