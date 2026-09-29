// Package v2 contains the API Schema definitions for the actions-gateway.com v2
// API group, the General Availability (GA) version: the five converted v2 kinds —
// the GMC-reconciled ActionsGateway (control) and EgressProxy (data) kinds, and the
// AGC-reconciled RunnerSet (control) and RunnerTemplate / ClusterRunnerTemplate
// (data) kinds — plus the platform-owned PriorityClassAllowlist and their shared
// types.
//
// v2 is served beside v2beta1 and is NOT the storage version: v2beta1 keeps that
// role until v2.0.0, because Kubernetes' deprecation policy (rule #4b) will not let
// the storage version advance in the release that introduces its successor (Q413).
//
// v2 is a conversion **spoke** of the v2beta1 hub (see conversion.go), like
// v2alpha1. The shape is identical to v2beta1 with one exception: EgressProxy's
// egressPolicyMode enum is CIDR;FQDN, without the deprecated CiliumFQDN/CalicoFQDN
// aliases (Q452); a stored alias reads at v2 as FQDN with the alias carried in a
// conversion annotation (see conversion.go).
// PriorityClassAllowlist has no conversion webhook: its schema is identical at
// every version, so the apiserver converts it by rewriting apiVersion alone.
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
