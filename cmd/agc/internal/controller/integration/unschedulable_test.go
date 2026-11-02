//go:build integration

package integration_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// Q157 WorkersUnschedulable condition, proven against a real apiserver with the
// manager's Pod watch and RequeueAfter loop running. envtest has no scheduler, so
// the test plays the scheduler's role: it marks a Pending worker pod
// PodScheduled=False/Unschedulable, then asserts the reconciler surfaces the
// condition once the pod ages past the scheduling grace (half pendingPodDeadline).

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
