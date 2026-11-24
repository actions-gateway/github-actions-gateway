//go:build integration

package integration_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestV2_RunnerSet_ScaleSet_FailedJobsContextIsItsFinishedWorker (Q1376) takes the order
// dogfood measured through the real wiring: the failed job's completion arrives while its
// worker runs, and the record gains the worker's terminal state when the pod finishes.
func TestV2_RunnerSet_ScaleSet_FailedJobsContextIsItsFinishedWorker(t *testing.T) {
	f := newOrphanFixture(t, "v2-rs-ss-q1376", "ss-q1376", "linux-q1376")
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
	pod.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	recordedPhase := func() string {
		var cm corev1.ConfigMap
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "scaleset-failures-" + f.setName}, &cm); err != nil {
			return ""
		}
		var ledger struct {
			Failures map[string]struct {
				JobID   string `json:"jobID"`
				Context struct {
					PodPhase string `json:"podPhase"`
				} `json:"context"`
			} `json:"failures"`
		}
		require.NoError(t, json.Unmarshal([]byte(cm.Data["failures.json"]), &ledger))
		for _, r := range ledger.Failures {
			if r.JobID == jobID {
				return r.Context.PodPhase
			}
		}
		return ""
	}
	require.True(t, f.srv.StartAssignedJob(f.ssID, jobID, runner))
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(&pod), &pod) == nil &&
			pod.Annotations[provisioner.AnnotationStartedJobID] == jobID
	}, 20*time.Second, 50*time.Millisecond, "the start is recorded on the running worker")
	require.True(t, f.srv.CompleteRunningJob(f.ssID, jobID, runner, "failed"))
	require.Eventually(t, func() bool { return recordedPhase() == string(corev1.PodRunning) }, 20*time.Second, 50*time.Millisecond,
		"the failure is recorded at its completion, from the running worker")
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(&pod), &pod) == nil &&
			pod.Annotations[provisioner.AnnotationJobCompletedAt] != ""
	}, 20*time.Second, 50*time.Millisecond, "the completion stamps the running worker")

	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runner", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", FinishedAt: metav1.Now()}}}}
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))
	assert.Eventually(t, func() bool { return recordedPhase() == string(corev1.PodSucceeded) }, 20*time.Second, 50*time.Millisecond,
		"the record holds the worker as it finished")
}
