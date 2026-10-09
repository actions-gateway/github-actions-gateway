package provisioner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q1153: job-x ended before any runner started it, so the set holds one runner more
// than it has jobs. The worker created for job-x is not that runner when its runner has
// started job-d, and leaving the completion at that left the real surplus worker
// listening until spec.maxWorkerLifetime. Every test here crosses a worker onto
// another job, because the uncrossed case passes with or without the fix.

// surplusFixture provisions a running worker for each of jobIDs.
func surplusFixture(ctx context.Context, t *testing.T, jobIDs ...string) (*Provisioner, *stubTarget, client.Client, map[string]client.ObjectKey) {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, nil, nil)
	target := scaleSetSecretTestTarget(&ResolvedSpec{WorkerImage: "runner:test"})
	keys := map[string]client.ObjectKey{}
	for _, id := range jobIDs {
		keys[id] = provisionHolder(ctx, t, p, target, id)
		setPhase(ctx, t, fc, keys[id], corev1.PodRunning)
	}
	return p, target, fc, keys
}

// stamped returns the jobs whose workers carry a completion stamp.
func stamped(ctx context.Context, t *testing.T, c client.Client, keys map[string]client.ObjectKey) []string {
	t.Helper()
	var out []string
	for _, id := range []string{"job-x", "job-y", "job-z", "job-w"} {
		if key, ok := keys[id]; ok {
			if _, ok := podAnnotations(ctx, t, c, key)[AnnotationJobCompletedAt]; ok {
				out = append(out, id)
			}
		}
	}
	return out
}

// TestCleanupScaleSetJob_ReclaimsAnIdleWorkerWhenTheMintedOneIsBusy is the defect: the
// worker created for job-x runs job-d, so the worker created for job-y, idle, is the
// one with nothing left to run.
func TestCleanupScaleSetJob_ReclaimsAnIdleWorkerWhenTheMintedOneIsBusy(t *testing.T) {
	ctx := context.Background()
	p, target, fc, keys := surplusFixture(ctx, t, "job-x", "job-y")
	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, "job-d", "gpu-job-x"))

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))

	assert.Equal(t, []string{"job-y"}, stamped(ctx, t, fc, keys),
		"the idle worker must get a reap deadline, and the busy one must not")
	assert.Equal(t, "job-x", podAnnotations(ctx, t, fc, keys["job-y"])[AnnotationSurplusForJob])
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-x"), "the busy worker mounts job-x's Secret")
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-y"), "the reclaimed worker's Secret goes with it")
}

// TestCleanupScaleSetJob_NeverReclaimsABusyOrStartingWorker pins the candidate set: a
// worker whose runner started a job is never stamped, and neither is one still Pending.
func TestCleanupScaleSetJob_NeverReclaimsABusyOrStartingWorker(t *testing.T) {
	ctx := context.Background()
	p, target, fc, keys := surplusFixture(ctx, t, "job-x", "job-y", "job-z")
	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, "job-d", "gpu-job-x"))
	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, "job-e", "gpu-job-y"))
	setPhase(ctx, t, fc, keys["job-z"], corev1.PodPending)

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))

	assert.Empty(t, stamped(ctx, t, fc, keys), "with no idle Running worker, nothing may be reclaimed")
	for _, id := range []string{"job-x", "job-y", "job-z"} {
		assert.True(t, secretExists(ctx, t, fc, "team-a", id), "%s's Secret must survive", id)
	}
}

// TestCleanupScaleSetJob_ReplayedCompletionReclaimsOnce pins idempotency: a re-created
// session replays job-x's completion, and one runnerless completion frees one runner.
func TestCleanupScaleSetJob_ReplayedCompletionReclaimsOnce(t *testing.T) {
	ctx := context.Background()
	p, target, fc, keys := surplusFixture(ctx, t, "job-x", "job-y", "job-z")
	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, "job-d", "gpu-job-x"))

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))

	assert.Len(t, stamped(ctx, t, fc, keys), 1, "a replayed completion must not reclaim a second worker")
}

// TestMarkScaleSetJobStarted_MovesASurplusStampToAnotherIdleWorker covers a wrong pick:
// the reclaimed worker's runner is given job-k after all, so the runner that would have
// taken job-k is the surplus one now, and the stamp follows it rather than vanishing.
func TestMarkScaleSetJobStarted_MovesASurplusStampToAnotherIdleWorker(t *testing.T) {
	ctx := context.Background()
	p, target, fc, keys := surplusFixture(ctx, t, "job-x", "job-y", "job-z")
	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, "job-d", "gpu-job-x"))
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))
	first := stamped(ctx, t, fc, keys)
	require.Len(t, first, 1)
	other := map[string]string{"job-y": "job-z", "job-z": "job-y"}[first[0]]

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, "job-k", "gpu-"+first[0]))

	assert.Equal(t, []string{other}, stamped(ctx, t, fc, keys),
		"the stamp must leave the worker that started job-k and land on the remaining idle one")
	assert.Equal(t, "job-x", podAnnotations(ctx, t, fc, keys[other])[AnnotationSurplusForJob])
	assert.NotContains(t, podAnnotations(ctx, t, fc, keys[first[0]]), AnnotationSurplusForJob)
}
