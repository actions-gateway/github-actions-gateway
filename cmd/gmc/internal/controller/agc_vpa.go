package controller

import (
	"context"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Managed vertical right-sizing of the AGC control plane (Q360). A gateway that sets
// spec.agcAutoscaling gets a VerticalPodAutoscaler stamped next to its AGC Deployment,
// so the autoscaler observes the pod's real CPU/memory usage and sizes its *requests*
// instead of the operator guessing them. The opt-in is off by default and the block's
// presence is the switch: removing it deletes the managed object again.
//
// The autoscaling.k8s.io kinds are addressed as unstructured objects, exactly like the
// CNI-native FQDN policies and the tenant ServiceMonitors, so the GMC never takes a
// compile-time dependency on the autoscaler's API module. Because those CRDs are an
// optional add-on rather than core Kubernetes, every path here is guarded by
// vpaCRDInstalled and every residual NoMatch is treated as "not installed" — the opt-in
// degrades to a condition + Event, never to a failed reconcile.

// verticalPodAutoscalerGVK identifies the managed autoscaler kind. autoscaling.k8s.io/v1
// is the GA version shipped by every supported vertical-pod-autoscaler release.
var verticalPodAutoscalerGVK = schema.GroupVersionKind{Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler"}

// agcAutoscalerReprobeInterval is how often a gateway whose agcAutoscaling opt-in is
// unsatisfiable re-probes for the VerticalPodAutoscaler CRD. Ten minutes is a poll, not
// a hot loop: it costs one cached-discovery RESTMapping per gateway per interval, and it
// means an operator who installs the vertical-pod-autoscaler sees the gateway pick it up
// without touching the CR.
const agcAutoscalerReprobeInterval = 10 * time.Minute

// agcVPAControlledResources is the resource set the managed autoscaler is allowed to
// size. CPU and memory only — the AGC requests no extended resources, and leaving the
// set implicit would let a future VPA release widen it silently.
var agcVPAControlledResources = []interface{}{string(corev1.ResourceCPU), string(corev1.ResourceMemory)}

// agcVPAName is the managed VerticalPodAutoscaler's name: the same per-gateway name as
// the AGC Deployment it targets, so it is unique per gateway in a multi-gateway
// namespace and reads unambiguously in `kubectl get vpa`.
func agcVPAName(ag *gmcv2alpha1.ActionsGateway) string { return agcNameV2(ag) }

// agcVPAUpdateMode returns the effective updateMode for the managed autoscaler,
// treating the empty string as the Off default so a hand-built object that skipped
// apiserver defaulting behaves like a defaulted one.
func agcVPAUpdateMode(a *gmcv2alpha1.AGCVerticalAutoscaling) gmcv2alpha1.VPAUpdateMode {
	if a == nil || a.Mode == "" {
		return gmcv2alpha1.VPAUpdateModeOff
	}
	return a.Mode
}

// agcVPABounds derives the managed autoscaler's container-policy bounds from
// spec.agcResources — the precedence contract between the two fields (Q360):
//
//   - minAllowed comes from the requests the tenant EXPLICITLY set (the raw override,
//     not the defaulted merge). An explicit request is an operator's deliberate floor,
//     so it is honored as the floor the autoscaler may not size below rather than
//     silently overwritten. A gateway that sets no requests gets no minAllowed, which
//     is the configuration where right-sizing has the most to win — the autoscaler is
//     free to shrink the AGC down to its own global floor.
//   - maxAllowed comes from the EFFECTIVE limits (default overlaid with the override),
//     which is a correctness requirement rather than a preference: the limits are what
//     the GMC stamps on the container, and a container whose request exceeds its own
//     limit is rejected by the apiserver, so the autoscaler must never recommend above
//     them. This is also exactly the pairing upstream requires — controlledValues:
//     RequestsOnly on a container that HAS limits is only safe when maxAllowed is at
//     most those limits, which is why the ceiling is always emitted, never optional.
//
// Only cpu and memory are carried across, matching agcVPAControlledResources.
func agcVPABounds(override *corev1.ResourceRequirements) (minAllowed, maxAllowed map[string]interface{}) {
	pick := func(list corev1.ResourceList) map[string]interface{} {
		out := map[string]interface{}{}
		for _, k := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if q, ok := list[k]; ok {
				// Quantities must be serialized as their canonical string form: an
				// unstructured object only carries JSON-native scalars.
				out[string(k)] = q.String()
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	if override != nil {
		minAllowed = pick(override.Requests)
	}
	return minAllowed, pick(agcResources(override).Limits)
}

// buildAGCVerticalPodAutoscaler builds the managed VerticalPodAutoscaler for a gateway
// that opted into spec.agcAutoscaling. It targets the AGC Deployment by name and
// constrains the autoscaler to the AGC container.
//
// controlledValues is pinned to RequestsOnly, which is what keeps agcResources and the
// autoscaler composable instead of competing: the autoscaler moves requests, and the
// limits the operator chose (or the platform default) stay exactly as stamped. On a
// single-pod control plane an autoscaler that also drifted the memory *limit* would
// erode the OOM ceiling that bounds a runaway — the property the limit exists for.
func buildAGCVerticalPodAutoscaler(ag *gmcv2alpha1.ActionsGateway) *unstructured.Unstructured {
	minAllowed, maxAllowed := agcVPABounds(ag.Spec.AGCResources)

	containerPolicy := map[string]interface{}{
		"containerName":       agcContainerName,
		"controlledValues":    "RequestsOnly",
		"controlledResources": agcVPAControlledResources,
	}
	if minAllowed != nil {
		containerPolicy["minAllowed"] = minAllowed
	}
	if maxAllowed != nil {
		containerPolicy["maxAllowed"] = maxAllowed
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": verticalPodAutoscalerGVK.GroupVersion().String(),
		"kind":       verticalPodAutoscalerGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      agcVPAName(ag),
			"namespace": ag.Namespace,
			"labels":    toUnstructuredLabels(v2GatewayLabels(ag)),
		},
		"spec": map[string]interface{}{
			"targetRef": map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       agcNameV2(ag),
			},
			"updatePolicy": map[string]interface{}{
				"updateMode": string(agcVPAUpdateMode(ag.Spec.AGCAutoscaling)),
			},
			"resourcePolicy": map[string]interface{}{
				"containerPolicies": []interface{}{containerPolicy},
			},
		},
	}}
}

// formatAGCVPABounds renders the derived bounds for the Event that announces the
// managed autoscaler, so the precedence resolution is visible in `kubectl describe`
// rather than only inside the stamped object.
func formatAGCVPABounds(bounds map[string]interface{}, unset string) string {
	if bounds == nil {
		return unset
	}
	// Fixed key order so the message is stable across reconciles (map iteration is not).
	var out string
	for _, k := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		v, ok := bounds[string(k)].(string)
		if !ok {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += string(k) + "=" + v
	}
	return out
}

// agcAutoscalerState is the outcome of one autoscaler reconcile pass, threaded to
// updateStatus so it can set the advisory AGCAutoscalingUnavailable condition.
type agcAutoscalerState struct {
	// requested is true when the gateway opted into spec.agcAutoscaling.
	requested bool
	// unavailable is true when the opt-in could not be satisfied because the cluster
	// has no autoscaling.k8s.io VerticalPodAutoscaler CRD.
	unavailable bool
}

// vpaCRDInstalled reports whether this cluster serves the autoscaling.k8s.io
// VerticalPodAutoscaler kind, by asking the client's RESTMapper for a mapping. This is
// the graceful-degradation probe (Q360): it is answered from cached discovery (no API
// round-trip in the steady state), and the manager's dynamic RESTMapper reloads
// discovery on a miss, so a CRD installed after the GMC started is discovered rather
// than cached-negative forever.
//
// A NoMatch is the "not installed" answer, not an error. Any other error is a genuine
// discovery failure and is returned so the reconcile retries with backoff rather than
// silently reporting the opt-in as unsatisfiable.
func (r *ActionsGatewayV2Reconciler) vpaCRDInstalled() (bool, error) {
	if _, err := r.RESTMapper().RESTMapping(verticalPodAutoscalerGVK.GroupKind(), verticalPodAutoscalerGVK.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// applyOrPruneAGCAutoscaler reconciles the managed VerticalPodAutoscaler for a gateway
// (Q360), mirroring applyOrPruneServiceMonitors:
//
//   - opted in: create/patch it, unless vpaCRDInstalled reports the autoscaling.k8s.io
//     CRD absent — then the opt-in is reported as unavailable rather than attempted. A
//     missing optional right-sizing prerequisite must not wedge a gateway whose AGC is
//     otherwise perfectly provisionable. The caller surfaces it as the advisory
//     AGCAutoscalingUnavailable condition plus a Warning Event and re-probes on a slow
//     bounded requeue, so installing the VPA controllers later converges without an
//     operator edit and without a hot loop. A NoMatch from the apply itself (the CRD
//     was uninstalled between the probe and the write) lands in the same state.
//   - not opted in (the default): best-effort delete, so removing the block — or never
//     adding it — leaves no stale autoscaler still moving the AGC's requests. A NoMatch
//     (CRD absent) is success: there is nothing to prune.
func (r *ActionsGatewayV2Reconciler) applyOrPruneAGCAutoscaler(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) (agcAutoscalerState, error) {
	installed, err := r.vpaCRDInstalled()
	if err != nil {
		return agcAutoscalerState{requested: ag.Spec.AGCAutoscaling != nil}, err
	}

	if ag.Spec.AGCAutoscaling == nil {
		if !installed {
			// No CRD ⇒ nothing was ever stamped ⇒ nothing to prune.
			return agcAutoscalerState{}, nil
		}
		return agcAutoscalerState{}, r.deleteAGCAutoscaler(ctx, ag)
	}

	if !installed {
		return agcAutoscalerState{requested: true, unavailable: true}, nil
	}

	desired := buildAGCVerticalPodAutoscaler(ag)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(verticalPodAutoscalerGVK)
	obj.SetNamespace(desired.GetNamespace())
	obj.SetName(desired.GetName())
	op, err := controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		obj.SetLabels(desired.GetLabels())
		spec, _, _ := unstructured.NestedMap(desired.Object, "spec")
		if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(ag, obj, r.Scheme)
	})
	if err != nil {
		if meta.IsNoMatchError(err) {
			return agcAutoscalerState{requested: true, unavailable: true}, nil
		}
		return agcAutoscalerState{requested: true}, err
	}
	if op == controllerutil.OperationResultCreated {
		// Announce the precedence resolution once, when the autoscaler first appears:
		// which knob the autoscaler now owns, and the bounds derived from agcResources.
		minAllowed, maxAllowed := agcVPABounds(ag.Spec.AGCResources)
		r.recordEvent(ag, corev1.EventTypeNormal, gmcv2alpha1.ReasonAGCAutoscalingActive, "ApplyAGCAutoscaler",
			"managed VerticalPodAutoscaler %q provisioned in updateMode %s; it sizes the AGC container's requests only (limits stay as stamped from agcResources), bounded by minAllowed [%s] and maxAllowed [%s]",
			agcVPAName(ag), agcVPAUpdateMode(ag.Spec.AGCAutoscaling),
			formatAGCVPABounds(minAllowed, "unset — no explicit agcResources.requests floor"),
			formatAGCVPABounds(maxAllowed, "unset"))
	}
	return agcAutoscalerState{requested: true}, nil
}

// deleteAGCAutoscaler removes the managed VerticalPodAutoscaler when a gateway drops its
// spec.agcAutoscaling opt-in. Callers reach it only after vpaCRDInstalled said yes; a
// NoMatch from the delete itself (the CRD was uninstalled in between) is still tolerated,
// since "the kind does not exist" and "the object does not exist" are the same desired
// end state. Teardown deletes the object through reconcileDelete's verifying `del` helper
// instead, so a lingering child holds the finalizer like every other child.
func (r *ActionsGatewayV2Reconciler) deleteAGCAutoscaler(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) error {
	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(verticalPodAutoscalerGVK)
	vpa.SetNamespace(ag.Namespace)
	vpa.SetName(agcVPAName(ag))
	if err := r.Delete(ctx, vpa); client.IgnoreNotFound(err) != nil && !meta.IsNoMatchError(err) {
		return err
	}
	return nil
}
