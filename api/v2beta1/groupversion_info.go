// Package v2beta1 contains the API Schema definitions for the
// actions-gateway.com v2beta1 API group: all five v2 kinds — the GMC-reconciled
// ActionsGateway (control) and EgressProxy (data) kinds, and the AGC-reconciled
// RunnerSet (control) and RunnerTemplate / ClusterRunnerTemplate (data) kinds —
// plus their shared types.
//
// v2beta1 is the graduation of the v2alpha1 API (Q74). It is the storage version
// and the conversion **hub**: the five kinds implement conversion.Hub here, while
// v2alpha1 implements conversion.Convertible (ConvertTo/ConvertFrom) as the spoke.
// v2beta1 is served beside v2alpha1 during the coexistence window so a tenant can
// roll forward at its own pace (and the gag-migrate Classic on-ramp keeps landing on
// v2alpha1); the two versions round-trip losslessly through the conversion webhook.
//
// The shape is identical to v2alpha1 with one deliberate exception: v2beta1 is
// ScaleSet-only, so RunnerSet drops the transitional acquisitionProtocol selector
// and the classic-only maxListeners knob (Q264 §5a-U7/U8). Those two v2alpha1-only
// fields survive a v2alpha1→v2beta1→v2alpha1 round-trip via an annotation carried on
// the v2beta1 object (see api/v2alpha1/conversion.go), so a coexistence-era Classic
// or ScaleSet set is never silently re-protocol'd.
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
// +kubebuilder:object:generate=true
// +groupName=actions-gateway.com
package v2beta1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "actions-gateway.com", Version: "v2beta1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
