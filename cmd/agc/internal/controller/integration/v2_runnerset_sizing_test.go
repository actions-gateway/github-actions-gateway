//go:build integration

package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// sizingStub is the suite's mutable SizingSource (Q359 Phase 2): tests seed
// per-RunnerSet recommendations with Set while the manager is running.
type sizingStub struct {
	mu   sync.Mutex
	recs map[types.NamespacedName][]v2alpha1.ContainerSizingRecommendation
}

func (s *sizingStub) SizingStatus(key types.NamespacedName) []v2alpha1.ContainerSizingRecommendation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recs[key]
}

func (s *sizingStub) Set(key types.NamespacedName, recs []v2alpha1.ContainerSizingRecommendation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recs == nil {
		s.recs = make(map[types.NamespacedName][]v2alpha1.ContainerSizingRecommendation)
	}
	s.recs[key] = recs
}

// TestV2_RunnerSet_SizingRecommendationAndDrift drives the Phase 2 acceptance
// path against the real apiserver: once the sizing source reports a confident
// measured recommendation far below the template's ask, the RunnerSet's status
// carries the recommendation and the advisory SizingDrift condition fires —
// and a subsequent status read-back (what the sampler's restart re-seed
// consumes) round-trips the persisted values.
func TestV2_RunnerSet_SizingRecommendationAndDrift(t *testing.T) {
	const ns = "v2-rs-sizing"
	createNSForAGC(t, ns)
	r := startRunnerSetReconciler(t)

	// Direct egress; template with a deliberately oversized runner ask.
	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gw) })
	tmpl := newRunnerTemplate("tmpl", ns)
	tmpl.Spec.PodTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tmpl) })
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// Seed a confident measured recommendation: 500m/1Gi against the 2-CPU/4Gi ask.
	key := types.NamespacedName{Namespace: ns, Name: "set"}
	windowStart := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	r.Sizing.(*sizingStub).Set(key, []v2alpha1.ContainerSizingRecommendation{{
		Container: "runner",
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("2112Mi"),
		},
		ObservedPeak: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("800m"),
			corev1.ResourceMemory: resource.MustParse("1536Mi"),
		},
		ObservedP95: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("450m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		SampleCount: 25,
		WindowStart: windowStart,
	}})
	// Touch the set to trigger a reconcile that picks the seeded snapshot up.
	//
	// Retry on conflict: the RunnerSet reconciler is running and writes to this
	// same object, so a write of its own landing between the Get and the Update
	// rejects ours with "the object has been modified". A bare Get-then-Update
	// fails the test outright when it loses that race — observed on #874, where
	// this line failed on a change that touched no AGC code. Mirrors the same
	// retry around the concurrent PodTemplate edit in pod_provisioning_test.go.
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, key, rs); err != nil {
			return err
		}
		if rs.Annotations == nil {
			rs.Annotations = map[string]string{}
		}
		rs.Annotations["test/sizing-poke"] = "1"
		return k8sClient.Update(ctx, rs)
	}))

	var got v2alpha1.RunnerSet
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, key, &got); err != nil {
			return false
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionSizingDrift)
		return len(got.Status.SizingRecommendation) == 1 &&
			cond != nil && cond.Status == metav1.ConditionTrue &&
			cond.Reason == v2alpha1.ReasonSizingDriftDetected
	}, 20*time.Second, 100*time.Millisecond,
		"RunnerSet should surface the sizing recommendation and SizingDrift=True")

	// The persisted status round-trips the seeded values — this is exactly what
	// the usage sampler's restart re-seed reads back.
	rec := got.Status.SizingRecommendation[0]
	require.Equal(t, "runner", rec.Container)
	require.Equal(t, int64(25), rec.SampleCount)
	require.True(t, rec.WindowStart.Equal(&windowStart), "windowStart should persist")
	require.Equal(t, "500m", rec.Requests.Cpu().String())
	require.Equal(t, "1536Mi", rec.ObservedPeak.Memory().String())
}
