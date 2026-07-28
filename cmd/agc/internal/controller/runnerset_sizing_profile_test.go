package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

const testGPU = corev1.ResourceName("nvidia.com/gpu")

// profileTemplate returns a template spec with a runner container carrying a
// static ask plus one GPU, and an optional dind sidecar.
func profileTemplate(withSidecar bool) *v2alpha1.RunnerTemplateSpec {
	runner := corev1.Container{
		Name: "runner",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				testGPU:               resource.MustParse("1"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
				testGPU:               resource.MustParse("1"),
			},
		},
	}
	containers := []corev1.Container{runner}
	if withSidecar {
		containers = append(containers, corev1.Container{Name: "dind"})
	}
	return &v2alpha1.RunnerTemplateSpec{
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}},
	}
}

func confidentRec(container string) v2alpha1.ContainerSizingRecommendation {
	return v2alpha1.ContainerSizingRecommendation{
		Container: container,
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
		ObservedPeak: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("800m"),
			corev1.ResourceMemory: resource.MustParse("1536Mi"),
		},
		SampleCount: 25,
	}
}

func TestApplySizingProfileStaticIsUntouched(t *testing.T) {
	tmpl := profileTemplate(false)
	for _, sizing := range []*v2alpha1.WorkerSizing{nil, {Profile: v2alpha1.SizingProfileStatic}} {
		out := applySizingProfile(tmpl.PodTemplate, sizing, tmpl, []v2alpha1.ContainerSizingRecommendation{confidentRec("runner")})
		cpu := out.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
		if cpu.String() != "2" {
			t.Fatalf("static profile changed the template: cpu request %s", cpu.String())
		}
	}
}

func TestApplySizingProfileBinpack(t *testing.T) {
	tmpl := profileTemplate(false)
	out := applySizingProfile(tmpl.PodTemplate, &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileBinpack},
		tmpl, []v2alpha1.ContainerSizingRecommendation{confidentRec("runner")})

	res := out.Spec.Containers[0].Resources
	// Guaranteed QoS: requests == limits, from the recommendation (cpu p95,
	// memory recommended limit).
	for _, rl := range []corev1.ResourceList{res.Requests, res.Limits} {
		if q := rl[corev1.ResourceCPU]; q.String() != "500m" {
			t.Fatalf("binpack cpu = %s, want 500m", q.String())
		}
		if q := rl[corev1.ResourceMemory]; q.String() != "2Gi" {
			t.Fatalf("binpack memory = %s, want 2Gi", q.String())
		}
		// GPUs byte-identical to the template in every profile.
		if q := rl[testGPU]; q.String() != "1" {
			t.Fatalf("GPU modified: %s", q.String())
		}
	}
	// The input template must not have been mutated (informer-cached object).
	orig := tmpl.PodTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if orig.String() != "2" {
		t.Fatalf("input template mutated: cpu request %s", orig.String())
	}
}

func TestApplySizingProfileBinpackAllOrNothing(t *testing.T) {
	tmpl := profileTemplate(true) // runner + dind, but only runner has history
	sizing := &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileBinpack}
	recs := []v2alpha1.ContainerSizingRecommendation{confidentRec("runner")}

	if sizingProfileApplies(sizing, tmpl, recs) {
		t.Fatal("profile must not apply while a template container lacks a confident recommendation")
	}
	out := applySizingProfile(tmpl.PodTemplate, sizing, tmpl, recs)
	cpu := out.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpu.String() != "2" {
		t.Fatalf("partial history must fall back to the template, got cpu %s", cpu.String())
	}

	// Low-confidence recommendation on the second container: still not applied.
	low := confidentRec("dind")
	low.SampleCount = 5
	if sizingProfileApplies(sizing, tmpl, append(recs, low)) {
		t.Fatal("profile must not apply below the confidence minimum")
	}
	// Confident on both: applies.
	if !sizingProfileApplies(sizing, tmpl, []v2alpha1.ContainerSizingRecommendation{confidentRec("runner"), confidentRec("dind")}) {
		t.Fatal("profile should apply once every container is confident")
	}
}

func TestApplySizingProfileThroughput(t *testing.T) {
	tmpl := profileTemplate(false)
	out := applySizingProfile(tmpl.PodTemplate, &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileThroughput},
		tmpl, []v2alpha1.ContainerSizingRecommendation{confidentRec("runner")})

	res := out.Spec.Containers[0].Resources
	if q := res.Requests[corev1.ResourceCPU]; q.String() != "500m" {
		t.Fatalf("throughput cpu request = %s, want 500m", q.String())
	}
	if _, ok := res.Limits[corev1.ResourceCPU]; ok {
		t.Fatal("throughput must remove the CPU limit")
	}
	// Memory limit = observed peak (1536Mi) × default 150% = 2304Mi.
	if q := res.Limits[corev1.ResourceMemory]; q.String() != "2304Mi" {
		t.Fatalf("throughput memory limit = %s, want 2304Mi", q.String())
	}
	if q := res.Limits[testGPU]; q.String() != "1" {
		t.Fatalf("GPU modified: %s", q.String())
	}

	// Custom headroom.
	out = applySizingProfile(tmpl.PodTemplate,
		&v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileThroughput, LimitHeadroomPercent: ptr.To(int32(200))},
		tmpl, []v2alpha1.ContainerSizingRecommendation{confidentRec("runner")})
	if q := out.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; q.String() != "3Gi" {
		t.Fatalf("throughput 200%% memory limit = %s, want 3Gi", q.String())
	}
}

func TestApplySizingProfileNodeShare(t *testing.T) {
	tmpl := profileTemplate(true)
	sizing := &v2alpha1.WorkerSizing{
		Profile: v2alpha1.SizingProfileNodeShare,
		NodeShare: &v2alpha1.NodeShareSizing{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("15"),
				corev1.ResourceMemory: resource.MustParse("60Gi"),
			},
			WorkersPerNode: 4,
		},
	}
	// NodeShare needs no history.
	out := applySizingProfile(tmpl.PodTemplate, sizing, tmpl, nil)

	res := out.Spec.Containers[0].Resources
	if q := res.Requests[corev1.ResourceCPU]; q.String() != "3750m" {
		t.Fatalf("nodeshare cpu request = %s, want 3750m (15 / 4)", q.String())
	}
	if q := res.Requests[corev1.ResourceMemory]; q.String() != "15Gi" {
		t.Fatalf("nodeshare memory request = %s, want 15Gi (60Gi / 4)", q.String())
	}
	// Template memory limit (8Gi) below the derived request is lifted to it.
	if q := res.Limits[corev1.ResourceMemory]; q.String() != "15Gi" {
		t.Fatalf("nodeshare memory limit = %s, want lifted to 15Gi", q.String())
	}
	if q := res.Requests[testGPU]; q.String() != "1" {
		t.Fatalf("GPU modified: %s", q.String())
	}
	// The sidecar keeps its (empty) template ask.
	if len(out.Spec.Containers[1].Resources.Requests) != 0 {
		t.Fatalf("sidecar resources modified: %+v", out.Spec.Containers[1].Resources)
	}
}

// A Guaranteed node share is reachable today without a NodeShare-specific QoS
// knob: the limit-lift rule (a template limit below the derived request is
// raised to it, so the apiserver cannot reject the pod) lands requests ==
// limits on the runner container whenever the template's limits sit at or below
// the derived share. Q481 decided against adding that knob partly on this
// property, so it is pinned rather than left to the reader of applyNodeShare —
// the sibling TestApplySizingProfileNodeShare covers the ordinary case, where a
// CPU limit ABOVE the share is left alone and the result is Burstable.
func TestApplySizingProfileNodeShareLiftedLimitsReachGuaranteed(t *testing.T) {
	tmpl := profileTemplate(false)
	// Both template limits below the 15 / 4 = 3750m and 60Gi / 4 = 15Gi share.
	tmpl.PodTemplate.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1"),
		corev1.ResourceMemory: resource.MustParse("2Gi"),
		testGPU:               resource.MustParse("1"),
	}
	sizing := &v2alpha1.WorkerSizing{
		Profile: v2alpha1.SizingProfileNodeShare,
		NodeShare: &v2alpha1.NodeShareSizing{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("15"),
				corev1.ResourceMemory: resource.MustParse("60Gi"),
			},
			WorkersPerNode: 4,
		},
	}
	out := applySizingProfile(tmpl.PodTemplate, sizing, tmpl, nil)

	res := out.Spec.Containers[0].Resources
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		req, lim := res.Requests[name], res.Limits[name]
		if req.Cmp(lim) != 0 {
			t.Fatalf("%s request %s != limit %s, want the lift to reach Guaranteed", name, req.String(), lim.String())
		}
	}
	if q := res.Requests[corev1.ResourceCPU]; q.String() != "3750m" {
		t.Fatalf("cpu settled at %s, want the derived share 3750m (not the template limit)", q.String())
	}
	// The GPU is still byte-identical: the lift only ever touches cpu/memory.
	if q := res.Limits[testGPU]; q.String() != "1" {
		t.Fatalf("GPU limit modified: %s", q.String())
	}
}

func TestApplySizingProfileClamps(t *testing.T) {
	tmpl := profileTemplate(false)
	sizing := &v2alpha1.WorkerSizing{
		Profile: v2alpha1.SizingProfileBinpack,
		MinRequests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"),
		},
		MaxRequests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1536Mi"),
		},
	}
	out := applySizingProfile(tmpl.PodTemplate, sizing, tmpl, []v2alpha1.ContainerSizingRecommendation{confidentRec("runner")})

	res := out.Spec.Containers[0].Resources
	if q := res.Requests[corev1.ResourceCPU]; q.String() != "1" {
		t.Fatalf("cpu floor clamp = %s, want 1", q.String())
	}
	if q := res.Requests[corev1.ResourceMemory]; q.String() != "1536Mi" {
		t.Fatalf("memory ceiling clamp = %s, want 1536Mi", q.String())
	}
}

// TestApplySizingStatusProfileActiveSupersedesDrift verifies the reconciler
// reports the profile state and swaps the drift judgment for the
// SizingProfileActive reason while a profile actuates.
func TestApplySizingStatusProfileActiveSupersedesDrift(t *testing.T) {
	r := &RunnerSetReconciler{}
	rs := sizingTestSet()
	rs.Spec.Sizing = &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileBinpack}
	rs.Status.SizingRecommendation = []v2alpha1.ContainerSizingRecommendation{confidentRec("runner")}
	tmpl := profileTemplate(false) // oversized ask that would otherwise drift

	r.applySizingStatus(rs, tmpl)

	if rs.Status.SizingProfileState != v2alpha1.SizingProfileStateActive {
		t.Fatalf("SizingProfileState = %q, want Active", rs.Status.SizingProfileState)
	}
	cond := findSizingDrift(t, rs)
	if cond == nil || cond.Status != "False" || cond.Reason != v2alpha1.ReasonSizingProfileActive {
		t.Fatalf("SizingDrift = %+v, want False/SizingProfileActive", cond)
	}

	// Not enough history: state reports AwaitingSamples and drift is judged
	// normally against the template (which is oversized → True).
	rs2 := sizingTestSet()
	rs2.Spec.Sizing = &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileBinpack}
	low := confidentRec("runner")
	low.SampleCount = 25 // confident for drift…
	rs2.Status.SizingRecommendation = []v2alpha1.ContainerSizingRecommendation{low}
	tmplTwo := profileTemplate(true) // …but the dind container has no history
	r.applySizingStatus(rs2, tmplTwo)
	if rs2.Status.SizingProfileState != v2alpha1.SizingProfileStateAwaitingSamples {
		t.Fatalf("SizingProfileState = %q, want AwaitingSamples", rs2.Status.SizingProfileState)
	}
	cond = findSizingDrift(t, rs2)
	if cond == nil || cond.Reason == v2alpha1.ReasonSizingProfileActive {
		t.Fatalf("SizingDrift = %+v, want the normal drift judgment while awaiting samples", cond)
	}
}
