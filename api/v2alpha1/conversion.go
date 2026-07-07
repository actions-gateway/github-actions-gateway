/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v2alpha1

// v2alpha1 is the Convertible **spoke** of the actions-gateway.com conversion graph
// (Q74): its five kinds convert to and from the v2beta1 **hub** (the storage
// version). Hub-and-spoke keeps the conversion count linear — every served version
// converts to/from the single hub, never pairwise.
//
// Four of the five kinds are a pure **identity** conversion: v2beta1 has the same
// shape, so ObjectMeta is deep-copied and Spec/Status round-trip through JSON. The
// JSON round-trip is deliberate over hand-written field assignment: it is lossless
// for identical shapes, self-maintaining as fields are added, and far less
// error-prone than threading assignments through the embedded PodTemplateSpec.
//
// RunnerSet is the one non-identity case. v2beta1 is ScaleSet-only, so it drops the
// v2alpha1-only acquisitionProtocol selector and the classic-only maxListeners knob
// (Q264 §5a-U7/U8). To keep the round-trip lossless, ConvertTo stashes both values in
// annotations on the hub object and ConvertFrom restores them — so a coexistence-era
// v2alpha1 set (Classic *or* ScaleSet) is never silently re-protocol'd. A
// v2beta1-native set carries no such annotation, so it restores to the ScaleSet
// defaults.

import (
	"encoding/json"
	"fmt"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

const (
	// annAcquisitionProtocol carries a v2alpha1 RunnerSet's acquisitionProtocol
	// across a conversion to the ScaleSet-only v2beta1 hub, which has no field for
	// it. The conversion.* prefix marks it a conversion-machinery annotation, not an
	// operator-facing one; ConvertFrom strips it from the restored v2alpha1 view.
	annAcquisitionProtocol = "conversion.actions-gateway.com/acquisition-protocol"
	// annMaxListeners carries a v2alpha1 RunnerSet's maxListeners (decimal string)
	// across the same conversion. maxListeners is meaningless under ScaleSet (one
	// session per set), which is why v2beta1 drops it, but it is preserved here so a
	// v2alpha1 object round-trips byte-for-byte.
	annMaxListeners = "conversion.actions-gateway.com/max-listeners"

	// defaultMaxListeners mirrors the v2alpha1 RunnerSetSpec.MaxListeners
	// +kubebuilder:default. A v2beta1-native RunnerSet (no annMaxListeners) restores
	// to this value so the surfaced v2alpha1 view satisfies the field's Minimum=1.
	defaultMaxListeners int32 = 10
)

// Compile-time proof that every v2alpha1 root kind is a conversion spoke.
var (
	_ conversion.Convertible = &ActionsGateway{}
	_ conversion.Convertible = &EgressProxy{}
	_ conversion.Convertible = &RunnerSet{}
	_ conversion.Convertible = &RunnerTemplate{}
	_ conversion.Convertible = &ClusterRunnerTemplate{}
)

// jsonRoundTrip copies src into dst by marshalling src to JSON and unmarshalling it
// into dst. For two versions of one kind with identical field shapes it is lossless;
// fields present in src but absent in dst (the acquisitionProtocol / maxListeners
// strip) are dropped, which is exactly the behavior the RunnerSet annotation
// round-trip then compensates for.
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

// --- ActionsGateway (identity) ---

// ConvertTo converts this v2alpha1 ActionsGateway to the v2beta1 hub.
func (r *ActionsGateway) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.ActionsGateway)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2alpha1 ActionsGateway from the v2beta1 hub.
func (r *ActionsGateway) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.ActionsGateway)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}

// --- EgressProxy (identity) ---

// ConvertTo converts this v2alpha1 EgressProxy to the v2beta1 hub.
func (r *EgressProxy) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.EgressProxy)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2alpha1 EgressProxy from the v2beta1 hub.
func (r *EgressProxy) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.EgressProxy)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}

// --- RunnerTemplate (identity) ---

// ConvertTo converts this v2alpha1 RunnerTemplate to the v2beta1 hub.
func (r *RunnerTemplate) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.RunnerTemplate)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2alpha1 RunnerTemplate from the v2beta1 hub.
func (r *RunnerTemplate) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.RunnerTemplate)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}

// --- ClusterRunnerTemplate (identity) ---

// ConvertTo converts this v2alpha1 ClusterRunnerTemplate to the v2beta1 hub.
func (r *ClusterRunnerTemplate) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.ClusterRunnerTemplate)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	return convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status)
}

// ConvertFrom populates this v2alpha1 ClusterRunnerTemplate from the v2beta1 hub.
func (r *ClusterRunnerTemplate) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.ClusterRunnerTemplate)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	return convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status)
}

// --- RunnerSet (identity + protocol-field annotation round-trip) ---

// ConvertTo converts this v2alpha1 RunnerSet to the v2beta1 hub. The hub is
// ScaleSet-only and has no acquisitionProtocol / maxListeners fields, so those two
// values are stashed in conversion annotations on the hub object to keep the
// round-trip lossless (Q264 §5a-U7).
func (r *RunnerSet) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v2beta1.RunnerSet)
	r.ObjectMeta.DeepCopyInto(&dst.ObjectMeta)
	if err := convertSpecStatus(&r.Spec, &r.Status, &dst.Spec, &dst.Status); err != nil {
		return err
	}
	if dst.Annotations == nil {
		dst.Annotations = map[string]string{}
	}
	dst.Annotations[annAcquisitionProtocol] = r.Spec.AcquisitionProtocol
	dst.Annotations[annMaxListeners] = strconv.Itoa(int(r.Spec.MaxListeners))
	return nil
}

// ConvertFrom populates this v2alpha1 RunnerSet from the v2beta1 hub, restoring the
// acquisitionProtocol and maxListeners fields from the conversion annotations. A
// v2beta1-native set carries no such annotation, so it restores to the ScaleSet
// defaults (acquisitionProtocol=ScaleSet, maxListeners=10). The conversion
// annotations are stripped from the surfaced v2alpha1 view.
func (r *RunnerSet) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v2beta1.RunnerSet)
	src.ObjectMeta.DeepCopyInto(&r.ObjectMeta)
	if err := convertSpecStatus(&src.Spec, &src.Status, &r.Spec, &r.Status); err != nil {
		return err
	}
	if v, ok := r.Annotations[annAcquisitionProtocol]; ok {
		r.Spec.AcquisitionProtocol = v
		delete(r.Annotations, annAcquisitionProtocol)
	} else {
		r.Spec.AcquisitionProtocol = AcquisitionProtocolScaleSet
	}
	if v, ok := r.Annotations[annMaxListeners]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("restore maxListeners from annotation %q=%q: %w", annMaxListeners, v, err)
		}
		r.Spec.MaxListeners = int32(n)
		delete(r.Annotations, annMaxListeners)
	} else {
		r.Spec.MaxListeners = defaultMaxListeners
	}
	if len(r.Annotations) == 0 {
		r.Annotations = nil
	}
	return nil
}
