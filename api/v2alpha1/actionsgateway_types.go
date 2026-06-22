package v2alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ActionsGatewaySpec is the desired state of a v2 ActionsGateway: the GitHub
// identity plus the AGC control plane only. v2 decomposes the v1alpha1 monolith —
// the inline proxy moves to a standalone EgressProxy and the bootstrap runner
// groups become explicit RunnerSet objects, both removed from this spec (§H.4).
// Multiple ActionsGateways are permitted per namespace in v2 (the singleton rule is
// dropped); the multi-gateway controller behavior lands in M3b.
//
// The Pod Security Admission level is NOT a field here. v1alpha1 hung securityProfile
// on this per-gateway object, but Pod Security Admission is a namespace-scoped control
// in Kubernetes, so under multi-gateway two gateways in one namespace would fight over
// the single namespace PSA label. v2 moves the profile to the namespace itself — the
// operator sets the actions-gateway.com/security-profile label on the tenant namespace
// and the GMC stamps the PSA labels from it (GMC-guarded, §H.16 #7). See
// docs/operations/security-operations.md.
type ActionsGatewaySpec struct {
	// GitHubAppRef names the Secret holding this gateway's GitHub App credentials,
	// in the gateway's own namespace (LocalSecretReference — v1's namespace field is
	// dropped, §H.15). githubAppRef.name is deliberately mutable: changing it is the
	// supported credential-rotation path.
	GitHubAppRef LocalSecretReference `json:"githubAppRef"`

	// GitHubURL is the GitHub organization, enterprise, or repository URL this
	// gateway's runners register against (e.g. "https://github.com/my-org"). It is
	// immutable: rebinding a running gateway's GitHub org is a footgun, so v2 freezes
	// it via a CEL transition rule (§H.15). Casing follows the v2 convention —
	// "github" is one lowercased word, the trailing initialism stays uppercase.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="githubURL is immutable; create a new ActionsGateway to bind a different GitHub org/enterprise/repo"
	GitHubURL string `json:"githubURL"`

	// DefaultProxyRef names an EgressProxy used for AGC control-plane egress and
	// inherited by RunnerSets under this gateway that do not set their own proxyRef.
	// Optional: unset means the control plane egresses directly (subject to
	// NetworkPolicy). Same-namespace unless the target EgressProxy grants
	// cross-namespace use (§H.4, §H.9).
	//
	// +optional
	DefaultProxyRef *LocalObjectRef `json:"defaultProxyRef,omitempty"`

	// LogLevel controls the log verbosity of this tenant's AGC. Allowed values: info
	// (default), debug. Changing it is a rolling restart of the AGC, not a hot
	// reload. Use debug only for a bug repro.
	//
	// +optional
	// +kubebuilder:validation:Enum=info;debug
	// +kubebuilder:default=info
	LogLevel string `json:"logLevel,omitempty"`

	// Tracing configures opt-in OpenTelemetry distributed tracing for this tenant's
	// AGC. Tracing stays off unless tracing.endpoint is set.
	//
	// +optional
	Tracing TracingConfig `json:"tracing,omitempty"`
}

// ActionsGatewayStatus is the observed state of an ActionsGateway, following the
// uniform v2 status/condition contract (§H.7).
type ActionsGatewayStatus struct {
	// Conditions are the observed conditions of the gateway. Known types: Ready,
	// AGCAvailable, CredentialUnavailable, Degraded.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation the most recent reconcile acted on.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ActionsGateway is a namespace-scoped CRD reconciled by the GMC: it binds a GitHub
// identity and provisions the per-tenant AGC control plane. v2 permits multiple per
// namespace (multi-gateway support lands in M3b).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag,categories=actions-gateway
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.githubURL`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=='Ready')].reason`
// +kubebuilder:printcolumn:name="ObservedGen",type=integer,priority=1,JSONPath=`.status.observedGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 52",message="metadata.name must be at most 52 characters: the GMC derives child resource names as <name>-<suffix> and reserves the remainder of the 63-char label/Service-name budget"
type ActionsGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ActionsGatewaySpec   `json:"spec,omitempty"`
	Status ActionsGatewayStatus `json:"status,omitempty"`
}

// ActionsGatewayList contains a list of ActionsGateway.
//
// +kubebuilder:object:root=true
type ActionsGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActionsGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ActionsGateway{}, &ActionsGatewayList{})
}
