//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q547: deleting an ActionsGateway removes the AGC that is the tenant's only worker
// reaper, but not the worker pods — they are owned by the RunnerSet, which survives
// gateway deletion by design. The pods then keep their do-not-disrupt annotations and
// pin a billable node until the kubelet's activeDeadlineSeconds fires, up to
// maxWorkerLifetime (12h by default) later.
//
// This runs against a real apiserver because the whole design rests on an API
// behaviour a fake client only imitates: a gateway carrying a finalizer *persists* in
// a terminating state, so the AGC has a window in which to observe the deletion
// timestamp and still hold the RBAC to act on it. Here the finalizer stands in for the
// GMC's own cleanup finalizer, which is what holds that window open in production.

// TestV2_RunnerSet_GatewayTeardownReapsWorkers proves the reap end to end: pods the
// ordinary reaper would retain indefinitely are deleted once the gateway starts
// terminating, and the status counts the GMC gates its teardown on reach zero.
func TestV2_RunnerSet_GatewayTeardownReapsWorkers(t *testing.T) {
	const ns = "v2-rs-gw-teardown"
	createNSForAGC(t, ns)

	gw := newGatewayForSet("gw", ns, "")
	// Stands in for the GMC's actions-gateway.com/gmc-cleanup finalizer: without one,
	// the delete below is immediate and there is no terminating state to observe.
	gw.Finalizers = []string{"test.actions-gateway.com/hold-teardown"}
	require.NoError(t, k8sClient.Create(ctx, gw))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))

	rs := newRunnerSet("teardown-set", ns, "gw")
	// Neither ordinary reap arm may fire: whatever is deleted here is deleted because
	// the gateway is terminating, not because a TTL or deadline elapsed.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		var held v2alpha1.ActionsGateway
		if err := k8sClient.Get(bg, types.NamespacedName{Namespace: ns, Name: "gw"}, &held); err == nil {
			held.Finalizers = nil
			_ = k8sClient.Update(bg, &held)
		}
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, "teardown-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// A Running worker with no completion stamp is the pod the ordinary reaper retains
	// forever — the one that pinned the node in the 2026-07-31 dogfood observation.
	running := createV2WorkerPod(t, ns, "teardown-set", "worker-running")
	running.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Status().Update(ctx, running))
	// Pending, nowhere near its (one-hour) deadline.
	createV2WorkerPod(t, ns, "teardown-set", "worker-pending")

	require.NoError(t, k8sClient.Delete(ctx, gw))

	require.Eventually(t, func() bool {
		return podGone(ns, "worker-running") && podGone(ns, "worker-pending")
	}, 20*time.Second, 100*time.Millisecond,
		"a terminating gateway must reap every worker pod, whatever its phase or age")

	// The GMC holds its teardown open until these reach zero, so they are what actually
	// releases the AGC Deployment for deletion.
	require.Eventually(t, func() bool {
		var got v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "teardown-set"}, &got); err != nil {
			return false
		}
		return got.Status.ActiveJobs == 0 && got.Status.PendingJobs == 0
	}, 20*time.Second, 100*time.Millisecond,
		"status.activeJobs/pendingJobs must reach zero — the GMC gates teardown on them")

	waitForSetReadyReason(t, ns, "teardown-set", metav1.ConditionFalse, v2alpha1.ReasonGatewayTerminating)
}

// TestV2_RunnerSet_LiveGatewayRetainsWorkers is the control against the same real
// apiserver: an existing gateway reaps nothing, so the reap is attributable to the
// deletion timestamp rather than to the reconcile it happens to arrive with.
func TestV2_RunnerSet_LiveGatewayRetainsWorkers(t *testing.T) {
	const ns = "v2-rs-gw-live"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("live-set", ns, "gw")
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, "live-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	running := createV2WorkerPod(t, ns, "live-set", "worker-running")
	running.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Status().Update(ctx, running))

	require.Never(t, func() bool { return podGone(ns, "worker-running") },
		3*time.Second, 250*time.Millisecond,
		"a live gateway must never reap a worker pod")
}
