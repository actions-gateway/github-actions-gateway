// Package v2alpha1 contains the AGC's API Schema definitions for the
// actions-gateway.com v2alpha1 API group: the RunnerSet control kind and the
// RunnerTemplate / ClusterRunnerTemplate data kinds.
//
// v2 renames the API group from actions-gateway.github.com (v1alpha1) to
// actions-gateway.com — a domain the project owns (docs/design/appendix-h-v2-api-decomposition.md
// §H.15). v2alpha1 is served beside v1alpha1 during the coexistence window; the
// two groups are distinct, so the kinds round-trip independently.
//
// +kubebuilder:object:generate=true
// +groupName=actions-gateway.com
package v2alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "actions-gateway.com", Version: "v2alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
