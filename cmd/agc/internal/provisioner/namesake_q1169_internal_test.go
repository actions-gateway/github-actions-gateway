package provisioner

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// Q1169: a worker whose runner never started a job carries the run identity of the job
// it was created for, and GitHub can have given that job to another runner of the set.
// Recovering the worker by that identity then cancels or re-runs a run that lost
// nothing. Each test builds the crossing — job-a running on the worker minted for
// another job — beside a control where nothing else holds job-a.

// TestRecoverAbandonedScaleSetWorker_SparesARunWhoseJobRunsElsewhere is the abandoned
// path: the worker minted for job-a was reaped Pending, while job-a runs on job-b's.
func TestRecoverAbandonedScaleSetWorker_SparesARunWhoseJobRunsElsewhere(t *testing.T) {
	ctx := context.Background()
	host := mintedWorker("job-b", "myorg/repo-b", "222")
	host.Annotations[AnnotationStartedJobID] = "job-a"
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted, host)

	<-f.p.RecoverAbandonedScaleSetWorker(ctx, f.target, reapedPendingWorker(scaleSetPodName("gpu", "job-a")))

	forceCancel, _ := f.calls.snapshot()
	assert.Empty(t, forceCancel, "job-a is running on another worker; cancelling its run would end it")
	assert.Empty(t, f.p.pendingAbandonedRerunKeys())
}

// TestRecoverAbandonedScaleSetWorker_StillCancelsBesideOtherWorkers is the control: the
// other workers are busy with other jobs or idle, so nothing holds job-a, and Q766's
// recovery stands. An idle worker does not count here, since it is most often waiting
// on the same capacity the reaped one never got.
func TestRecoverAbandonedScaleSetWorker_StillCancelsBesideOtherWorkers(t *testing.T) {
	ctx := context.Background()
	busy := mintedWorker("job-b", "myorg/repo-b", "222")
	busy.Annotations[AnnotationStartedJobID] = "job-c"
	idle := mintedWorker("job-d", "myorg/repo-d", "444")
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted, busy, idle)

	<-f.p.RecoverAbandonedScaleSetWorker(ctx, f.target, reapedPendingWorker(scaleSetPodName("gpu", "job-a")))

	forceCancel, _ := f.calls.snapshot()
	assert.Equal(t, []string{"/repos/myorg/myrepo/actions/runs/4242/force-cancel"}, forceCancel)
}

// TestRecoverEvictedScaleSetWorkers_SparesANeverStartedWorkersHeldJob is the eviction
// path for a worker evicted before its runner started anything: its job-a is either
// running on another worker or can still be taken by an idle one.
func TestRecoverEvictedScaleSetWorkers_SparesANeverStartedWorkersHeldJob(t *testing.T) {
	for name, other := range map[string]func() *corev1.Pod{
		"started elsewhere": func() *corev1.Pod {
			pod := mintedWorker("job-b", "myorg/repo-b", "222")
			pod.Annotations[AnnotationStartedJobID] = "job-a"
			return pod
		},
		"idle worker": func() *corev1.Pod { return mintedWorker("job-c", "myorg/repo-c", "333") },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p, target, _, rerunCount, _ := recoveryFixture(t, evicted(mintedWorker("job-a", "myorg/repo-a", "111")), other())

			done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
			require.NoError(t, err)
			<-done

			assert.Equal(t, int64(0), rerunCount.Load(), "run 111 lost nothing on this worker and must not be re-run")
		})
	}
}

// TestRecoverEvictedScaleSetWorkers_RerunsWhenNothingHoldsTheJob is the control for the
// test above: the only other worker is busy with another job, so the check withholds
// nothing and recovery behaves as before.
func TestRecoverEvictedScaleSetWorkers_RerunsWhenNothingHoldsTheJob(t *testing.T) {
	ctx := context.Background()
	busy := mintedWorker("job-b", "myorg/repo-b", "222")
	busy.Annotations[AnnotationStartedJobID] = "job-c"
	p, target, _, _, paths := recoveryFixture(t, evicted(mintedWorker("job-a", "myorg/repo-a", "111")), busy)

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, []string{"/repos/myorg/repo-a/actions/runs/111/rerun-failed-jobs"}, reruns(paths))
}

// TestRecoverEvictedScaleSetWorkers_AStartedWorkerIgnoresIdleOnes keeps the check off a
// worker whose runner did start a job: that job lost its runner, whoever else is idle.
func TestRecoverEvictedScaleSetWorkers_AStartedWorkerIgnoresIdleOnes(t *testing.T) {
	ctx := context.Background()
	victim := mintedWorker("job-a", "myorg/repo-a", "111")
	victim.Annotations[AnnotationStartedJobID] = "job-a"
	p, target, _, _, paths := recoveryFixture(t, evicted(victim), mintedWorker("job-c", "myorg/repo-c", "333"))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, []string{"/repos/myorg/repo-a/actions/runs/111/rerun-failed-jobs"}, reruns(paths))
}
