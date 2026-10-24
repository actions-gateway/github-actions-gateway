package provisioner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q1174: a scale-set job is replayed to a re-created session after its worker was
// deleted and before the garbage collector deleted the worker's Secret. The collector
// has already read the Secret as orphaned, so its delete is pending; the replay must not
// hand that Secret to the new worker.

func q1174Secret(ctx context.Context, t *testing.T, c client.Client, jobID string) corev1.Secret {
	t.Helper()
	var s corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: scaleSetSecretName(jobID)}, &s))
	return s
}

// collectorDeletes is the collector's pending delete: the API server applies it only to
// the object whose UID the collector read, which the fake client's Delete does not check.
func collectorDeletes(ctx context.Context, t *testing.T, c client.Client, jobID string, uid types.UID) {
	t.Helper()
	var s corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: scaleSetSecretName(jobID)}, &s)
	if apierrors.IsNotFound(err) {
		return
	}
	require.NoError(t, err)
	if s.UID == uid {
		require.NoError(t, c.Delete(ctx, &s))
	}
}

func TestProvisionScaleSetWorker_ReplayAfterItsWorkerWentGetsAFreshSecret(t *testing.T) {
	ctx := context.Background()
	fc := uidAssigningClient()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})
	job := ScaleSetJob{JobID: "job-a", JITConfig: "c3BlbnQ=", RunnerName: "gpu-job-a"}

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, job))
	podKey := client.ObjectKey{Namespace: "team-a", Name: scaleSetPodName("gpu", "job-a")}
	read := q1174Secret(ctx, t, fc, "job-a")
	require.NoError(t, fc.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: podKey.Namespace, Name: podKey.Name}}))

	job.JITConfig, job.RunnerName = "ZnJlc2g=", "gpu-job-a-2"
	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, job))
	collectorDeletes(ctx, t, fc, "job-a", read.UID)

	require.True(t, secretExists(ctx, t, fc, "team-a", "job-a"),
		"the collector's delete of the old worker's Secret must not take the new worker's")
	got := q1174Secret(ctx, t, fc, "job-a")
	assert.Equal(t, "ZnJlc2g=", string(got.Data[jitConfigKey]), "the new worker mounts the JIT config this delivery minted")
	var pod corev1.Pod
	require.NoError(t, fc.Get(ctx, podKey, &pod))
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, pod.UID, got.OwnerReferences[0].UID, "the fresh Secret is handed to the new worker")

	collectOrphanedSecrets(ctx, t, fc)
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-a"), "the new worker's Secret is not orphaned")
}

// A Secret the RunnerSet still owns was staged by a delivery that has not created its pod
// yet, so a replay finding it must leave it as it is (Q373).
func TestProvisionScaleSetWorker_ReplayLeavesAnInFlightSecretAlone(t *testing.T) {
	ctx := context.Background()
	fc := uidAssigningClient()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	staged := p.buildSecret(target, scaleSetSecretName("job-a"), "job-a", "", nil, "aW5mbGlnaHQ=")
	require.NoError(t, fc.Create(ctx, staged))

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target,
		ScaleSetJob{JobID: "job-a", JITConfig: "cmVwbGF5", RunnerName: "gpu-job-a"}))

	got := q1174Secret(ctx, t, fc, "job-a")
	assert.Equal(t, staged.UID, got.UID, "the in-flight delivery's Secret is not replaced")
	assert.Equal(t, "aW5mbGlnaHQ=", string(got.Data[jitConfigKey]))
}

// A Secret already being deleted cannot be re-created until it is gone, so the delivery
// fails before it builds a pod that would wait on it, and the listener retries.
func TestProvisionScaleSetWorker_ReplayWaitsOutASecretBeingDeleted(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	deleting := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: scaleSetSecretName("job-a"),
		DeletionTimestamp: &now, Finalizers: []string{"test/hold"},
	}}
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(deleting).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})

	err := p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{JobID: "job-a", JITConfig: "cmVwbGF5", RunnerName: "gpu-job-a"})
	require.ErrorContains(t, err, "being deleted")
	var pod corev1.Pod
	err = fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: scaleSetPodName("gpu", "job-a")}, &pod)
	assert.True(t, apierrors.IsNotFound(err), "no worker is created to wait on a Secret that is going")
}
