// Package v2 contains the API Schema definitions for the actions-gateway.com v2
// API group, the General Availability (GA) version and the only one served: the
// GMC-reconciled ActionsGateway (control) and EgressProxy (data) kinds, the
// AGC-reconciled RunnerSet (control) and RunnerTemplate / ClusterRunnerTemplate (data)
// kinds, the platform-owned PriorityClassAllowlist, and their shared types.
//
// +kubebuilder:object:generate=true
// +groupName=actions-gateway.com
package v2

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "actions-gateway.com", Version: "v2"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
