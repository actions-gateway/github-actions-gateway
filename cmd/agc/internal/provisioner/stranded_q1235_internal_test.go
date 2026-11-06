package provisioner

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q1235: the job a lost scale-set worker left without a runner (Q1195) is recovered on
// the disruption path too, and a walk declined while a Running worker's start was
// unrecorded is re-checked once, after the start lands.

func q1235Jobs() []OrphanedWorker {
	return []OrphanedWorker{
		inFlight("job-a", "myorg/repo-a", "111"),
		inFlight("job-b", "myorg/repo-b", "222"),
		inFlight("job-c", "myorg/repo-c", "333"),
		inFlight("job-d", "myorg/repo-d", "444"),
	}
}

// TestRecoverEvictedScaleSetWorkers_Q1235_RecoversTheStrandedJob: the worker minted for
// job-a is evicted before it ran, while job-a runs on job-b's worker, so job-b has no
// runner. Its run is force-cancelled; run 111 is live and is neither cancelled nor
// re-run.
func TestRecoverEvictedScaleSetWorkers_Q1235_RecoversTheStrandedJob(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted,
		evicted(mintedWorker("job-a", "myorg/repo-a", "111")),
		startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"))
	target := &inFlightTarget{stubTarget: f.target, jobs: q1235Jobs()}

	done, err := f.p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	forceCancel, rerun := f.calls.snapshot()
	assert.Equal(t, []string{"/repos/myorg/repo-b/actions/runs/222/force-cancel"}, forceCancel)
	assert.Empty(t, rerun, "job-b never ran, so it waits for capacity rather than rerun-failed-jobs")
	e, ok := f.p.peekAbandonedRerun(abandonedRerunKey{owner: target.Key(), runID: "222"})
	require.True(t, ok, "the stranded run waits for the abandoned arm's capacity-gated re-run")
	assert.Equal(t, scaleSetPodName("gpu", "job-a"), e.disruption.key, "its budget slot is keyed by the lost worker")
}

// TestStrandedJob_Q1235_SparesARunWithAJobRunning: job-a and job-b are legs of one
// matrix run 111, and job-a runs on job-b's worker. The walk names job-b, but a
// force-cancel ends the whole run and job-a with it, so nothing is cancelled on either
// seam.
func TestStrandedJob_Q1235_SparesARunWithAJobRunning(t *testing.T) {
	matrix := []OrphanedWorker{
		inFlight("job-a", "myorg/repo-a", "111"),
		inFlight("job-b", "myorg/repo-a", "111"),
	}
	for _, seam := range []string{"abandoned", "disruption"} {
		t.Run(seam, func(t *testing.T) {
			ctx := context.Background()
			pods := []*corev1.Pod{startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a")}
			if seam == "disruption" {
				pods = append(pods, evicted(mintedWorker("job-a", "myorg/repo-a", "111")))
			}
			f := newAbandonedScaleSetFixture(t, http.StatusAccepted, pods...)
			target := &inFlightTarget{stubTarget: f.target, jobs: matrix}

			if seam == "abandoned" {
				<-f.p.RecoverAbandonedScaleSetWorker(ctx, target, reapedPendingWorker(scaleSetPodName("gpu", "job-a")))
			} else {
				done, err := f.p.RecoverEvictedScaleSetWorkers(ctx, target)
				require.NoError(t, err)
				<-done
			}

			forceCancel, rerun := f.calls.snapshot()
			assert.Empty(t, forceCancel, "cancelling run 111 would end job-a, which is running")
			assert.Empty(t, rerun)
			assert.Empty(t, f.p.pendingStrandedRecheckKeys())
		})
	}
}

// TestRecoverEvictedScaleSetWorkers_Q1235_AWorkerThatRanIsNotWalked is the control: a
// container that ran may have taken a job before its start was stamped, which may be
// the very job a walk would name, so the worker is spared as before.
func TestRecoverEvictedScaleSetWorkers_Q1235_AWorkerThatRanIsNotWalked(t *testing.T) {
	ctx := context.Background()
	victim := evicted(mintedWorker("job-a", "myorg/repo-a", "111"))
	victim.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runner",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			FinishedAt: metav1.NewTime(time.Unix(1_700_000_000, 0)),
		}},
	}}
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted, victim,
		startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"))
	target := &inFlightTarget{stubTarget: f.target, jobs: q1235Jobs()}

	done, err := f.p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	forceCancel, rerun := f.calls.snapshot()
	assert.Empty(t, forceCancel)
	assert.Empty(t, rerun)
	assert.Empty(t, f.p.pendingStrandedRecheckKeys())
}

// TestRecoverEvictedScaleSetWorkers_Q1235_AnIdleWorkerSparesTheWalk: the evicted worker
// was placed, so an idle worker beside it can take a job, as for the abandoned path's
// node-lost shape. job-d's worker is Pending, which the walk itself would not decline
// for, so only the idle check spares it. A Running worker with no start recorded is
// idle too, so the disruption path never defers a walk for a re-check.
func TestRecoverEvictedScaleSetWorkers_Q1235_AnIdleWorkerSparesTheWalk(t *testing.T) {
	ctx := context.Background()
	idle := mintedWorker("job-d", "myorg/repo-d", "444")
	idle.Status.Phase = corev1.PodPending
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted,
		evicted(mintedWorker("job-a", "myorg/repo-a", "111")),
		startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"),
		idle)
	target := &inFlightTarget{stubTarget: f.target, jobs: q1235Jobs()}

	done, err := f.p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	forceCancel, rerun := f.calls.snapshot()
	assert.Empty(t, forceCancel)
	assert.Empty(t, rerun)
	assert.Empty(t, f.p.pendingStrandedRecheckKeys())
}

// TestStrandedRecheck_Q1235_FiresOnceTheStartLands: job-d's worker is Running with no
// start recorded, so the reaped worker's walk declines. Once job-d's start is stamped
// the next sweep walks again and recovers whichever job is then left without a runner,
// and a later sweep does not recover it twice.
func TestStrandedRecheck_Q1235_FiresOnceTheStartLands(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startedD string
		want     string
	}{
		// job-d's runner took a job outside the walk, so job-b is still stranded.
		{"the start was another job", "job-x", "/repos/myorg/repo-b/actions/runs/222/force-cancel"},
		// job-d's runner took job-b, which leaves job-d's own job without a runner.
		{"the start was the stranded job", "job-b", "/repos/myorg/repo-d/actions/runs/444/force-cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newAbandonedScaleSetFixture(t, http.StatusAccepted,
				startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"),
				mintedWorker("job-d", "myorg/repo-d", "444"))
			target := &inFlightTarget{stubTarget: f.target, jobs: q1235Jobs()}

			<-f.p.RecoverAbandonedScaleSetWorker(ctx, target, reapedPendingWorker(scaleSetPodName("gpu", "job-a")))
			forceCancel, _ := f.calls.snapshot()
			require.Empty(t, forceCancel, "job-d's runner may hold job-b, so nothing is named yet")
			require.Len(t, f.p.pendingStrandedRecheckKeys(), 1)

			sweep(f.p)
			forceCancel, _ = f.calls.snapshot()
			require.Empty(t, forceCancel, "the start is still unrecorded")
			require.Len(t, f.p.pendingStrandedRecheckKeys(), 1, "an undecided re-check keeps waiting")

			stampStarted(ctx, t, f.fc, scaleSetPodName("gpu", "job-d"), tc.startedD)
			f.clock = f.clock.Add(time.Minute)
			sweep(f.p)
			sweep(f.p)

			forceCancel, _ = f.calls.snapshot()
			assert.Equal(t, []string{tc.want}, forceCancel, "recovered once, on the first sweep after the start")
			assert.Empty(t, f.p.pendingStrandedRecheckKeys())
			assert.Equal(t, 1.0, testutil.ToFloat64(
				f.m.AbandonedRunForceCancels.WithLabelValues("team-a", "gpu", "cancelled")))
		})
	}
}

// TestStrandedRecheck_Q1235_ExpiresWithTheWindow: a start that stays unrecorded for the
// whole window drops the re-check, so a start landing after it recovers nothing.
func TestStrandedRecheck_Q1235_ExpiresWithTheWindow(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted,
		startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"),
		mintedWorker("job-d", "myorg/repo-d", "444"))
	target := &inFlightTarget{stubTarget: f.target, jobs: q1235Jobs()}

	<-f.p.RecoverAbandonedScaleSetWorker(ctx, target, reapedPendingWorker(scaleSetPodName("gpu", "job-a")))
	require.Len(t, f.p.pendingStrandedRecheckKeys(), 1)

	f.clock = f.clock.Add(strandedRecheckWindow)
	sweep(f.p)
	assert.Empty(t, f.p.pendingStrandedRecheckKeys())

	stampStarted(ctx, t, f.fc, scaleSetPodName("gpu", "job-d"), "job-x")
	sweep(f.p)
	forceCancel, _ := f.calls.snapshot()
	assert.Empty(t, forceCancel)
}

func sweep(p *Provisioner) {
	for _, ch := range p.sweepStrandedRechecks(context.Background()) {
		<-ch
	}
}

// stampStarted records jobID as the job name's runner started, as the listener does on
// its JobStarted.
func stampStarted(ctx context.Context, t *testing.T, c client.Client, name, jobID string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, &pod))
	pod.Annotations[AnnotationStartedJobID] = jobID
	require.NoError(t, c.Update(ctx, &pod))
}
