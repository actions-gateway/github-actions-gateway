//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestV2_RunnerSet_ScaleSet_AKillWaitsForItsReason takes the wait through the real
// wiring: under Act a killed worker is not claimed until the reporter's condition comes,
// and that status update alone starts its recovery, well inside the 30 s the wait allows.
func TestV2_RunnerSet_ScaleSet_AKillWaitsForItsReason(t *testing.T) {
	f := newOrphanFixture(t, "v2-rs-ss-q1406", "ss-q1406", "linux-q1406")
	stop := startRunnerSetReconcilerWithScaleSet(t, f.srv, func(p *provisioner.Provisioner) {
		p.GitHubAPIURL = f.gitHub.URL
		p.HTTPClient = f.gitHub.Client()
		p.OOMKillReasons = provisioner.OOMKillReasonsAct
	})
	t.Cleanup(stop)
	require.Eventually(t, func() bool {
		id, ok := f.srv.ScaleSetIDByName(f.label)
		f.ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	_, jobID := f.srv.EnqueueJob(f.ssID)

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(f.ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: f.setName}); err != nil || len(pods.Items) == 0 {
			return false
		}
		pod = pods.Items[0]
		return true
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the assigned job")
	runner := pod.Annotations[provisioner.AnnotationRunnerName]
	require.True(t, f.srv.StartAssignedJob(f.ssID, jobID, runner))
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(&pod), &pod) == nil &&
			pod.Annotations[provisioner.AnnotationStartedJobID] == jobID
	}, 20*time.Second, 50*time.Millisecond, "the start is recorded on the worker")

	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runner", ContainerID: "containerd://killed",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://killed",
			ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.Now()}}}}
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))
	claimed := func() bool {
		var got corev1.Pod
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(&pod), &got))
		_, ok := got.Annotations[provisioner.AnnotationEvictionHandledAt]
		return ok
	}
	assert.Never(t, claimed, 3*time.Second, 100*time.Millisecond, "the kill waits for its reason")

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(&pod), &pod))
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: "oom.actions-gateway.com/runner",
		Status: corev1.ConditionTrue, Reason: "PodsLimit", LastTransitionTime: metav1.Now()})
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))
	assert.Eventually(t, claimed, 10*time.Second, 100*time.Millisecond, "the reason ends the wait")
	assert.Never(t, func() bool { return f.reruns.Load() > 0 }, 2*time.Second, 100*time.Millisecond,
		"a kill above the pod is not re-run")
}
