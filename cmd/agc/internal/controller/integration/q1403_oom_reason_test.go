//go:build integration

package integration_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestV2_RunnerSet_ScaleSet_AReasonOutlivesItsWorker (Q1403) takes the order the kill has
// through the real wiring: the worker fails, the reporter's condition follows as a status
// update that changes nothing else, and the pod is gone before its job is red.
func TestV2_RunnerSet_ScaleSet_AReasonOutlivesItsWorker(t *testing.T) {
	f := newOrphanFixture(t, "v2-rs-ss-q1403", "ss-q1403", "linux-q1403")
	f.startAGC(t)
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
	require.NotEmpty(t, runner)
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
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: "oom.actions-gateway.com/runner",
		Status: corev1.ConditionTrue, Reason: "AbovePods", LastTransitionTime: metav1.Now()})
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))
	require.NoError(t, client.IgnoreNotFound(k8sClient.Delete(ctx, &pod, client.GracePeriodSeconds(0))))

	// The same watch delivers this pod after the condition, so its count says that the
	// condition has been handled.
	marker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "marker", Namespace: f.ns, Labels: map[string]string{provisioner.LabelRunnerSet: f.setName}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
	}
	require.NoError(t, k8sClient.Create(ctx, marker))
	require.Eventually(t, func() bool {
		var rs v2.RunnerSet
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: f.setName}, &rs) == nil && rs.Status.PendingJobs == 1
	}, 20*time.Second, 50*time.Millisecond, "the marker must be counted")

	require.True(t, f.srv.CompleteRunningJob(f.ssID, jobID, runner, "failed"))
	reason := func() string {
		var cm corev1.ConfigMap
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "scaleset-failures-" + f.setName}, &cm); err != nil {
			return ""
		}
		var ledger struct {
			Failures map[string]struct {
				JobID   string `json:"jobID"`
				Context struct {
					Containers []struct {
						OOMReason string `json:"oomReason"`
					} `json:"containers"`
				} `json:"context"`
			} `json:"failures"`
		}
		require.NoError(t, json.Unmarshal([]byte(cm.Data["failures.json"]), &ledger))
		for _, r := range ledger.Failures {
			if r.JobID == jobID && len(r.Context.Containers) == 1 {
				return r.Context.Containers[0].OOMReason
			}
		}
		return ""
	}
	assert.Eventually(t, func() bool { return reason() == "AbovePods" }, 20*time.Second, 50*time.Millisecond,
		"the record holds the limit the condition named")
}
