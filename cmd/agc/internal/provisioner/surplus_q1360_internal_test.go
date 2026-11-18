package provisioner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// Q1360, as measured on dogfood on 2026-11-18: GitHub assigned job-x, and with no runner
// taking it in five minutes, withdrew it (a JobCompleted naming no runner) and re-issued
// the same workflow job as job-y. job-x's runner came up four seconds later and took
// job-y, while job-y's own worker was still Pending. The surplus that moved off job-x's
// worker on that start found no idle Running worker and was dropped, so job-y's worker
// started, registered, and listened with no deadline until it was deleted by hand.

// TestMarkScaleSetJobStarted_MovesTheSurplusToAStillPendingWorker is that sequence.
func TestMarkScaleSetJobStarted_MovesTheSurplusToAStillPendingWorker(t *testing.T) {
	ctx := context.Background()
	p, target, fc, keys := surplusFixture(ctx, t, "job-x")

	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))
	require.Equal(t, []string{"job-x"}, stamped(ctx, t, fc, keys), "job-x's idle worker is the surplus")

	keys["job-y"] = provisionHolder(ctx, t, p, target, "job-y")
	setPhase(ctx, t, fc, keys["job-y"], corev1.PodPending)

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-y", RunnerName: "gpu-job-x"}))

	assert.Equal(t, []string{"job-y"}, stamped(ctx, t, fc, keys),
		"the start lifts job-x's stamp, and job-y's worker is the one left with nothing to run")
	assert.Equal(t, "job-x", podAnnotations(ctx, t, fc, keys["job-y"])[AnnotationSurplusForJob])
	assert.False(t, secretExists(ctx, t, fc, "team-a", "job-y"), "the reclaimed worker's Secret goes with it")

	// job-y finishes on job-x's runner. That stamps job-x's worker and leaves job-y's
	// stamp where it is.
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-y", "gpu-job-x"))
	assert.ElementsMatch(t, []string{"job-x", "job-y"}, stamped(ctx, t, fc, keys))
}

// TestMarkScaleSetJobStarted_PendingPickLeavesARunnerForEveryOutstandingJob is the
// same-batch composition: job-z is still assigned and its worker Pending when job-x's
// re-issue job-y is started on job-x's runner before job-y has a worker. The moved surplus
// lands on job-z's worker, which is correct only because job-y's worker is still built and
// can take job-z.
func TestMarkScaleSetJobStarted_PendingPickLeavesARunnerForEveryOutstandingJob(t *testing.T) {
	ctx := context.Background()
	p, target, fc, keys := surplusFixture(ctx, t, "job-x", "job-z")
	setPhase(ctx, t, fc, keys["job-z"], corev1.PodPending)
	require.NoError(t, p.CleanupScaleSetJob(ctx, target, "job-x", ""))

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-y", RunnerName: "gpu-job-x"}))
	require.Equal(t, []string{"job-z"}, stamped(ctx, t, fc, keys), "the surplus moves onto the only idle worker")

	keys["job-y"] = provisionHolder(ctx, t, p, target, "job-y")

	ann := podAnnotations(ctx, t, fc, keys["job-y"])
	assert.NotContains(t, ann, AnnotationJobCompletedAt, "job-y's worker is the runner left for job-z")
	assert.Empty(t, ann[AnnotationStartedJobID])
	assert.True(t, secretExists(ctx, t, fc, "team-a", "job-y"))
}
