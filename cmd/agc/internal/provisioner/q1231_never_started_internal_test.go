package provisioner

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// kubeletDeletedPending returns a scale-set worker carrying the status a real kubelet
// published for a deleted worker still waiting on its image (Q1231): PodFailed, the
// deletion mark, and a synthetic Terminated/ContainerStatusUnknown container with no
// start, exit or ID. testdata/deleted_pending_pod_failed.json is the kind v1.36.1 capture.
func kubeletDeletedPending(t *testing.T) *corev1.Pod {
	t.Helper()
	var captured corev1.Pod
	raw, err := os.ReadFile("testdata/deleted_pending_pod_failed.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &captured))
	pod := scaleSetWorkerPod("runner-gpu-pending", identityAnnotations())
	pod.CreationTimestamp = captured.CreationTimestamp
	pod.DeletionTimestamp = captured.DeletionTimestamp
	pod.DeletionGracePeriodSeconds = captured.DeletionGracePeriodSeconds
	pod.Status = captured.Status
	return pod
}

// TestPodEverStarted_KubeletSyntheticTermination pins the reading both tiers' never-started
// routing rests on: the kubelet's synthetic terminated record is not a start, and a
// container that was running keeps its ID through the same rewrite.
func TestPodEverStarted_KubeletSyntheticTermination(t *testing.T) {
	pod := kubeletDeletedPending(t)
	require.NotNil(t, pod.Status.ContainerStatuses[0].State.Terminated,
		"the capture records the waiting container as terminated")
	assert.False(t, podEverStarted(pod), "a synthetic termination with no start, exit or ID is not a start")
	_, observed := podLifetime(pod)
	assert.False(t, observed, "a worker that never started adds no job-duration sample")

	ran := pod.DeepCopy()
	ran.Status.ContainerStatuses[0].ContainerID = "containerd://runner"
	assert.True(t, podEverStarted(ran), "a container the runtime created and lost keeps its ID, and did start")
}

// TestAbandonedAwaitingRecovery_KubeletFailedOnDeletedPendingWorker is the scale-set half:
// the scan takes the kubelet's real publish for an external delete of a Pending worker,
// and the deletion arm still leaves it alone.
func TestAbandonedAwaitingRecovery_KubeletFailedOnDeletedPendingWorker(t *testing.T) {
	pod := kubeletDeletedPending(t)
	assert.True(t, abandonedAwaitingRecovery(pod), "an externally deleted worker that never ran is abandoned")
	_, ok := disruptionAwaitingRecovery(pod)
	assert.False(t, ok, "a job that never ran has no failed job for rerun-failed-jobs to act on")
}
