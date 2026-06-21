package v2alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunnerTemplateSpec is the worker pod shape shared by RunnerTemplate (namespaced)
// and ClusterRunnerTemplate (cluster-scoped). It is the only v2 object permitted to
// be large: isolating the PodTemplateSpec here keeps it out of the controller
// objects it would otherwise co-bloat, and lets one template be referenced by many
// RunnerSets (docs/design/appendix-h-v2-api-decomposition.md §H.2, §H.5).
//
// Reserved pod-field guardrail (§H.4, §H.7). A worker pod's identity and isolation
// are controller-enforced invariants — in v1 the AGC silently overrode them at pod
// build time. v2 makes them author-time rejections instead, so a template that
// tries to set them fails closed rather than being silently rewritten. The cheap,
// scalar pod-level fields are enforced here via CEL; the per-container checks that
// require iterating an unbounded containers array (privileged containers, proxy
// env vars) exceed the CEL cost budget and move to the RunnerTemplate admission
// webhook in M2 (the milestone that adds the data-kind reconcilers).
//
// +kubebuilder:validation:XValidation:rule="!has(self.podTemplate.spec.serviceAccountName) || size(self.podTemplate.spec.serviceAccountName) == 0",message="podTemplate.spec.serviceAccountName is reserved: the AGC binds the worker ServiceAccount; leave it unset"
// +kubebuilder:validation:XValidation:rule="!has(self.podTemplate.spec.automountServiceAccountToken) || !self.podTemplate.spec.automountServiceAccountToken",message="podTemplate.spec.automountServiceAccountToken is reserved and must not be enabled: worker pods do not mount a ServiceAccount token"
// +kubebuilder:validation:XValidation:rule="!has(self.podTemplate.spec.hostPID) || !self.podTemplate.spec.hostPID",message="podTemplate.spec.hostPID is reserved and must not be enabled on worker pods"
// +kubebuilder:validation:XValidation:rule="!has(self.podTemplate.spec.hostNetwork) || !self.podTemplate.spec.hostNetwork",message="podTemplate.spec.hostNetwork is reserved and must not be enabled on worker pods"
// +kubebuilder:validation:XValidation:rule="!has(self.podTemplate.spec.hostIPC) || !self.podTemplate.spec.hostIPC",message="podTemplate.spec.hostIPC is reserved and must not be enabled on worker pods"
type RunnerTemplateSpec struct {
	// PodTemplate is the standard Kubernetes PodTemplateSpec for worker pods — the
	// large field this kind exists to isolate.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`

	// WorkerImage is the fully-qualified container image for the runner container.
	//
	// +optional
	WorkerImage string `json:"workerImage,omitempty"`
}

// RunnerTemplateStatus is the observed state of a (Cluster)RunnerTemplate. The
// templates are pure data with no reconciler in M1/M2, so the contract fields are
// present for uniformity across all five v2 kinds (§H.7) and forward-compatibility
// (a future validating reconciler could surface a Ready condition) rather than
// being populated today.
type RunnerTemplateStatus struct {
	// Conditions are the observed conditions of the template. Known types: Ready.
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

// RunnerTemplate is a namespace-scoped, reusable worker pod shape referenced by
// RunnerSets via templateRef. Pure data: nothing owns it and it owns nothing (§H.8).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rt,categories=actions-gateway
// +kubebuilder:printcolumn:name="WorkerImage",type=string,JSONPath=`.spec.workerImage`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 52",message="metadata.name must be at most 52 characters: referenced by templateRef, which shares the v2 52-char name budget"
type RunnerTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunnerTemplateSpec   `json:"spec,omitempty"`
	Status RunnerTemplateStatus `json:"status,omitempty"`
}

// RunnerTemplateList contains a list of RunnerTemplate.
//
// +kubebuilder:object:root=true
type RunnerTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RunnerTemplate `json:"items"`
}

// ClusterRunnerTemplate is the cluster-scoped sibling of RunnerTemplate, with an
// identical spec. It lets the platform own golden privileged templates (DinD,
// sysbox) once cluster-wide; a RunnerSet selects it with templateRef.kind:
// ClusterRunnerTemplate (§H.4). Pure data: nothing owns it and it owns nothing.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=crt,categories=actions-gateway
// +kubebuilder:printcolumn:name="WorkerImage",type=string,JSONPath=`.spec.workerImage`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 52",message="metadata.name must be at most 52 characters: referenced by templateRef, which shares the v2 52-char name budget"
type ClusterRunnerTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunnerTemplateSpec   `json:"spec,omitempty"`
	Status RunnerTemplateStatus `json:"status,omitempty"`
}

// ClusterRunnerTemplateList contains a list of ClusterRunnerTemplate.
//
// +kubebuilder:object:root=true
type ClusterRunnerTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterRunnerTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RunnerTemplate{}, &RunnerTemplateList{})
	SchemeBuilder.Register(&ClusterRunnerTemplate{}, &ClusterRunnerTemplateList{})
}
