package provisioner

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Q844: recovering a scale-set worker that was already gone when the AGC came back.
//
// Every arm of the disruption scan reads its discriminator off a pod that still exists,
// and preemption and drain both DELETE their victim — so an AGC down across that window
// issues no re-run at all. These tests pin the replacement record (the listener's
// persisted in-flight set) and, just as importantly, the three ways it must NOT fire:
// on a worker whose pod is still there, more than once per process, and on a scan that
// could not read the cluster.

// orphan builds an in-flight record as the listener persists one, for the job whose
// worker pod recoveryFixture's helpers name.
func orphan(jobID string) OrphanedWorker {
	return OrphanedWorker{JobID: jobID, Owner: "myorg", Repository: "myrepo", RunID: "4242"}
}

// TestRecoverOrphanedScaleSetWorkers_RerunsAWorkerThatIsGone is the headline behaviour:
// the run behind a worker pod that no longer exists is re-run, which before Q844 needed
// a human because the pod was the only record of the disruption.
func TestRecoverOrphanedScaleSetWorkers_RerunsAWorkerThatIsGone(t *testing.T) {
	ctx := context.Background()
	// No pods: the preempted worker was torn down while this AGC was not running.
	p, target, m, rerunCount, paths := recoveryFixture(t)

	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{orphan("job1")})
	require.NoError(t, err)
	<-done

	require.Equal(t, int64(1), rerunCount.Load(), "the run whose worker vanished must be re-run exactly once")
	select {
	case path := <-paths:
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs/4242/rerun-failed-jobs", path)
	default:
		t.Fatal("rerun API path was not recorded")
	}

	// Reported under its own cause: which of preemption, drain or node loss took the
	// worker is exactly what the missing pod no longer says, so the counter must not
	// claim one of them.
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseVanished)))
	assert.Equal(t, float64(0),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCausePreemption)))
	assert.Contains(t, target.events, "OrphanedWorkerRecovered",
		"a lost worker must be visible in kubectl describe, not only in the log")
}

// TestRecoverOrphanedScaleSetWorkers_LeavesALiveWorkerAlone is the discriminator itself.
// A job still running, and a job that genuinely failed and is sitting in PodFailed until
// the reaper takes it, both still HAVE their pod — and re-running either would be the
// retry loop the design refuses.
func TestRecoverOrphanedScaleSetWorkers_LeavesALiveWorkerAlone(t *testing.T) {
	ctx := context.Background()
	running := scaleSetWorkerPod(scaleSetPodName("gpu", "job1"), identityAnnotations())
	running.Status.Phase = corev1.PodRunning
	failed := scaleSetWorkerPod(scaleSetPodName("gpu", "job2"), identityAnnotations())
	failed.Status.Phase = corev1.PodFailed
	p, target, m, rerunCount, _ := recoveryFixture(t, running, failed)

	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target,
		[]OrphanedWorker{orphan("job1"), orphan("job2")})
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(), "a worker whose pod is still there is the live path's to handle")
	assert.Equal(t, float64(0),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseVanished)))
	assert.NotContains(t, target.events, "OrphanedWorkerRecovered")
}

// TestRecoverOrphanedScaleSetWorkers_RunsOncePerProcess is what makes it safe to call
// from a reconcile that runs continuously. The stored record is not retired by this scan
// — only the listener retires it, on the job's conclusion — so without the claim every
// reconcile until then would spend another slot of the run's retry budget.
func TestRecoverOrphanedScaleSetWorkers_RunsOncePerProcess(t *testing.T) {
	ctx := context.Background()
	p, target, _, rerunCount, _ := recoveryFixture(t)

	for range 3 {
		done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{orphan("job1")})
		require.NoError(t, err)
		<-done
	}

	assert.Equal(t, int64(1), rerunCount.Load(), "the startup scan must not repeat on later reconciles")
}

// TestRecoverOrphanedScaleSetWorkers_UnreadableClusterRecoversNothingAndRetries is the
// asymmetry this scan carries and the disruption scan does not: its verdict is driven by
// a pod NOT being listed, so a List that failed must recover nothing at all — and must
// leave the question open rather than count as the one scan this process gets.
func TestRecoverOrphanedScaleSetWorkers_UnreadableClusterRecoversNothingAndRetries(t *testing.T) {
	ctx := context.Background()
	listErr := errors.New("apiserver unavailable")
	failing := true
	p, target, _, rerunCount, _ := recoveryFixtureWith(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if failing {
				return listErr
			}
			return c.List(ctx, list, opts...)
		},
	})

	_, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{orphan("job1")})
	require.ErrorIs(t, err, listErr)
	require.Equal(t, int64(0), rerunCount.Load(), "an unreadable cluster is not evidence that a worker is gone")

	// The claim was released, so the next reconcile takes the scan the failure cost.
	failing = false
	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{orphan("job1")})
	require.NoError(t, err)
	<-done
	assert.Equal(t, int64(1), rerunCount.Load(), "a failed scan must be retried, not silently skipped for the process")
}

// TestRecoverOrphanedScaleSetWorkers_SpendsTheSharedRetryBudget pins that this cause
// draws on the same per-run budget as every other one (Q106). A run alternately
// disrupted and orphaned must not get two budgets.
func TestRecoverOrphanedScaleSetWorkers_SpendsTheSharedRetryBudget(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, _ := recoveryFixture(t)
	target.spec.MaxEvictionRetries = 1

	// Spend the run's only slot on an ordinary eviction first.
	spent := p.handleEviction(ctx, target, "myorg", "myrepo", "4242", p.logForKey(target.key),
		1, 0, evictionTierScaleSet, recoveryCauseEviction)
	<-spent
	require.Equal(t, int64(1), rerunCount.Load())

	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{orphan("job1")})
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(1), rerunCount.Load(), "the orphan recovery must not get a budget of its own")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseVanished)))
}

// TestRecoverOrphanedScaleSetWorkers_EmptySetTakesNoScan keeps the common path free: a
// set that never lost a worker must not spend its one claim, or a later restart's
// records would go unread.
func TestRecoverOrphanedScaleSetWorkers_EmptySetTakesNoScan(t *testing.T) {
	ctx := context.Background()
	p, target, _, rerunCount, _ := recoveryFixture(t)

	done, err := p.RecoverOrphanedScaleSetWorkers(ctx, target, nil)
	require.NoError(t, err)
	<-done
	require.Equal(t, int64(0), rerunCount.Load())

	done, err = p.RecoverOrphanedScaleSetWorkers(ctx, target, []OrphanedWorker{orphan("job1")})
	require.NoError(t, err)
	<-done
	assert.Equal(t, int64(1), rerunCount.Load(), "an empty set must leave the scan still to take")
}
