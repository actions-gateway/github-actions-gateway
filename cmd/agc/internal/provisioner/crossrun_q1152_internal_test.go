package provisioner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q1152: GitHub gives a scale-set job to whichever of the set's runners asks first, so
// the worker created for job-a can run job-b, from another run. Every test here builds
// that crossing — a pod minted for one job whose runner started another — because a
// test that keeps each job on its own worker passes against the defect.

// mintedWorker is a running scale-set worker created for jobID, stamped with the run
// identity of that job's assignment and the runner name the listener registered for it.
func mintedWorker(jobID, repository, runID string) *corev1.Pod {
	pod := scaleSetWorkerPod(scaleSetPodName("gpu", jobID), map[string]string{
		AnnotationRunID:      runID,
		AnnotationRepository: repository,
		AnnotationRunnerName: "gpu-" + jobID,
	})
	pod.Status.Phase = corev1.PodRunning
	return pod
}

// evictInPlace marks a stored pod evicted, the way the kubelet does under node pressure.
func evictInPlace(ctx context.Context, t *testing.T, c client.Client, name string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, &pod))
	evicted(&pod)
	require.NoError(t, c.Status().Update(ctx, &pod))
}

func reruns(paths chan string) []string {
	var out []string
	for {
		select {
		case p := <-paths:
			out = append(out, p)
		default:
			return out
		}
	}
}

// TestRecoverEvictedScaleSetWorkers_RerunsTheRunItsRunnerStarted is the eviction half:
// the worker minted for job-a (run 111) ran job-b (run 222) and was evicted. The run
// that lost a job is 222, and 111's job-a is running, or ran, somewhere else.
func TestRecoverEvictedScaleSetWorkers_RerunsTheRunItsRunnerStarted(t *testing.T) {
	ctx := context.Background()
	p, target, _, rerunCount, paths := recoveryFixture(t, mintedWorker("job-a", "myorg/repo-a", "111"))

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{
		JobID: "job-b", RunnerName: "gpu-job-a", Owner: "myorg", Repository: "repo-b", RunID: "222", JobName: "shellcheck",
	}))
	evictInPlace(ctx, t, p.Client, scaleSetPodName("gpu", "job-a"))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	require.Equal(t, int64(1), rerunCount.Load())
	assert.Equal(t, []string{"/repos/myorg/repo-b/actions/runs/222/rerun-failed-jobs"}, reruns(paths),
		"recovery must re-run the run the evicted worker was serving, not the one it was created for")
}

// TestMarkScaleSetJobStarted_DropsAnotherJobsIdentityWhenTheStartCarriesNone covers a
// start with no run identity on a worker created for a different job. The stamped
// identity is then known to name the wrong run, so recovery must report it unknown
// rather than re-run it.
func TestMarkScaleSetJobStarted_DropsAnotherJobsIdentityWhenTheStartCarriesNone(t *testing.T) {
	ctx := context.Background()
	p, target, _, rerunCount, _ := recoveryFixture(t, mintedWorker("job-a", "myorg/repo-a", "111"))

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-b", RunnerName: "gpu-job-a"}))
	evictInPlace(ctx, t, p.Client, scaleSetPodName("gpu", "job-a"))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(), "run 111 lost nothing on this worker and must not be re-run")
	assert.Contains(t, target.events, "EvictionRecoveryIdentityUnknown")
}

// TestMarkScaleSetJobStarted_KeepsTheAssignedIdentityOnTheMintedWorker is the
// uncrossed control for the test above: the runner started the job its worker was
// created for, so the assignment's identity is that job's and must survive a start that
// carries none.
func TestMarkScaleSetJobStarted_KeepsTheAssignedIdentityOnTheMintedWorker(t *testing.T) {
	ctx := context.Background()
	p, target, _, _, paths := recoveryFixture(t, mintedWorker("job-a", "myorg/repo-a", "111"))

	require.NoError(t, p.MarkScaleSetJobStarted(ctx, target, ScaleSetJob{JobID: "job-a", RunnerName: "gpu-job-a"}))
	evictInPlace(ctx, t, p.Client, scaleSetPodName("gpu", "job-a"))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, []string{"/repos/myorg/repo-a/actions/runs/111/rerun-failed-jobs"}, reruns(paths))
}

// TestRecoverOrphanedScaleSetWorkers_SparesAJobRunningOnAnotherWorker is the orphan
// scan's first direction: job-b's own worker is gone, because its runner ran some other
// job and was reaped, while job-b runs on the worker created for job-a.
func TestRecoverOrphanedScaleSetWorkers_SparesAJobRunningOnAnotherWorker(t *testing.T) {
	ctx := context.Background()
	host := mintedWorker("job-a", "myorg/repo-a", "111")
	host.Annotations[AnnotationStartedJobID] = "job-b"
	p, target, _, rerunCount, _ := recoveryFixture(t, host)

	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{
		{JobID: "job-b", Owner: "myorg", Repository: "repo-b", RunID: "222"},
	})
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(), "job-b is running; re-running its run would be the retry loop")
	assert.NotContains(t, target.events, "OrphanedWorkerRecovered")
}

// TestRecoverOrphanedScaleSetWorkers_RecoversAJobWhoseNamesakeRunsAnother is the
// reverse: job-a's own worker is alive but its runner started job-b, and nothing alive
// started job-a — so job-a's real worker is the one that went away.
func TestRecoverOrphanedScaleSetWorkers_RecoversAJobWhoseNamesakeRunsAnother(t *testing.T) {
	ctx := context.Background()
	namesake := mintedWorker("job-a", "myorg/repo-a", "111")
	namesake.Annotations[AnnotationStartedJobID] = "job-b"
	p, target, _, _, paths := recoveryFixture(t, namesake)

	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{
		{JobID: "job-a", Owner: "myorg", Repository: "repo-a", RunID: "111"},
	})
	require.NoError(t, err)
	<-done

	assert.Equal(t, []string{"/repos/myorg/repo-a/actions/runs/111/rerun-failed-jobs"}, reruns(paths),
		"a live pod named for job-a is not job-a's worker once its runner started another job")
}

// TestRecoverOrphanedScaleSetWorkers_SparesAQueuedJobWhileAWorkerIsIdle is the shape
// mid-crossing: job-a's namesake runs job-b, and job-b's namesake is still starting with
// no job. job-a has not started anywhere, so it is queued, and the idle worker is the
// one that will take it; nothing was lost.
func TestRecoverOrphanedScaleSetWorkers_SparesAQueuedJobWhileAWorkerIsIdle(t *testing.T) {
	ctx := context.Background()
	namesake := mintedWorker("job-a", "myorg/repo-a", "111")
	namesake.Annotations[AnnotationStartedJobID] = "job-b"
	idle := mintedWorker("job-b", "myorg/repo-b", "222")
	idle.Status.Phase = corev1.PodPending
	p, target, _, rerunCount, _ := recoveryFixture(t, namesake, idle)

	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{
		{JobID: "job-a", Owner: "myorg", Repository: "repo-a", RunID: "111"},
		{JobID: "job-b", Owner: "myorg", Repository: "repo-b", RunID: "222"},
	})
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(), "job-a is queued with an idle worker to take it")
	assert.NotContains(t, target.events, "OrphanedWorkerRecovered")
}

// TestRecoverOrphanedScaleSetWorkers_AnExitedWorkerTakesNoJob is the control for the
// test above: a worker that has exited, or is being deleted, will take no job, so it
// does not stand in for job-a.
func TestRecoverOrphanedScaleSetWorkers_AnExitedWorkerTakesNoJob(t *testing.T) {
	for name, gone := range map[string]func(*corev1.Pod){
		"failed": func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		"deleting": func(p *corev1.Pod) {
			p.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			p.Finalizers = []string{"test"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			namesake := mintedWorker("job-a", "myorg/repo-a", "111")
			namesake.Annotations[AnnotationStartedJobID] = "job-b"
			other := mintedWorker("job-c", "myorg/repo-c", "333")
			gone(other)
			p, target, _, _, paths := recoveryFixture(t, namesake, other)

			done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{
				{JobID: "job-a", Owner: "myorg", Repository: "repo-a", RunID: "111"},
			})
			require.NoError(t, err)
			<-done

			assert.Equal(t, []string{"/repos/myorg/repo-a/actions/runs/111/rerun-failed-jobs"}, reruns(paths))
		})
	}
}

// TestRecoverOrphanedScaleSetWorkers_ReadsTheClaimByTheJobItsWorkerServed covers the
// ledger. A previous process recovered job-b off the worker created for job-a, so the
// claim sits under that pod's name; a claim on job-c's namesake, which served job-d,
// recovered job-d and not job-c.
func TestRecoverOrphanedScaleSetWorkers_ReadsTheClaimByTheJobItsWorkerServed(t *testing.T) {
	ctx := context.Background()
	p, target, _, _, paths := recoveryFixture(t)
	require.NoError(t, p.claimDisruptionRecovery(ctx, target, scaleSetPodName("gpu", "job-a"), "job-b", recoveryCauseDeletion))
	require.NoError(t, p.claimDisruptionRecovery(ctx, target, scaleSetPodName("gpu", "job-c"), "job-d", recoveryCauseDeletion))

	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{
		{JobID: "job-b", Owner: "myorg", Repository: "repo-b", RunID: "222"},
		{JobID: "job-c", Owner: "myorg", Repository: "repo-c", RunID: "333"},
	})
	require.NoError(t, err)
	<-done

	assert.Equal(t, []string{"/repos/myorg/repo-c/actions/runs/333/rerun-failed-jobs"}, reruns(paths),
		"job-b was already recovered off another job's worker; job-c's was never recovered")
}
