package v1alpha1

import (
	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretReference is a pointer to a Kubernetes Secret.
//
// Cross-namespace references are not supported; Namespace must be left empty.
// The admission webhook enforces this at create/update time. A CEL XValidation
// rule is intentionally omitted: k8s ≤ 1.30 CEL cannot use has() on optional
// non-pointer string fields, so the rule would fail to install on those versions.
type SecretReference struct {
	Name string `json:"name"`
	// Namespace must be left empty. Cross-namespace Secret references are not
	// supported; the referenced Secret must reside in the ActionsGateway's own
	// namespace. This field is reserved for a future protocol extension.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ProxyConfig configures the per-tenant egress proxy pool.
//
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || !has(self.maxReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type ProxyConfig struct {
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// +optional
	// +kubebuilder:default=60
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// +optional
	NoProxyCIDRs []string `json:"noProxyCIDRs,omitempty"`

	// +optional
	// +kubebuilder:default=true
	ManagedNetworkPolicy *bool `json:"managedNetworkPolicy,omitempty"`
}

// ActionsGatewaySpec is the desired state of an ActionsGateway.
//
// The securityProfile no-downgrade rule below runs only on update (it references
// oldSelf). securityProfile may be *upgraded* (e.g. baseline->restricted) but
// never downgraded: the rule ranks the profiles privileged(0) < baseline(1) <
// restricted(2) and rejects any update that lowers the rank, closing the
// silent-security-downgrade hole (D5) while still allowing in-place hardening.
//
// gitHubAppRef.name is deliberately NOT pinned: changing it is the supported
// credential-rotation mechanism (point the tenant at a freshly-minted App
// Secret), exercised by TestGMC_TenantProvisioning_CredentialRotation. An
// immutability rule there would break rotation, so it is omitted.
//
// +kubebuilder:validation:XValidation:rule="{'privileged':0,'baseline':1,'restricted':2}[self.securityProfile] >= {'privileged':0,'baseline':1,'restricted':2}[oldSelf.securityProfile]",message="securityProfile may be upgraded (e.g. baseline->restricted) but not downgraded; downgrading is a silent security regression — recreate the ActionsGateway instead"
type ActionsGatewaySpec struct {
	GitHubAppRef SecretReference `json:"gitHubAppRef"`
	// +optional
	Proxy ProxyConfig `json:"proxy,omitempty"`
	// RunnerGroups lists RunnerGroup specs bootstrapped in the tenant namespace.
	// +optional
	RunnerGroups []agcv1alpha1.RunnerGroupSpec `json:"runnerGroups,omitempty"`
	// +optional
	NamespaceQuota corev1.ResourceList `json:"namespaceQuota,omitempty"`
	// SecurityProfile controls the Pod Security Admission enforcement level
	// applied to the tenant namespace. Allowed values: baseline (default),
	// restricted, privileged. Use privileged only for workloads that run
	// Docker-in-Docker or require host-level capabilities.
	//
	// +optional
	// +kubebuilder:validation:Enum=baseline;restricted;privileged
	// +kubebuilder:default=baseline
	SecurityProfile string `json:"securityProfile,omitempty"`
}

// ActionsGatewayStatus is the observed state of an ActionsGateway.
type ActionsGatewayStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ProxyReadyReplicas int32 `json:"proxyReadyReplicas,omitempty"`
	// +optional
	ActiveSessions int32 `json:"activeSessions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ActionsGateway is a namespace-scoped CRD managed by the GMC.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag,categories=actions-gateway
// +kubebuilder:printcolumn:name="ProxyReady",type=integer,JSONPath=".status.proxyReadyReplicas"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="ObservedGen",type=integer,priority=1,JSONPath=".status.observedGeneration"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type ActionsGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ActionsGatewaySpec   `json:"spec,omitempty"`
	Status            ActionsGatewayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ActionsGatewayList contains a list of ActionsGateway.
type ActionsGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActionsGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ActionsGateway{}, &ActionsGatewayList{})
}
