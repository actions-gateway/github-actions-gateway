//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Q420: a ScaleSet worker that registers but never receives its job sits at
// "Listening for Jobs" forever. The reaper counted PodRunning as active with no
// deadline of any kind, so such a pod held a concurrency slot, a quota slot, and a
// node until an operator deleted it by hand. The deadline now comes from the
// completion stamp the scale-set listener writes when GitHub reports the job
// terminal, which is why it is durable across an AGC restart — it lives on the pod.
//
// Proven here against a real apiserver so the reap rides the manager's Pod watch and
// RequeueAfter loop rather than a hand-driven reconcile. envtest runs no kubelet, so
// the test plays that role: it drives the pod to Running through the status
// subresource and back-dates the stamp past the grace instead of waiting it out.

// markRunningWithCompletedJob drives pod to Running and stamps it as if its job went
// terminal at GitHub completedAgo in the past. An empty completedAgo leaves the pod
// unstamped (its job is still assigned).
func markRunningWithCompletedJob(t *testing.T, pod *corev1.Pod, completedAgo time.Duration) {
	t.Helper()
	if completedAgo > 0 {
		stamp := time.Now().Add(-completedAgo).UTC().Format(time.RFC3339)
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[provisioner.AnnotationJobCompletedAt] = stamp
		require.NoError(t, k8sClient.Update(ctx, pod))
	}
	pod.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
}

// TestV2_RunnerSet_OrphanedRunningPodReaped proves the Q420 arm end to end: a Running
// worker pod whose job completed past the grace is deleted, while a Running pod within
// the grace and a Running pod whose job is still assigned are both left alone.
func TestV2_RunnerSet_OrphanedRunningPodReaped(t *testing.T) {
	const ns = "v2-rs-orphan"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("orphan-set", ns, "gw")
	// Keep the phase-based arms out of the way: only the completion stamp may reap.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, "orphan-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// Job over an hour ago: far past the five-minute grace.
	markRunningWithCompletedJob(t, createV2WorkerPod(t, ns, "orphan-set", "worker-orphaned"), time.Hour)
	// Job over just now: inside the grace, so the runner may still be shutting down.
	markRunningWithCompletedJob(t, createV2WorkerPod(t, ns, "orphan-set", "worker-finishing"), time.Second)
	// No stamp at all: its job is still assigned, so it must never get a deadline.
	markRunningWithCompletedJob(t, createV2WorkerPod(t, ns, "orphan-set", "worker-live"), 0)

	require.Eventually(t, func() bool { return podGone(ns, "worker-orphaned") },
		20*time.Second, 100*time.Millisecond,
		"a Running worker past its job's completion grace must be reaped")

	require.False(t, podGone(ns, "worker-finishing"),
		"a Running worker within the grace must be retained")
	require.False(t, podGone(ns, "worker-live"),
		"a Running worker whose job is still assigned must never be reaped")
}
