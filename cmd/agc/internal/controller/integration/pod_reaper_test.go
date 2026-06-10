//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q95 worker-pod reaper, proven against a real apiserver with the manager's
// Pod watch and RequeueAfter loop running — behaviours a fake-client unit test
// cannot exercise (real watch event delivery, status-subresource semantics,
// and the requeue timer driving a reap with no further object writes).

// createWorkerPod creates a minimal pod carrying the worker label for rgName.
func createWorkerPod(t *testing.T, nsName, rgName, name string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels:    map[string]string{provisioner.LabelRunnerGroup: rgName},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "runner", Image: "runner:test"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
	return pod
}

// podGone reports whether the named pod no longer exists.
func podGone(nsName, name string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: name}, &corev1.Pod{})
	return apierrors.IsNotFound(err)
}

// TestAGC_Reaper_CompletedPodDeletedAfterTTL proves a worker pod that reached
// a terminal phase is deleted once completedPodTTL elapses. The pod's phase
// transition arrives through the real Pod watch, and the reap itself fires off
// the reconciler's RequeueAfter timer (no object changes after the status
// update), so this exercises the full production trigger chain.
func TestAGC_Reaper_CompletedPodDeletedAfterTTL(t *testing.T) {
	const nsName = "agc-reap-completed"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "reap-completed-rg", 1)
	rg.Spec.CompletedPodTTL = &metav1.Duration{Duration: 2 * time.Second}
	// Keep the Pending deadline out of the way: the pod stays Pending (envtest
	// has no scheduler) until the test flips its phase.
	rg.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconciler(t)

	pod := createWorkerPod(t, nsName, "reap-completed-rg", "runner-completed")

	// Drive the pod to Succeeded via the status subresource, as the kubelet would.
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runner",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   0,
				FinishedAt: metav1.Now(),
			},
		},
	}}
	require.NoError(t, k8sClient.Status().Update(ctx, pod))

	require.Eventually(t, func() bool { return podGone(nsName, "runner-completed") },
		20*time.Second, 100*time.Millisecond,
		"terminal worker pod should be reaped after completedPodTTL")
}

// TestAGC_Reaper_StuckPendingPodDeletedAfterDeadline proves a worker pod that
// never leaves Pending — envtest has no scheduler, so every pod is genuinely
// unscheduled, exactly the stuck shape Q95 targets — is deleted once
// pendingPodDeadline elapses, and that a fresh Pending pod within the deadline
// is left alone.
func TestAGC_Reaper_StuckPendingPodDeletedAfterDeadline(t *testing.T) {
	const nsName = "agc-reap-pending"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "reap-pending-rg", 1)
	rg.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rg.Spec.PendingPodDeadline = &metav1.Duration{Duration: 3 * time.Second}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconciler(t)

	createWorkerPod(t, nsName, "reap-pending-rg", "runner-stuck")

	require.Eventually(t, func() bool { return podGone(nsName, "runner-stuck") },
		20*time.Second, 100*time.Millisecond,
		"stuck-Pending worker pod should be reaped after pendingPodDeadline")

	// A new Pending pod created now is within its deadline and must survive a
	// few reconcile rounds (the previous reap proves the reaper is active).
	createWorkerPod(t, nsName, "reap-pending-rg", "runner-fresh")
	require.Never(t, func() bool { return podGone(nsName, "runner-fresh") },
		2*time.Second, 200*time.Millisecond,
		"a Pending worker pod within the deadline must not be reaped")
}

// TestAGC_Reaper_WorkerPodHasOwnerRef proves a pod created by the real
// provisioner path carries a controller OwnerReference to its RunnerGroup
// with the UID the apiserver assigned — the hook Kubernetes GC uses to
// cascade-delete worker pods on RunnerGroup/namespace deletion. (The cascade
// itself needs the GC controller, which envtest does not run; the Tier-A kind
// e2e asserts the same ownerRef end-to-end where GC is live.)
func TestAGC_Reaper_WorkerPodHasOwnerRef(t *testing.T) {
	const nsName = "agc-reap-ownerref"
	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, "ownerref-rg", 2)
	// Long TTL/deadline so nothing reaps the pod while we inspect it.
	rg.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rg.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	startAGCReconcilerWithProvisioner(t, provisionerOptions{})

	id := enqueueJobOnOwnerSession(15*time.Second, "ownerref-rg", nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for ownerref-rg should register")

	pod := waitForWorkerPod(t, nsName, "ownerref-rg")

	require.Len(t, pod.OwnerReferences, 1, "worker pod must carry exactly one ownerReference")
	ref := pod.OwnerReferences[0]
	require.Equal(t, "RunnerGroup", ref.Kind)
	require.Equal(t, "ownerref-rg", ref.Name)
	require.Equal(t, rg.UID, ref.UID, "ownerReference must carry the apiserver-assigned RunnerGroup UID")
	require.NotNil(t, ref.Controller)
	require.True(t, *ref.Controller)
}
