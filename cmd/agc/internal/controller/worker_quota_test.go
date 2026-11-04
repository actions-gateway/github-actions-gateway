package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
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
	return s
}

// wqPool is a worker pool of up to maxWorkers pods, each requesting perCPU and
// perMem, counted under the RunnerSet label "rg".
func wqPool(maxWorkers int32, perCPU, perMem string) workerQuotaPool {
	req := corev1.ResourceList{}
	if perCPU != "" {
		req[corev1.ResourceCPU] = resource.MustParse(perCPU)
	}
	if perMem != "" {
		req[corev1.ResourceMemory] = resource.MustParse(perMem)
	}
	return workerQuotaPool{
		namespace: "tenant-ns",
		podSpec: &corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runner", Resources: corev1.ResourceRequirements{Requests: req}}},
		},
		ceiling: maxWorkers,
		bounded: true,
		label:   provisioner.LabelRunnerSet,
		name:    "rg",
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
			Labels:    map[string]string{provisioner.LabelRunnerSet: "rg"},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestWorkerFootprint(t *testing.T) {
	fp := provisioner.WorkerFootprint(wqPool(10, "100m", "256Mi").podSpec, 10)
	pods := fp[corev1.ResourcePods]
	assert.Equal(t, int64(10), pods.Value())
	cpu := fp[corev1.ResourceRequestsCPU]
	assert.Equal(t, "1", cpu.String(), "10 × 100m = 1")
	mem := fp[corev1.ResourceRequestsMemory]
	assert.Equal(t, int64(10*256*1024*1024), mem.Value())
}

func TestCountActiveWorkerPods(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(wqScheme(t)).WithObjects(
		wqWorkerPod("a", corev1.PodRunning),
		wqWorkerPod("b", corev1.PodPending),
		wqWorkerPod("c", corev1.PodSucceeded), // terminal — not counted
		wqWorkerPod("d", corev1.PodFailed),    // terminal — not counted
	).Build()
	assert.Equal(t, int32(2), countActiveWorkerPodsByLabel(context.Background(), c, "tenant-ns", provisioner.LabelRunnerSet, "rg"))
}

func TestEvalWorkerQuota(t *testing.T) {
	tests := []struct {
		name         string
		pool         workerQuotaPool
		objs         []client.Object
		wantPressure bool
		wantPReason  string
		wantExceeded bool
		wantEReason  string
	}{
		{
			name:        "no quota",
			pool:        wqPool(10, "100m", ""),
			wantPReason: "NoQuota",
			wantEReason: "NoRejection",
		},
		{
			name: "ample headroom",
			pool: wqPool(10, "100m", ""),
			objs: []client.Object{wqQuota("c",
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("100")},
				corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")})},
			wantPReason: "QuotaHeadroomSufficient",
			wantEReason: "NoRejection",
		},
		{
			name: "exceeded: cannot fit one more pod supersedes pressure",
			pool: wqPool(10, "100m", ""),
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
			pool: wqPool(10, "100m", ""),
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
			pool: wqPool(10, "1", ""),
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
			c := fake.NewClientBuilder().WithScheme(wqScheme(t)).WithObjects(tc.objs...).Build()
			st := evalWorkerQuotaFor(context.Background(), c, tc.pool)
			assert.Equal(t, tc.wantPressure, st.pressure, "pressure")
			assert.Equal(t, tc.wantPReason, st.pressureReason, "pressureReason")
			assert.Equal(t, tc.wantExceeded, st.exceeded, "exceeded")
			assert.Equal(t, tc.wantEReason, st.exceededReason, "exceededReason")
		})
	}
}

func TestEvalWorkerQuota_ListErrorIsBenign(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(wqScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("forbidden")
			},
		}).Build()
	st := evalWorkerQuotaFor(context.Background(), c, wqPool(10, "100m", ""))
	assert.False(t, st.pressure)
	assert.False(t, st.exceeded)
	assert.Equal(t, "QuotaUnknown", st.pressureReason)
}

// TestRunnerSetWorkerQuota pins the RunnerSet evaluation's verdict in every tier
// against an equivalent pod shape, ceiling, and namespace quota (Q521).
func TestRunnerSetWorkerQuota(t *testing.T) {
	const ns = "tenant-ns"
	const ceiling = 3

	quota := func(cpu string) *corev1.ResourceQuota {
		return &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
			Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(cpu)}},
		}
	}
	workerPod := func(label, name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Labels: map[string]string{label: "pool"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}

	tests := []struct {
		name string
		// hard is the quota's requests.cpu ceiling; empty means no quota at all.
		hard string
		// active worker pods already counted against the pool's ceiling.
		active      int
		wantPReason string
		wantEReason string
	}{
		{"no quota", "", 0, "NoQuota", "NoRejection"},
		{"cannot admit one worker", "100m", 0, "Superseded", "QuotaExhausted"},
		{"cannot reach the ceiling", "1", 0, "InsufficientQuotaHeadroom", "NoRejection"},
		// 1200m fits the remaining 2 × 500m but not a full 3 × 500m, so the active
		// count is load-bearing: a pool that counted its workers under the wrong
		// label would report pressure here.
		{"ceiling reachable from current count", "1200m", 1, "QuotaHeadroomSufficient", "NoRejection"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objsFor := func(label string) []client.Object {
				var objs []client.Object
				if tc.hard != "" {
					objs = append(objs, quota(tc.hard))
				}
				for i := 0; i < tc.active; i++ {
					objs = append(objs, workerPod(label, fmt.Sprintf("worker-%d", i)))
				}
				return objs
			}

			rs := rsObj("pool", ns, func(r *v2.RunnerSet) { r.Spec.MaxWorkers = ptr.To(int32(ceiling)) })
			rsr := &RunnerSetReconciler{Client: fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).
				WithObjects(objsFor(provisioner.LabelRunnerSet)...).Build()}
			st := rsr.evalRunnerSetWorkerQuota(context.Background(), rs, capTemplate("500m"))

			assert.Equal(t, tc.wantPReason, st.pressureReason)
			assert.Equal(t, tc.wantEReason, st.exceededReason)
		})
	}
}

func TestResourceListEqual_AGC(t *testing.T) {
	a := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")}
	b := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5000m")}
	assert.True(t, resourceListEqual(a, b))
	assert.False(t, resourceListEqual(a, corev1.ResourceList{corev1.ResourcePods: resource.MustParse("6")}))
}

func TestConditionGaugeValue_AGC(t *testing.T) {
	conds := []metav1.Condition{{Type: apiconditions.ConditionWorkerQuotaExceeded, Status: metav1.ConditionTrue}}
	assert.Equal(t, float64(1), conditionGaugeValue(conds, apiconditions.ConditionWorkerQuotaExceeded))
	assert.Equal(t, float64(0), conditionGaugeValue(conds, apiconditions.ConditionWorkerQuotaPressure))
}
