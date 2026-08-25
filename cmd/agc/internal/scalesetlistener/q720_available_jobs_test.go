package scalesetlistener_test

import (
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
	_, ssID := startListener(t, srv, fixedCapacity(1), prov, m)

	const queued = 4
	for i := 0; i < queued; i++ {
		srv.EnqueueJob(ssID)
	}

	require.Eventually(t, func() bool {
		n, writes := m.availableJobs()
		return writes > 1 && n == queued-1
	}, 5*time.Second, 10*time.Millisecond,
		"a delivered message must publish GitHub's queued-but-unassigned count")

	n, _ := m.availableJobs()
	assert.Equal(t, queued-1, n,
		"one job assigned under the single slot, the other three still queued at GitHub")
	assert.Equal(t, 1, prov.count(), "capacity of one must not provision the whole queue")
}
