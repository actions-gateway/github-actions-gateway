package provisioner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q1199: a scale-set job is replayed to a re-created session while its previous worker
// pod is terminating. The pod name is derived from the jobID, so the replay's create
// collides with a pod that is going away and will never run the runner this delivery
// registered.

// terminate starts a graceful deletion the test controls the end of: the finalizer holds
// the pod in place with a deletionTimestamp, as a kubelet's grace period or a lost node
// does.
func terminate(ctx context.Context, t *testing.T, c client.Client, key client.ObjectKey) corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, key, &pod))
	pod.Finalizers = append(pod.Finalizers, "test/grace")
	require.NoError(t, c.Update(ctx, &pod))
	require.NoError(t, c.Delete(ctx, &pod))
	require.NoError(t, c.Get(ctx, key, &pod))
	require.NotNil(t, pod.DeletionTimestamp, "the pod is terminating before the replay")
	return pod
}

func TestProvisionScaleSetWorker_ReplayDuringItsWorkersTerminationIsNotProvisioned(t *testing.T) {
	ctx := context.Background()
	fc := uidAssigningClient()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	podKey := provisionHolder(ctx, t, p, target, "job-a")
	old := terminate(ctx, t, fc, podKey)
	oldSecret := q1174Secret(ctx, t, fc, "job-a")

	err := p.ProvisionScaleSetWorker(ctx, target,
		ScaleSetJob{JobID: "job-a", JITConfig: "ZnJlc2g=", RunnerName: "gpu-job-a-2"})
	require.Error(t, err, "a terminating pod is not this delivery's worker, so the delivery is not provisioned")
	assert.True(t, IsWorkerTerminating(err), "the error is typed so the listener re-offers rather than redelivers: %v", err)

	var pod corev1.Pod
	require.NoError(t, fc.Get(ctx, podKey, &pod))
	assert.Equal(t, old.UID, pod.UID, "the terminating pod is left to finish")
	got := q1174Secret(ctx, t, fc, "job-a")
	assert.Equal(t, oldSecret.UID, got.UID, "the terminating pod's Secret is left to it")
}

// Once the terminating pod is gone, the re-offer provisions a worker of its own, mounting
// the JIT config that delivery minted.
func TestProvisionScaleSetWorker_ReplayAfterTheTerminationEndsGetsItsOwnWorker(t *testing.T) {
	ctx := context.Background()
	fc := uidAssigningClient()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	podKey := provisionHolder(ctx, t, p, target, "job-a")
	old := terminate(ctx, t, fc, podKey)
	job := ScaleSetJob{JobID: "job-a", JITConfig: "ZnJlc2g=", RunnerName: "gpu-job-a-2"}
	require.True(t, IsWorkerTerminating(p.ProvisionScaleSetWorker(ctx, target, job)))

	old.Finalizers = nil
	require.NoError(t, fc.Update(ctx, &old), "the grace period ends and the pod is removed")

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, job))
	var pod corev1.Pod
	require.NoError(t, fc.Get(ctx, podKey, &pod))
	assert.NotEqual(t, old.UID, pod.UID, "the re-offer creates a new worker")
	assert.Equal(t, "gpu-job-a-2", pod.Annotations[AnnotationRunnerName])
	got := q1174Secret(ctx, t, fc, "job-a")
	assert.Equal(t, "ZnJlc2g=", string(got.Data[jitConfigKey]), "the new worker mounts the JIT config the re-offer minted")
}
