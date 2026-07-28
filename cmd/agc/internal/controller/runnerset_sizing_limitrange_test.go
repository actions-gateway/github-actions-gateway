package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

func lrScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v2alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// containerLimitRange builds a LimitRange with one Container-type entry carrying
// the given default/max maps (either may be nil).
func containerLimitRange(name string, def, max corev1.ResourceList) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: name},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				Type:    corev1.LimitTypeContainer,
				Default: def,
				Max:     max,
			}},
		},
	}
}

// throughputSet is a RunnerSet opted into the Throughput profile.
func throughputSet() *v2alpha1.RunnerSet {
	rs := sizingTestSet()
	rs.Spec.Sizing = &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileThroughput}
	return rs
}

func lrReconciler(t *testing.T, objs ...client.Object) *RunnerSetReconciler {
	t.Helper()
	return &RunnerSetReconciler{
		Client: fake.NewClientBuilder().WithScheme(lrScheme(t)).WithObjects(objs...).Build(),
	}
}

func findOverride(rs *v2alpha1.RunnerSet) *metav1.Condition {
	return meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionSizingProfileOverridden)
}

// TestSizingProfileOverriddenCPUDefault: the silent cancellation Q489 exists for —
// a Container-type cpu default re-injects the limit Throughput removes.
func TestSizingProfileOverriddenCPUDefault(t *testing.T) {
	r := lrReconciler(t, containerLimitRange("tenant-defaults",
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, nil))
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs)

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v2alpha1.ReasonLimitRangeCPULimit {
		t.Fatalf("SizingProfileOverridden = %+v, want True/LimitRangeCPULimit", cond)
	}
	if cond.ObservedGeneration != 3 {
		t.Fatalf("ObservedGeneration = %d, want 3", cond.ObservedGeneration)
	}
	for _, want := range []string{`"tenant-defaults"`, "cpu default of 2", "Binpack"} {
		if !strings.Contains(cond.Message, want) {
			t.Fatalf("message %q missing %q", cond.Message, want)
		}
	}
}

// TestSizingProfileOverriddenCPUMax: a max with no default is a default —
// Kubernetes defaults the container limit to it.
func TestSizingProfileOverriddenCPUMax(t *testing.T) {
	r := lrReconciler(t, containerLimitRange("tenant-max",
		nil, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}))
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs)

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v2alpha1.ReasonLimitRangeCPULimit {
		t.Fatalf("SizingProfileOverridden = %+v, want True/LimitRangeCPULimit", cond)
	}
	if !strings.Contains(cond.Message, "max (which Kubernetes uses as the default limit) of 4") {
		t.Fatalf("message %q does not attribute the limit to the entry's max", cond.Message)
	}
}

// TestSizingProfileOverriddenIgnoresNonCPUAndNonContainer: a memory-only default,
// a Pod-type cpu entry, and a cpu defaultRequest all leave Throughput's CPU limit
// absent — none of them is the silent cancellation.
func TestSizingProfileOverriddenIgnoresNonCPUAndNonContainer(t *testing.T) {
	podType := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: "pod-scope"},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type: corev1.LimitTypePod,
			Max:  corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
		}}},
	}
	memOnly := containerLimitRange("mem-defaults",
		corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}, nil)
	reqOnly := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: "request-defaults"},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		}}},
	}
	r := lrReconciler(t, podType, memOnly, reqOnly)
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs)

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonNoLimitRangeOverride {
		t.Fatalf("SizingProfileOverridden = %+v, want False/NoLimitRangeOverride", cond)
	}
}

// TestSizingProfileOverriddenNoLimitRange: the ordinary namespace.
func TestSizingProfileOverriddenNoLimitRange(t *testing.T) {
	r := lrReconciler(t)
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs)

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonNoLimitRangeOverride {
		t.Fatalf("SizingProfileOverridden = %+v, want False/NoLimitRangeOverride", cond)
	}
}

// TestSizingProfileOverriddenOtherProfilesRemoveIt: the condition is Throughput-only,
// and switching away from Throughput drops a previously-True alarm rather than
// stranding it — Binpack sets its own CPU limit, so the LimitRange default never
// applies to it.
func TestSizingProfileOverriddenOtherProfilesRemoveIt(t *testing.T) {
	r := lrReconciler(t, containerLimitRange("tenant-defaults",
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, nil))

	for _, tc := range []struct {
		name   string
		sizing *v2alpha1.WorkerSizing
	}{
		{"binpack", &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileBinpack}},
		{"static", &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileStatic}},
		{"nodeshare", &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileNodeShare}},
		{"unset", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := throughputSet()
			r.applySizingProfileOverride(context.Background(), rs)
			if findOverride(rs) == nil {
				t.Fatal("precondition: Throughput should have set the condition")
			}

			rs.Spec.Sizing = tc.sizing
			r.applySizingProfileOverride(context.Background(), rs)

			if cond := findOverride(rs); cond != nil {
				t.Fatalf("SizingProfileOverridden = %+v, want the condition removed", cond)
			}
		})
	}
}

// TestSizingProfileOverriddenListErrorFailsOpen: an install not yet granted the
// limitranges read reports LimitRangesUnreadable, not a conflict it never saw.
func TestSizingProfileOverriddenListErrorFailsOpen(t *testing.T) {
	r := &RunnerSetReconciler{
		Client: fake.NewClientBuilder().WithScheme(lrScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*corev1.LimitRangeList); ok {
					return errors.New("limitranges is forbidden")
				}
				return nil
			},
		}).Build(),
	}
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs)

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonLimitRangesUnreadable {
		t.Fatalf("SizingProfileOverridden = %+v, want False/LimitRangesUnreadable", cond)
	}
	if !strings.Contains(cond.Message, "forbidden") {
		t.Fatalf("message %q does not carry the underlying error", cond.Message)
	}
}

// TestSizingProfileOverriddenReadsOnlyItsOwnNamespace: another tenant's LimitRange
// is not this set's problem.
func TestSizingProfileOverriddenReadsOnlyItsOwnNamespace(t *testing.T) {
	other := containerLimitRange("tenant-defaults",
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, nil)
	other.Namespace = "t2"
	r := lrReconciler(t, other)
	rs := throughputSet() // namespace t1

	r.applySizingProfileOverride(context.Background(), rs)

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonNoLimitRangeOverride {
		t.Fatalf("SizingProfileOverridden = %+v, want False/NoLimitRangeOverride", cond)
	}
}
