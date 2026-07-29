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

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

func overrideScheme(t *testing.T) *runtime.Scheme {
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

// sizedWorkerPod builds a worker pod for set rs1 in t1. profile is the value of the
// sizing-profile annotation ("" stamps none); limits are applied to the runner
// container.
func sizedWorkerPod(name, profile string, limits corev1.ResourceList, extra ...corev1.Container) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "t1",
			Name:      name,
			Labels:    map[string]string{provisioner.LabelRunnerSet: "rs1"},
		},
		Spec: corev1.PodSpec{
			Containers: append([]corev1.Container{{
				Name:      "runner",
				Resources: corev1.ResourceRequirements{Limits: limits},
			}}, extra...),
		},
	}
	if profile != "" {
		p.Annotations = map[string]string{provisioner.AnnotationSizingProfile: profile}
	}
	return p
}

func throughputSet() *v2alpha1.RunnerSet {
	rs := sizingTestSet()
	rs.Spec.Sizing = &v2alpha1.WorkerSizing{Profile: v2alpha1.SizingProfileThroughput}
	return rs
}

func overrideReconciler(t *testing.T, objs ...client.Object) *RunnerSetReconciler {
	t.Helper()
	return &RunnerSetReconciler{
		Client: fake.NewClientBuilder().WithScheme(overrideScheme(t)).WithObjects(objs...).Build(),
	}
}

func findOverride(rs *v2alpha1.RunnerSet) *metav1.Condition {
	return meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionSizingProfileOverridden)
}

// runnerTemplate is the one-container template the pods above are built from.
func runnerTemplate() *v2alpha1.RunnerTemplateSpec {
	return templateWithRunnerResources(corev1.ResourceRequirements{})
}

// TestSizingOverrideCPULimitInjected is the silent cancellation Q489 exists for: a
// pod the profile built with no CPU limit is running with one.
func TestSizingOverrideCPULimitInjected(t *testing.T) {
	r := overrideReconciler(t, sizedWorkerPod("runner-abc", v2alpha1.SizingProfileThroughput,
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}))
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs, runnerTemplate())

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v2alpha1.ReasonCPULimitInjected {
		t.Fatalf("SizingProfileOverridden = %+v, want True/CPULimitInjected", cond)
	}
	if cond.ObservedGeneration != 3 {
		t.Fatalf("ObservedGeneration = %d, want 3", cond.ObservedGeneration)
	}
	for _, want := range []string{"runner-abc", "container runner", "(2)", "limitrange"} {
		if !strings.Contains(cond.Message, want) {
			t.Fatalf("message %q missing %q", cond.Message, want)
		}
	}
}

// TestSizingOverrideCleanPods: profile-built pods admitted as built.
func TestSizingOverrideCleanPods(t *testing.T) {
	r := overrideReconciler(t,
		sizedWorkerPod("runner-a", v2alpha1.SizingProfileThroughput, nil),
		sizedWorkerPod("runner-b", v2alpha1.SizingProfileThroughput,
			corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}), // memory limit is the profile's own
	)
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs, runnerTemplate())

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonNoCPULimitInjected {
		t.Fatalf("SizingProfileOverridden = %+v, want False/NoCPULimitInjected", cond)
	}
	if !strings.Contains(cond.Message, "2 profile-built") {
		t.Fatalf("message %q should count the pods it examined", cond.Message)
	}
}

// TestSizingOverrideIgnoresPodsTheProfileDidNotBuild is the case the annotation
// exists for: a pod created before the operator selected Throughput is legitimately
// running the template's CPU limit and must not read as an override.
func TestSizingOverrideIgnoresPodsTheProfileDidNotBuild(t *testing.T) {
	r := overrideReconciler(t,
		sizedWorkerPod("runner-old", "", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}),
		sizedWorkerPod("runner-binpack", v2alpha1.SizingProfileBinpack,
			corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}),
	)
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs, runnerTemplate())

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonAwaitingWorkerPods {
		t.Fatalf("SizingProfileOverridden = %+v, want False/AwaitingWorkerPods", cond)
	}
}

// TestSizingOverrideIgnoresInjectedSidecars: a container the template never declared
// — injected by the same admission chain — is not the profile's business.
func TestSizingOverrideIgnoresInjectedSidecars(t *testing.T) {
	sidecar := corev1.Container{
		Name:      "istio-proxy",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
	}
	r := overrideReconciler(t, sizedWorkerPod("runner-mesh", v2alpha1.SizingProfileThroughput, nil, sidecar))
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs, runnerTemplate())

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonNoCPULimitInjected {
		t.Fatalf("SizingProfileOverridden = %+v, want False/NoCPULimitInjected", cond)
	}
}

// TestSizingOverrideIgnoresDeletingPods: a pod on its way out is frozen evidence of
// an older state and cannot describe what runs now.
func TestSizingOverrideIgnoresDeletingPods(t *testing.T) {
	dying := sizedWorkerPod("runner-dying", v2alpha1.SizingProfileThroughput,
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")})
	now := metav1.Now()
	dying.DeletionTimestamp = &now
	dying.Finalizers = []string{"test.actions-gateway.com/hold"} // a fake-client delete-timestamped object needs one
	r := overrideReconciler(t, dying)
	rs := throughputSet()

	r.applySizingProfileOverride(context.Background(), rs, runnerTemplate())

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v2alpha1.ReasonAwaitingWorkerPods {
		t.Fatalf("SizingProfileOverridden = %+v, want False/AwaitingWorkerPods", cond)
	}
}

// TestSizingOverrideOtherProfilesRemoveIt: Throughput-only, and switching away drops
// a previously-True alarm rather than stranding it.
func TestSizingOverrideOtherProfilesRemoveIt(t *testing.T) {
	r := overrideReconciler(t, sizedWorkerPod("runner-abc", v2alpha1.SizingProfileThroughput,
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}))

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
			r.applySizingProfileOverride(context.Background(), rs, runnerTemplate())
			if findOverride(rs) == nil {
				t.Fatal("precondition: Throughput should have set the condition")
			}

			rs.Spec.Sizing = tc.sizing
			r.applySizingProfileOverride(context.Background(), rs, runnerTemplate())

			if cond := findOverride(rs); cond != nil {
				t.Fatalf("SizingProfileOverridden = %+v, want the condition removed", cond)
			}
		})
	}
}

// TestSizingOverrideListErrorWritesNoVerdict: a pod list failure must not overwrite a
// True alarm with a clean bill of health it did not observe.
func TestSizingOverrideListErrorWritesNoVerdict(t *testing.T) {
	r := &RunnerSetReconciler{
		Client: fake.NewClientBuilder().WithScheme(overrideScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					return errors.New("pods is forbidden")
				}
				return nil
			},
		}).Build(),
	}
	rs := throughputSet()
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:    v2alpha1.ConditionSizingProfileOverridden,
		Status:  metav1.ConditionTrue,
		Reason:  v2alpha1.ReasonCPULimitInjected,
		Message: "from an earlier reconcile",
	})

	r.applySizingProfileOverride(context.Background(), rs, runnerTemplate())

	cond := findOverride(rs)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v2alpha1.ReasonCPULimitInjected {
		t.Fatalf("SizingProfileOverridden = %+v, want the prior verdict left untouched", cond)
	}
}
