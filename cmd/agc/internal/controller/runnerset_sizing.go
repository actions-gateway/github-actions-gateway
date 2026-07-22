package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/usage"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// SizingSource supplies the status-shaped worker sizing recommendation for a
// RunnerSet (Q359 Phase 2). Implemented by *usage.Sampler; a nil interface (or
// a typed-nil sampler, which returns nil) leaves any persisted recommendation
// untouched and the drift judgment running against it.
type SizingSource interface {
	SizingStatus(key types.NamespacedName) []v2alpha1.ContainerSizingRecommendation
}

// sizingDriftWasteFactor is the over-provisioning threshold for the SizingDrift
// condition: a template request at or above this multiple of the measured
// recommendation is flagged as waste. Coarse by design — the condition should
// fire on "your guess is off by binary orders", not on tuning noise.
const sizingDriftWasteFactor = 2.0

// applySizingStatus refreshes status.sizingRecommendation from the sizing
// source and upserts the advisory SizingDrift condition against the resolved
// template's per-container resource ask (Q359 Phase 2).
//
// The status field is only ever overwritten with fresh data — an empty snapshot
// (sampler still warming up after a restart, or disabled) leaves the persisted
// recommendation in place, because that field IS the aggregate store the
// sampler re-seeds from. The drift judgment then runs against whatever the
// status holds, so a confident persisted recommendation keeps judging drift
// across restarts.
func (r *RunnerSetReconciler) applySizingStatus(rs *v2alpha1.RunnerSet, template *v2alpha1.RunnerTemplateSpec) {
	if r.Sizing != nil {
		if recs := r.Sizing.SizingStatus(types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}); len(recs) > 0 {
			rs.Status.SizingRecommendation = recs
		}
	}
	recs := rs.Status.SizingRecommendation

	// Report whether the opt-in sizing profile is actuating (Q359 Phase 3),
	// using the same predicate the pod-build transform runs — the status can
	// never disagree with what Resolve actually does.
	rs.Status.SizingProfileState = ""
	profileSelected := rs.Spec.Sizing != nil && rs.Spec.Sizing.Profile != "" &&
		rs.Spec.Sizing.Profile != v2alpha1.SizingProfileStatic
	if profileSelected && template != nil {
		if sizingProfileApplies(rs.Spec.Sizing, template, recs) {
			rs.Status.SizingProfileState = v2alpha1.SizingProfileStateActive
		} else {
			rs.Status.SizingProfileState = v2alpha1.SizingProfileStateAwaitingSamples
		}
	}
	// An actively-applied profile supersedes the drift judgment: pods run the
	// derived values, so comparing the template ask against the recommendation
	// would mislead.
	if rs.Status.SizingProfileState == v2alpha1.SizingProfileStateActive {
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
			Type:               v2alpha1.ConditionSizingDrift,
			Status:             metav1.ConditionFalse,
			Reason:             v2alpha1.ReasonSizingProfileActive,
			Message:            fmt.Sprintf("the %s sizing profile applies the measured recommendation at pod build; the template's static ask is not what worker pods run with", rs.Spec.Sizing.Profile),
			ObservedGeneration: rs.Generation,
		})
		return
	}

	// No data and no template: nothing to say — set no condition rather than a
	// noisy InsufficientSamples on every set in a cluster without metrics-server.
	if len(recs) == 0 || template == nil {
		return
	}

	var confident bool
	var findings []string
	for _, rec := range recs {
		if rec.SampleCount < usage.MinSamplesForDrift {
			continue
		}
		container := templateContainer(template, rec.Container)
		if container == nil {
			// Not part of the template (e.g. an injected sidecar) — measured
			// data exists but there is no ask to compare.
			continue
		}
		confident = true
		findings = append(findings, sizingDriftFindings(rec, effectiveAsk(container.Resources))...)
	}

	status := metav1.ConditionFalse
	reason := v2alpha1.ReasonSizingWithinRange
	msg := "template container resources are within the drift thresholds of the measured recommendation"
	switch {
	case !confident:
		reason = v2alpha1.ReasonInsufficientSamples
		msg = fmt.Sprintf("fewer than %d sampled jobs for every template container; not judging drift yet (see status.sizingRecommendation sampleCount)", usage.MinSamplesForDrift)
	case len(findings) > 0:
		status = metav1.ConditionTrue
		reason = v2alpha1.ReasonSizingDriftDetected
		msg = strings.Join(findings, "; ")
	}
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionSizingDrift,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rs.Generation,
	})
}

// templateContainer returns the named regular container of the template's pod
// spec, or nil.
func templateContainer(template *v2alpha1.RunnerTemplateSpec, name string) *corev1.Container {
	for i := range template.PodTemplate.Spec.Containers {
		if template.PodTemplate.Spec.Containers[i].Name == name {
			return &template.PodTemplate.Spec.Containers[i]
		}
	}
	return nil
}

// effectiveAsk is the resource ask a worker container actually runs with: the
// template's declaration, or — when it declares neither requests nor limits —
// the provisioner's gap-fill defaults (which is exactly the unmeasured guess
// this feature exists to revisit).
func effectiveAsk(res corev1.ResourceRequirements) corev1.ResourceRequirements {
	if len(res.Requests) > 0 || len(res.Limits) > 0 {
		return res
	}
	defaults := provisioner.DefaultWorkerResources()
	return corev1.ResourceRequirements{Requests: defaults, Limits: defaults}
}

// sizingDriftFindings compares one container's effective ask against its
// measured recommendation and returns a human-readable finding per material
// mismatch: waste (a request ≥ sizingDriftWasteFactor × the recommendation) or
// OOM risk (a memory limit below the highest observed per-job peak).
func sizingDriftFindings(rec v2alpha1.ContainerSizingRecommendation, ask corev1.ResourceRequirements) []string {
	var findings []string
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		askQ, ok := ask.Requests[res]
		if !ok {
			continue
		}
		recQ, ok := rec.Requests[res]
		if !ok || recQ.IsZero() {
			continue
		}
		if askQ.AsApproximateFloat64() >= sizingDriftWasteFactor*recQ.AsApproximateFloat64() {
			findings = append(findings, fmt.Sprintf(
				"container %s: %s request %s is >=%.0fx the recommended %s (waste)",
				rec.Container, res, askQ.String(), sizingDriftWasteFactor, recQ.String()))
		}
	}
	if limitQ, ok := ask.Limits[corev1.ResourceMemory]; ok {
		if peakQ, okPeak := rec.ObservedPeak[corev1.ResourceMemory]; okPeak &&
			limitQ.AsApproximateFloat64() < peakQ.AsApproximateFloat64() {
			findings = append(findings, fmt.Sprintf(
				"container %s: memory limit %s is below the observed per-job peak %s (OOM risk)",
				rec.Container, limitQ.String(), peakQ.String()))
		}
	}
	return findings
}
