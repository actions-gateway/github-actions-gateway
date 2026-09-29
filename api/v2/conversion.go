package v2

// v2 is a Convertible **spoke** of the actions-gateway.com conversion graph: its five
// converted kinds convert to and from the v2beta1 **hub**, as v2alpha1's do.
//
// The hub stays at v2beta1 rather than moving to v2. controller-runtime routes a
// spoke-to-spoke request through the hub, so a v2 hub would send every v2alpha1 read
// of a v2beta1-stored object through a type that cannot hold the deprecated
// CiliumFQDN/CalicoFQDN aliases, and a stored alias would fail v2alpha1 reads that
// work today. With the v2beta1 hub, only a request that names v2 meets the alias.
//
// Every kind is an identity conversion, deep-copying ObjectMeta and round-tripping
// Spec/Status through JSON for the reasons api/v2alpha1/conversion.go gives. The one
// value v2 cannot represent is an alias in EgressProxy.spec.egressPolicyMode, and
// ConvertFrom refuses it rather than collapsing it to FQDN: the alias pins a backend
// that FQDN leaves to the operator, so the collapse would be lossy on the way back.

import (
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

// Compile-time proof that every converted v2 root kind is a conversion spoke.
var (
	_ conversion.Convertible = &ActionsGateway{}
	_ conversion.Convertible = &EgressProxy{}
	_ conversion.Convertible = &RunnerSet{}
	_ conversion.Convertible = &RunnerTemplate{}
	_ conversion.Convertible = &ClusterRunnerTemplate{}
)

// jsonRoundTrip copies src into dst by marshalling src to JSON and unmarshalling it
// into dst, which is lossless for two versions of one kind with identical shapes.
func jsonRoundTrip(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// convertSpecStatus round-trips a kind's Spec and Status between versions of
// identical shape. It is the whole body of an identity conversion.
func convertSpecStatus(srcSpec, srcStatus, dstSpec, dstStatus any) error {
	if err := jsonRoundTrip(srcSpec, dstSpec); err != nil {
		return fmt.Errorf("convert spec: %w", err)
	}
	if err := jsonRoundTrip(srcStatus, dstStatus); err != nil {
		return fmt.Errorf("convert status: %w", err)
	}
	return nil
}

// Conversion receivers are named r consistently across ConvertTo/ConvertFrom (a
// per-type staticcheck requirement, ST1016). In ConvertTo, r is the source spoke and
// the local dst is the hub; in ConvertFrom, r is the destination spoke and the local
// src is the hub.

// ConvertTo converts this v2 ActionsGateway to the v2beta1 hub.
func (r *ActionsGateway) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.ActionsGateway)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2 ActionsGateway from the v2beta1 hub.
func (r *ActionsGateway) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.ActionsGateway)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}

// ConvertTo converts this v2 EgressProxy to the v2beta1 hub. The v2 enum is a subset
// of the hub's, so every v2 value is representable there.
func (r *EgressProxy) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.EgressProxy)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2 EgressProxy from the v2beta1 hub. It refuses a hub
// object naming a deprecated alias, which v2 does not define.
func (r *EgressProxy) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.EgressProxy)
	switch mode := src.Spec.EgressPolicyMode; mode {
	case v2beta1.EgressPolicyModeCiliumFQDN, v2beta1.EgressPolicyModeCalicoFQDN:
		return fmt.Errorf("EgressProxy %s/%s: egressPolicyMode %q is a deprecated alias that "+
			"actions-gateway.com/v2 does not define; set egressPolicyMode: FQDN and choose the "+
			"backend with the GMC --fqdn-policy-backend flag, then read it at v2",
			src.Namespace, src.Name, mode)
	}
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}

// ConvertTo converts this v2 RunnerSet to the v2beta1 hub.
func (r *RunnerSet) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.RunnerSet)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2 RunnerSet from the v2beta1 hub.
func (r *RunnerSet) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.RunnerSet)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}

// ConvertTo converts this v2 RunnerTemplate to the v2beta1 hub.
func (r *RunnerTemplate) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.RunnerTemplate)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2 RunnerTemplate from the v2beta1 hub.
func (r *RunnerTemplate) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.RunnerTemplate)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}

// ConvertTo converts this v2 ClusterRunnerTemplate to the v2beta1 hub.
func (r *ClusterRunnerTemplate) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.ClusterRunnerTemplate)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2 ClusterRunnerTemplate from the v2beta1 hub.
func (r *ClusterRunnerTemplate) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.ClusterRunnerTemplate)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}
