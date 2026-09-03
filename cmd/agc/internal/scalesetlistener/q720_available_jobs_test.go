package scalesetlistener_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// TestListener_PublishesAvailableJobsOnSessionOpen covers the reading a set with nothing
// to deliver would otherwise never get. The session response carries statistics and a
// 202 does not, so without this the demand gauge stays unpublished — and "unpublished"
// and "zero queued" are the same absence to a scrape (Q720).
func TestListener_PublishesAvailableJobsOnSessionOpen(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	startListener(t, srv, fixedCapacity(0), prov, m)

	_, writes := m.availableJobs()
	assert.GreaterOrEqual(t, writes, 1, "opening the session must publish a demand reading")
}

// TestListener_PublishesAvailableJobsFromDelivery is the steady-state path: statistics
// ride on every delivered message, so the gauge tracks GitHub's queued-and-unassigned
// count while work is flowing.
//
// The one assigned job is held in flight rather than completed, which is what makes the
// reading stable enough to assert: let it complete and the whole queue drains inside a
// single poll cycle, leaving a transient no sampling interval can be relied on to catch.
// The value is one the listener could not have invented — it never sees the three jobs
// GitHub is still holding.
func TestListener_PublishesAvailableJobsFromDelivery(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	prov.setCompleteErr(true) // the worker stays in flight, pinning capacity at its one slot
	m := newCountingMetrics()
	var capacity atomicInt // opens below, once GitHub holds the whole queue
	_, ssID := startListener(t, srv, func(context.Context) int { return capacity.get() }, prov, m)

	const queued = 4
	for i := 0; i < queued; i++ {
		srv.EnqueueJob(ssID)
	}
	// Only now open the slot. A poll landing mid-enqueue would assign the first job and
	// publish the statistics riding on that message — counting only what GitHub held at
	// that instant. Nothing would republish: the slot is full so no further message is
	// delivered, and refreshDemand is gated on advertising zero capacity, which a fixed
	// non-zero CapacityFunc never does. The gauge would hold that stale value for the
	// rest of the run and `n == queued-1` would never come true (Q1005 mode B).
	capacity.set(1)

	require.Eventually(t, func() bool {
		n, writes := m.availableJobs()
		return writes > 1 && n == queued-1
	}, 5*time.Second, 10*time.Millisecond,
		"a delivered message must publish GitHub's queued-but-unassigned count")

	// The gauge above rides on statistics arrival, which handleMessage publishes ahead of
	// provisioning the job the same message assigned — so waiting on it says a message
	// landed, not that its job reached the provisioner, and on a busy box the count below
	// was read in that gap (Q1005). Wait on prov.count() itself, and hold it rather than
	// sampling once: a listener that ignored capacity provisions the rest of the batch a
	// moment later, which a single read taken as the first one lands would pass.
	require.Eventually(t, func() bool { return prov.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the job the single slot admitted must reach the provisioner")
	require.Never(t, func() bool { return prov.count() != 1 }, 500*time.Millisecond, 10*time.Millisecond,
		"capacity of one must not provision the whole queue")

	n, _ := m.availableJobs()
	assert.Equal(t, queued-1, n,
		"one job assigned under the single slot, the other three still queued at GitHub")
}
