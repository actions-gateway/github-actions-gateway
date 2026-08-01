package v2beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PriorityClassAllowlistSpec is the platform allowlist of PriorityClass names a
// tenant may reference.
type PriorityClassAllowlistSpec struct {
	// AllowedPriorityClasses is the set of cluster-scoped PriorityClass names
	// tenants may reference from RunnerSet.spec.priorityTiers[].priorityClassName
	// and RunnerTemplate.spec.podTemplate.spec.priorityClassName (and, while
	// v1alpha1 is served, RunnerGroup.spec.priorityTiers[].priorityClassName).
	//
	// A PriorityClass sets cluster-wide scheduling preemption order, so a tenant
	// naming a high-priority class could evict another tenant's worker pods. The
	// platform admin pre-creates each class (with preemptionPolicy: Never unless a
	// tier is genuinely meant to preempt across tenants) and lists its name here.
	//
	// Unset or empty forbids every named class — the secure default. This list is
	// additive to the GMC's static --allowed-priority-classes flag for the
	// admission webhook, and is the SOLE source for the
	// priorityclass-allowlist-guard ValidatingAdmissionPolicy, which cannot read a
	// controller flag. Keep it a superset of the flag, or direct writes naming a
	// flag-only class are denied by the policy.
	//
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	AllowedPriorityClasses []string `json:"allowedPriorityClasses,omitempty"`
}

// PriorityClassAllowlist is the cluster-scoped, platform-owned allowlist of
// PriorityClass names tenants may reference. It is pure configuration: nothing
// owns it and it owns nothing.
//
// It exists as a CRD rather than a ConfigMap because it is the `paramKind` of the
// priorityclass-allowlist-guard ValidatingAdmissionPolicy, and a kube-apiserver
// defect (Q444) permanently kills the param informer for any CORE-type paramKind
// once the set of bindings naming it goes empty for one refresh tick — which a
// `helm uninstall` does. The apiserver allocates a fresh dynamic informer per
// context for a CRD paramKind, so this kind is structurally immune; the contrast
// is measured by scripts/e2e/vap-param-informer-check.sh. Background:
// docs/design/05-security.md (PriorityClass allowlist).
//
// Whose fact is it (api-review.md § Ask whose fact it is): the platform's, in
// every sense — only whoever runs the nodes knows which classes exist and what
// preempting one costs, and a cap a tenant could raise is not a cap. Hence
// cluster-scoped, and writable only by a platform admin.
//
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:scope=Cluster,shortName=pca,categories=actions-gateway
// +kubebuilder:printcolumn:name="Allowed",type=string,JSONPath=`.spec.allowedPriorityClasses`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PriorityClassAllowlist struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PriorityClassAllowlistSpec `json:"spec,omitempty"`
}

// PriorityClassAllowlistList contains a list of PriorityClassAllowlist.
//
// +kubebuilder:object:root=true
type PriorityClassAllowlistList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PriorityClassAllowlist `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PriorityClassAllowlist{}, &PriorityClassAllowlistList{})
}
