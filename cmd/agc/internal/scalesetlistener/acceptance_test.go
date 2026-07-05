package scalesetlistener_test

import (
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListener_FanoutFreeAcceptance is the scale-set twin of the Q260 acceptance test
// TestAGC_Q260_FanoutCompletionReconciles: N concurrent jobs against the scalesettest
// fake, and EVERY one concludes completed — with ZERO dedup involved.
//
// The payoff of Option E is that this holds by construction, not by reconciliation.
// The classic tier needed fan-out completion reconciliation because GitHub fanned one
// logical job out to N sibling sessions (shared planID) that all acquired it, leaving
// N−1 dangling deliveries to cancel the job at the unstarted-job timeout (Q260/Q224).
// Under the scale-set protocol each job is enqueued once in the scale set's single
// serialized queue and claimed by this listener's single session: there are no sibling
// deliveries, no planID to dedup, and nothing to reconcile. This test is the offline
// proof that the fan-out starvation class is gone — the analog of the Q260 twin, but
// with no dedup machinery in the path at all.
func TestListener_FanoutFreeAcceptance(t *testing.T) {
	const n = 8

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// The provisioner auto-completes each job against the fake, standing in for the
	// worker pod that pulls its one job through its own session and reports completion.
	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	_, ssID := startListener(t, srv, fixedCapacity(n), prov, m)

	for i := 0; i < n; i++ {
		srv.EnqueueJob(ssID)
	}

	// Every one of the N jobs concludes completed — the terminal signal the classic
	// protocol never delivered to the AGC (§2b-6), here observed for all N.
	require.Eventually(t, func() bool { return m.completedCount() >= n }, 10*time.Second, 10*time.Millisecond,
		"every one of the N concurrent jobs must conclude completed")

	// Zero dedup: N distinct jobs, each provisioned exactly one worker. There are no
	// sibling deliveries by construction, so no delivery is ever deduped or abandoned.
	ids := prov.jobIDs()
	require.Len(t, ids, n, "exactly N workers provisioned — one per job, none abandoned")
	distinct := map[string]bool{}
	for _, id := range ids {
		assert.False(t, distinct[id], "job %s provisioned more than once — a sibling delivery leaked in", id)
		distinct[id] = true
	}
	assert.Len(t, distinct, n, "each of the N jobs provisioned exactly once (no fan-out, no dedup)")

	// Every job concluded with the worker's real result.
	assert.Equal(t, n, m.completedResult("succeeded"), "every job concluded succeeded")

	// The queue drained: no job is left assigned-but-unfinished.
	require.Eventually(t, func() bool { return srv.AssignedJobCount(ssID) == 0 }, 5*time.Second, 10*time.Millisecond,
		"no delivery dangles after every job completes (the Q260 wedge cannot occur)")
}
