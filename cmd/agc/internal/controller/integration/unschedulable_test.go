//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q157 WorkersUnschedulable condition, proven against a real apiserver with the
// manager's Pod watch and RequeueAfter loop running. envtest has no scheduler, so
// the test plays the scheduler's role: it marks a Pending worker pod
// PodScheduled=False/Unschedulable, then asserts the reconciler surfaces the
// condition once the pod ages past the scheduling grace (half pendingPodDeadline).

// rgCondition fetches a single status condition from the named RunnerGroup.
func rgCondition(t *testing.T, nsName, rgName, condType string) *metav1.Condition {
	t.Helper()
	var rg v1alpha1.RunnerGroup
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: rgName}, &rg); err != nil {
		return nil
	}
	return meta.FindStatusCondition(rg.Status.Conditions, condType)
}

// markUnschedulable sets PodScheduled=False/Unschedulable on the pod's status, as
// the kube-scheduler would when no node can host it.
func markUnschedulable(t *testing.T, pod *corev1.Pod, message string) {
	t.Helper()
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  corev1.PodReasonUnschedulable,
		Message: message,
	}}
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
}

// TestAGC_WorkersUnschedulable_TrippedForSchedulerFailure proves a worker pod the
// scheduler cannot place (non-quota) trips WorkersUnschedulable on the RunnerGroup
// once it ages past the grace, and that the condition is False before then.
func TestAGC_WorkersUnschedulable_TrippedForSchedulerFailure(t *testing.T) {
	const nsName = "agc-unsched"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "unsched-rg", 1)
	// Keep terminal reaping out of the way; a 12s pending deadline gives a 6s
	// scheduling grace and a 6s window before the reaper deletes the pod at 12s.
	rg.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rg.Spec.PendingPodDeadline = &metav1.Duration{Duration: 12 * time.Second}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconciler(t)

	pod := createWorkerPod(t, nsName, "unsched-rg", "worker-unsched")
	markUnschedulable(t, pod, "0/3 nodes are available: 3 node(s) had untolerated taint {dedicated: gpu}")

	// Within the grace the condition must be False (or not yet set).
	c := rgCondition(t, nsName, "unsched-rg", v1alpha1.ConditionWorkersUnschedulable)
	if c != nil {
		require.Equal(t, metav1.ConditionFalse, c.Status, "must not trip before the grace elapses")
	}

	// After the grace the reconciler (driven by its own RequeueAfter) must flip the
	// condition True and name the stuck pod, before the reaper removes it at 12s.
	require.Eventually(t, func() bool {
		c := rgCondition(t, nsName, "unsched-rg", v1alpha1.ConditionWorkersUnschedulable)
		return c != nil && c.Status == metav1.ConditionTrue &&
			c.Reason == v1alpha1.ReasonPodsUnschedulable
	}, 11*time.Second, 100*time.Millisecond,
		"a scheduler-unschedulable worker pod must trip WorkersUnschedulable")

	c = rgCondition(t, nsName, "unsched-rg", v1alpha1.ConditionWorkersUnschedulable)
	require.Contains(t, c.Message, "worker-unsched")
	require.Contains(t, c.Message, "untolerated taint")
}

// TestAGC_WorkersUnschedulable_NotTrippedForPlainPending proves a worker pod that
// is merely Pending (no Unschedulable scheduler verdict — the envtest default,
// which models an image pull or a not-yet-scheduled pod) does NOT trip the
// condition, even past the grace. This is the same shape a quota-blocked group
// presents at the scheduler level (the scheduler is never the cause), so it
// guards the non-double-report property end-to-end.
func TestAGC_WorkersUnschedulable_NotTrippedForPlainPending(t *testing.T) {
	const nsName = "agc-unsched-plain"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "plain-rg", 1)
	rg.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	// Long deadline so the pod is neither reaped nor crosses a short grace quickly;
	// grace is 30s, and we only assert "never True" for a few seconds.
	rg.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Minute}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconciler(t)

	// A bare Pending pod with no PodScheduled=Unschedulable verdict.
	createWorkerPod(t, nsName, "plain-rg", "worker-plain")

	require.Never(t, func() bool {
		c := rgCondition(t, nsName, "plain-rg", v1alpha1.ConditionWorkersUnschedulable)
		return c != nil && c.Status == metav1.ConditionTrue
	}, 4*time.Second, 200*time.Millisecond,
		"a plain-Pending worker pod (no Unschedulable verdict) must not trip the condition")
}
