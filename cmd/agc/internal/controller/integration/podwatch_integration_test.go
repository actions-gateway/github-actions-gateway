//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// waitForQuiescence blocks until the reconciler's invocation count stops
// increasing for one poll interval, i.e. no reconcile is pending. Returning
// only after the count is stable guarantees that any straggler reconcile
// (e.g. the no-op confirming one that follows the first status write) has
// already run — so a condition injected afterwards cannot be drained until the
// next genuinely new event.
func waitForQuiescence(t *testing.T, r interface{ ReconcileCountForTest() int64 }) {
	t.Helper()
	var last int64 = -1
	require.Eventually(t, func() bool {
		c := r.ReconcileCountForTest()
		stable := c > 0 && c == last
		last = c
		return stable
	}, 15*time.Second, 200*time.Millisecond, "controller never quiesced")
}

func hasCondition(rg *v1alpha1.RunnerGroup, condType string) bool {
	for _, c := range rg.Status.Conditions {
		if c.Type == condType {
			return true
		}
	}
	return false
}

// TestAGC_Reconciler_WorkerPodEventTriggersReconcile proves the RunnerGroup
// controller's worker-Pod watch (k8s-best-practices §A A3 / Q63) is wired: a
// worker Pod lifecycle event re-triggers a reconcile so status refreshes and
// any buffered listener conditions are flushed, without waiting for the next
// RunnerGroup write or the cache resync.
//
// MaxListeners=0 keeps the test deterministic: no listener goroutines run and
// no provisioner pods are created, so the only Pod in the namespace — and the
// only thing that can wake the controller after it quiesces — is the worker Pod
// the test creates by hand.
func TestAGC_Reconciler_WorkerPodEventTriggersReconcile(t *testing.T) {
	const nsName = "agc-podwatch-test"
	const rgName = "podwatch-rg"
	const sentinelType = "PodWatchProbe"

	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, rgName, 0)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	r, _, _ := startAGCReconcilerOpts(t, provisionerOptions{})
	key := types.NamespacedName{Namespace: nsName, Name: rgName}

	// Let the controller reach steady state before we probe it.
	waitForQuiescence(t, r)

	// Inject a sentinel condition as a listener goroutine would. Because the
	// controller has quiesced, nothing drains it until the next new event.
	r.SetConditionForTest(nsName, rgName, metav1.Condition{
		Type:    sentinelType,
		Status:  metav1.ConditionTrue,
		Reason:  "Probe",
		Message: "injected by Pod-watch integration test",
	})

	before := r.ReconcileCountForTest()

	// Create a worker Pod for this RunnerGroup. With the controller quiesced,
	// this Pod's creation is the only thing that can trigger a reconcile.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runner-podwatch-probe",
			Namespace: nsName,
			Labels:    map[string]string{"actions-gateway/runner-group": rgName},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })

	// The Pod event must trigger at least one new reconcile, and that reconcile
	// must have drained the buffered sentinel condition into the status.
	require.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, key, &fetched); err != nil {
			return false
		}
		return r.ReconcileCountForTest() > before && hasCondition(&fetched, sentinelType)
	}, 15*time.Second, 50*time.Millisecond,
		"worker Pod creation did not trigger a reconcile that flushed the buffered condition (Pod watch not wired?)")
}
