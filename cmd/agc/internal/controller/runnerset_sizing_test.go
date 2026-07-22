package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// stubSizing is a fixed-map SizingSource.
type stubSizing struct {
	recs map[types.NamespacedName][]v2alpha1.ContainerSizingRecommendation
}

func (s *stubSizing) SizingStatus(key types.NamespacedName) []v2alpha1.ContainerSizingRecommendation {
	return s.recs[key]
}

func sizingRec(samples int64) v2alpha1.ContainerSizingRecommendation {
	return v2alpha1.ContainerSizingRecommendation{
		Container: "runner",
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("3584Mi"),
		},
		ObservedPeak: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2560Mi"),
		},
		ObservedP95: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("450m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		SampleCount: samples,
	}
}

func templateWithRunnerResources(res corev1.ResourceRequirements) *v2alpha1.RunnerTemplateSpec {
	return &v2alpha1.RunnerTemplateSpec{
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runner", Resources: res}},
			},
		},
	}
}

func sizingTestSet() *v2alpha1.RunnerSet {
	return &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: "rs1", Generation: 3}}
}

func findSizingDrift(t *testing.T, rs *v2alpha1.RunnerSet) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionSizingDrift)
}

// TestApplySizingStatusDriftDetected covers both drift directions at once: an
// oversized CPU/memory request (waste) and a memory limit below the observed
// per-job peak (OOM risk).
func TestApplySizingStatusDriftDetected(t *testing.T) {
	r := &RunnerSetReconciler{Sizing: &stubSizing{recs: map[types.NamespacedName][]v2alpha1.ContainerSizingRecommendation{
		{Namespace: "t1", Name: "rs1"}: {sizingRec(25)},
	}}}
	rs := sizingTestSet()
	template := templateWithRunnerResources(corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),   // 4x the recommended 500m
			corev1.ResourceMemory: resource.MustParse("4Gi"), // 4x the recommended 1Gi
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("2Gi"), // below the observed 2560Mi peak
		},
	})

	r.applySizingStatus(rs, template)

	if len(rs.Status.SizingRecommendation) != 1 {
		t.Fatalf("status.sizingRecommendation = %+v, want the stub's entry", rs.Status.SizingRecommendation)
	}
	cond := findSizingDrift(t, rs)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v2alpha1.ReasonSizingDriftDetected {
		t.Fatalf("SizingDrift = %+v, want True/SizingDriftDetected", cond)
	}
	if cond.ObservedGeneration != 3 {
		t.Fatalf("ObservedGeneration = %d, want 3", cond.ObservedGeneration)
	}
	for _, want := range []string{"cpu request 2", "memory request 4Gi", "OOM risk"} {
		if !strings.Contains(cond.Message, want) {
			t.Fatalf("message %q missing %q", cond.Message, want)
		}
	}
}

func TestApplySizingStatusWithinRange(t *testing.T) {
	r := &RunnerSetReconciler{Sizing: &stubSizing{recs: map[types.NamespacedName][]v2alpha1.ContainerSizingRecommendation{
		{Namespace: "t1", Name: "rs1"}: {sizingRec(25)},
	}}}
	rs := sizingTestSet()
	template := templateWithRunnerResources(corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("750m"),
			corev1.ResourceMemory: resource.MustParse("1536Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	})

	r.applySizingStatus(rs, template)

	cond := findSizingDrift(t, rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonSizingWithinRange {
		t.Fatalf("SizingDrift = %+v, want False/SizingWithinRange", cond)
	}
}

// TestApplySizingStatusGapFilledDefaults verifies a template that declares no
// resources at all is judged against the provisioner's gap-fill defaults
// (500m/1Gi requests==limits) — the classic unmeasured guess.
func TestApplySizingStatusGapFilledDefaults(t *testing.T) {
	rec := sizingRec(25)
	// Recommend far below the 500m/1Gi defaults so the waste check trips.
	rec.Requests = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	rec.ObservedPeak = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("150m"),
		corev1.ResourceMemory: resource.MustParse("2Gi"), // above the 1Gi default limit → OOM risk too
	}
	r := &RunnerSetReconciler{Sizing: &stubSizing{recs: map[types.NamespacedName][]v2alpha1.ContainerSizingRecommendation{
		{Namespace: "t1", Name: "rs1"}: {rec},
	}}}
	rs := sizingTestSet()

	r.applySizingStatus(rs, templateWithRunnerResources(corev1.ResourceRequirements{}))

	cond := findSizingDrift(t, rs)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("SizingDrift = %+v, want True against the gap-fill defaults", cond)
	}
}

func TestApplySizingStatusInsufficientSamples(t *testing.T) {
	r := &RunnerSetReconciler{Sizing: &stubSizing{recs: map[types.NamespacedName][]v2alpha1.ContainerSizingRecommendation{
		{Namespace: "t1", Name: "rs1"}: {sizingRec(7)}, // above the recommendation floor, below the drift floor
	}}}
	rs := sizingTestSet()

	r.applySizingStatus(rs, templateWithRunnerResources(corev1.ResourceRequirements{}))

	if len(rs.Status.SizingRecommendation) != 1 {
		t.Fatalf("low-confidence recommendation must still surface, got %+v", rs.Status.SizingRecommendation)
	}
	cond := findSizingDrift(t, rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonInsufficientSamples {
		t.Fatalf("SizingDrift = %+v, want False/InsufficientSamples", cond)
	}
}

// TestApplySizingStatusPersistedSurvivesEmptySnapshot verifies the store
// semantics: an empty snapshot (sampler warming up after a restart) must not
// wipe the persisted recommendation, and drift keeps being judged from it.
func TestApplySizingStatusPersistedSurvivesEmptySnapshot(t *testing.T) {
	r := &RunnerSetReconciler{Sizing: &stubSizing{recs: map[types.NamespacedName][]v2alpha1.ContainerSizingRecommendation{}}}
	rs := sizingTestSet()
	rs.Status.SizingRecommendation = []v2alpha1.ContainerSizingRecommendation{sizingRec(25)}
	template := templateWithRunnerResources(corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	})

	r.applySizingStatus(rs, template)

	if len(rs.Status.SizingRecommendation) != 1 {
		t.Fatalf("persisted recommendation wiped by empty snapshot: %+v", rs.Status.SizingRecommendation)
	}
	cond := findSizingDrift(t, rs)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("SizingDrift from persisted data = %+v, want True", cond)
	}
}

// TestApplySizingStatusNoDataNoCondition verifies a set with no sizing data and
// no source sets no condition — a metrics-server-less cluster stays silent.
func TestApplySizingStatusNoDataNoCondition(t *testing.T) {
	r := &RunnerSetReconciler{}
	rs := sizingTestSet()

	r.applySizingStatus(rs, templateWithRunnerResources(corev1.ResourceRequirements{}))

	if cond := findSizingDrift(t, rs); cond != nil {
		t.Fatalf("SizingDrift = %+v, want none with no data", cond)
	}
}
