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

// Q575: a scale-set worker pod whose job went terminal at GitHub while the pod was
// still Pending can never start — the terminal JobCompleted reclaims the JIT-config
// Secret the pod mounts, and a pod that has not mounted yet cannot mount one that is
// gone. The reaper read the completion stamp only in its Running arm, so the pod sat out
// the whole pendingPodDeadline (10m by default) holding a concurrency slot and a node,
// and was then reported as a scheduling stall it never had. The v1.3.0-rc.3 dogfood gate
// caught eight of them at once.
//
// Proven here against a real apiserver so the reap rides the manager's Pod watch and
// RequeueAfter loop rather than a hand-driven reconcile. envtest runs no kubelet, which
// is exactly the state under test: a pod with an absent Secret volume stays Pending on
// its own, so the test only has to supply the completion stamp.

// createV2WorkerPodMountingSecret creates a worker pod that mounts secretName as its
// job-payload volume, the shape ProvisionScaleSetWorker builds. Nothing creates that
// Secret, so the pod is precisely the one the gate captured: Pending on a Secret that
// does not exist.
func createV2WorkerPodMountingSecret(t *testing.T, ns, setName, name, secretName string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				provisioner.LabelRunnerSet:           setName,
				provisioner.LabelAcquisitionProtocol: provisioner.AcquisitionProtocolScaleSet,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{{
				Name:         "job-payload",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName}},
			}},
			Containers: []corev1.Container{{
				Name:         "runner",
				Image:        "runner:test",
				VolumeMounts: []corev1.VolumeMount{{Name: "job-payload", MountPath: "/run/secrets/job-payload"}},
			}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
	return pod
}

// stampJobCompleted marks pod as if GitHub reported its job terminal completedAgo in the
// past, the way the scale-set listener's CleanupScaleSetJob does.
func stampJobCompleted(t *testing.T, pod *corev1.Pod, completedAgo time.Duration) {
	t.Helper()
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[provisioner.AnnotationJobCompletedAt] =
		time.Now().Add(-completedAgo).UTC().Format(time.RFC3339)
	require.NoError(t, k8sClient.Update(ctx, pod))
}

// TestV2_RunnerSet_CompletedPendingPodReaped proves the Q575 arm end to end: a Pending
// worker mounting an absent Secret, whose job is already over, is collected on the
// completion grace rather than left to the unrelated pendingPodDeadline — while a
// Pending worker whose job is still assigned is left strictly alone.
func TestV2_RunnerSet_CompletedPendingPodReaped(t *testing.T) {
	const ns = "v2-rs-completed-pending"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("completed-pending-set", ns, "gw")
	// An hour of pendingPodDeadline keeps the phase-based arm out of the way: anything
	// reaped here was reaped for its job's completion, not for sitting Pending.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, "completed-pending-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// Job over an hour ago: far past the 30-second completion grace.
	stranded := createV2WorkerPodMountingSecret(t, ns, "completed-pending-set", "worker-stranded", "job-ss-gone")
	stampJobCompleted(t, stranded, time.Hour)

	// No stamp: its job is still assigned, so it must keep waiting for its Secret.
	createV2WorkerPodMountingSecret(t, ns, "completed-pending-set", "worker-waiting", "job-ss-live")

	require.Eventually(t, func() bool { return podGone(ns, "worker-stranded") },
		20*time.Second, 100*time.Millisecond,
		"a Pending worker whose job completed must be reaped without waiting out pendingPodDeadline")

	require.False(t, podGone(ns, "worker-waiting"),
		"a Pending worker whose job is still assigned must never be reaped early")
}
