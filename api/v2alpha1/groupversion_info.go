// Package v2alpha1 contains the API Schema definitions for the
// actions-gateway.com v2alpha1 API group: all five v2 kinds — the GMC-reconciled
// ActionsGateway (control) and EgressProxy (data) kinds, and the AGC-reconciled
// RunnerSet (control) and RunnerTemplate / ClusterRunnerTemplate (data) kinds —
// plus their shared types.
//
// These kinds live in a single neutral module
// (github.com/actions-gateway/github-actions-gateway/api) that both the GMC and
// AGC controller modules import. The AGC's RunnerSet reconciler must read the
// GMC-group ActionsGateway (gatewayRef) and EgressProxy (proxyRef), but the GMC
// module already imports the AGC module to build RunnerSet CRs; co-locating the v2
// kinds here breaks that would-be module dependency cycle without either
// controller module importing the other's API package (the neutral api/ module
// resolves the GMC↔AGC cycle; see docs/development/go-workspaces.md).
//
// v2 renames the API group from actions-gateway.github.com (v1alpha1) to
// actions-gateway.com — a domain the project owns
// (docs/design/appendix-h-v2-api-decomposition.md §H.15). v2alpha1 is served beside
// v1alpha1 during the coexistence window; the two groups are distinct, so the kinds
// round-trip independently. v2's ActionsGateway is decomposed: the inline proxy
// becomes a standalone EgressProxy and the bootstrap runner groups become explicit
// RunnerSet objects (§H.4).
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
