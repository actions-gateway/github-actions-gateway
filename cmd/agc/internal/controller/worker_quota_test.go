package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func wqScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func wqRunnerGroup(maxWorkers int32, perCPU, perMem string) *v1alpha1.RunnerGroup {
	mw := maxWorkers
	req := corev1.ResourceList{}
	if perCPU != "" {
		req[corev1.ResourceCPU] = resource.MustParse(perCPU)
	}
	if perMem != "" {
		req[corev1.ResourceMemory] = resource.MustParse(perMem)
	}
	return &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "rg", Namespace: "tenant-ns"},
		Spec: v1alpha1.RunnerGroupSpec{
			MaxWorkers: &mw,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Resources: corev1.ResourceRequirements{Requests: req}}},
				},
			},
		},
	}
}

func wqQuota(name string, hard, used corev1.ResourceList) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-ns"},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
		Status:     corev1.ResourceQuotaStatus{Hard: hard, Used: used},
	}
}

func wqWorkerPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "tenant-ns",
			Labels:    map[string]string{provisioner.LabelRunnerGroup: "rg"},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestWorkerFootprint(t *testing.T) {
	fp := workerFootprint(wqRunnerGroup(10, "100m", "256Mi"), 10)
	pods := fp[corev1.ResourcePods]
	assert.Equal(t, int64(10), pods.Value())
	cpu := fp[corev1.ResourceRequestsCPU]
	assert.Equal(t, "1", cpu.String(), "10 × 100m = 1")
	mem := fp[corev1.ResourceRequestsMemory]
	assert.Equal(t, int64(10*256*1024*1024), mem.Value())
}

func TestCountActiveWorkerPods(t *testing.T) {
	r := &RunnerGroupReconciler{Client: fake.NewClientBuilder().WithScheme(wqScheme(t)).WithObjects(
		wqWorkerPod("a", corev1.PodRunning),
		wqWorkerPod("b", corev1.PodPending),
		wqWorkerPod("c", corev1.PodSucceeded), // terminal — not counted
		wqWorkerPod("d", corev1.PodFailed),    // terminal — not counted
	).Build()}
	assert.Equal(t, int32(2), r.countActiveWorkerPods(context.Background(), wqRunnerGroup(10, "100m", "")))
}

func TestEvalWorkerQuota(t *testing.T) {
	tests := []struct {
		name         string
		rg           *v1alpha1.RunnerGroup
		objs         []client.Object
		wantPressure bool
		wantPReason  string
		wantExceeded bool
		wantEReason  string
	}{
		{
			name:        "no quota",
			rg:          wqRunnerGroup(10, "100m", ""),
			wantPReason: "NoQuota",
			wantEReason: "NoRejection",
		},
		{
			name: "ample headroom",
			rg:   wqRunnerGroup(10, "100m", ""),
			objs: []client.Object{wqQuota("c",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("100")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")})},
			wantPReason: "QuotaHeadroomSufficient",
			wantEReason: "NoRejection",
		},
		{
			name: "exceeded: cannot fit one more pod supersedes pressure",
			rg:   wqRunnerGroup(10, "100m", ""),
			objs: []client.Object{wqQuota("c",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")})},
			wantExceeded: true,
			wantEReason:  "QuotaExhausted",
			wantPressure: false,
			wantPReason:  "Superseded",
		},
		{
			name: "pressure: cannot reach ceiling but can fit some",
			rg:   wqRunnerGroup(10, "100m", ""),
			// 8 pods free; 2 active workers seeded → need 8 more to reach 10.
			objs: []client.Object{
				wqQuota("c",
					corev1.ResourceList{corev1.ResourcePods: resource.MustParse("8")},
					corev1.ResourceList{corev1.ResourcePods: resource.MustParse("4")}),
				wqWorkerPod("w1", corev1.PodRunning),
				wqWorkerPod("w2", corev1.PodRunning),
			},
			wantPressure: true,
			wantPReason:  "InsufficientQuotaHeadroom",
			wantEReason:  "NoRejection",
		},
		{
			name: "cpu headroom exhausted → exceeded",
			rg:   wqRunnerGroup(10, "1", ""),
			objs: []client.Object{wqQuota("c",
				corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1")},
				corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1")})},
			wantExceeded: true,
			wantEReason:  "QuotaExhausted",
			wantPReason:  "Superseded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &RunnerGroupReconciler{Client: fake.NewClientBuilder().WithScheme(wqScheme(t)).WithObjects(tc.objs...).Build()}
			st := r.evalWorkerQuota(context.Background(), tc.rg)
			assert.Equal(t, tc.wantPressure, st.pressure, "pressure")
			assert.Equal(t, tc.wantPReason, st.pressureReason, "pressureReason")
			assert.Equal(t, tc.wantExceeded, st.exceeded, "exceeded")
			assert.Equal(t, tc.wantEReason, st.exceededReason, "exceededReason")
		})
	}
}

func TestEvalWorkerQuota_ListErrorIsBenign(t *testing.T) {
	r := &RunnerGroupReconciler{Client: fake.NewClientBuilder().WithScheme(wqScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("forbidden")
			},
		}).Build()}
	st := r.evalWorkerQuota(context.Background(), wqRunnerGroup(10, "100m", ""))
	assert.False(t, st.pressure)
	assert.False(t, st.exceeded)
	assert.Equal(t, "QuotaUnknown", st.pressureReason)
}

func TestResourceListEqual_AGC(t *testing.T) {
	a := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")}
	b := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5000m")}
	assert.True(t, resourceListEqual(a, b))
	assert.False(t, resourceListEqual(a, corev1.ResourceList{corev1.ResourcePods: resource.MustParse("6")}))
}

func TestConditionGaugeValue_AGC(t *testing.T) {
	conds := []metav1.Condition{{Type: conditionWorkerQuotaExceeded, Status: metav1.ConditionTrue}}
	assert.Equal(t, float64(1), conditionGaugeValue(conds, conditionWorkerQuotaExceeded))
	assert.Equal(t, float64(0), conditionGaugeValue(conds, conditionWorkerQuotaPressure))
}
