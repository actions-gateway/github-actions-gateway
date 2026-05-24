package v1alpha1

import (
	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretReference is a pointer to a Kubernetes Secret with optional namespace override.
type SecretReference struct {
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ProxyConfig configures the per-tenant egress proxy pool.
type ProxyConfig struct {
	// +optional
	// +kubebuilder:default=2
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// +optional
	// +kubebuilder:default=10
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
type ActionsGatewaySpec struct {
	GitHubAppRef SecretReference `json:"gitHubAppRef"`
	// +optional
	Proxy ProxyConfig `json:"proxy,omitempty"`
	// RunnerGroups lists RunnerGroup specs bootstrapped in the tenant namespace.
	// +optional
	RunnerGroups []agcv1alpha1.RunnerGroupSpec `json:"runnerGroups,omitempty"`
	// +optional
	NamespaceQuota corev1.ResourceList `json:"namespaceQuota,omitempty"`
}

// ActionsGatewayStatus is the observed state of an ActionsGateway.
type ActionsGatewayStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ProxyReadyReplicas int32              `json:"proxyReadyReplicas"`
	ActiveSessions     int32              `json:"activeSessions"`
	ObservedGeneration int64              `json:"observedGeneration"`
}

// ActionsGateway is a namespace-scoped CRD managed by the GMC.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:printcolumn:name="ProxyReady",type=integer,JSONPath=".status.proxyReadyReplicas"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
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
