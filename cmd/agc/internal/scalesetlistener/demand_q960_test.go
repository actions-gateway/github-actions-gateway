package scalesetlistener_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// TestListener_RefreshesDemandGaugeWhileWithholding covers the reading a set advertising
// no capacity would otherwise never take (Q960). GitHub assigns it nothing, so no message
// is delivered and no statistics ride in; before the paced refresh the gauge held the zero
// it published at session open for the whole run, which is a stale zero in exactly the
// state the gauge exists to explain.
//
// The value asserted is one the listener could not have invented: it is never told about
// these jobs, and its own accounting counts none.
func TestListener_RefreshesDemandGaugeWhileWithholding(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	_, ssID := startListener(t, srv, fixedCapacity(0), prov, m, func(c *scalesetlistener.Config) {
		c.DemandRefreshInterval = 20 * time.Millisecond
	})

	const queued = 5
	for i := 0; i < queued; i++ {
		srv.EnqueueJob(ssID)
	}

	require.Eventually(t, func() bool {
		n, writes := m.availableJobs()
		return writes > 1 && n == queued
	}, 10*time.Second, 20*time.Millisecond,
		"a set advertising no capacity must refresh its demand reading on the empty-poll path")

	assert.Zero(t, prov.count(), "withholding capacity must still provision nothing")
}

// TestListener_DemandRefreshOnlyWhileWithholding pins the capacity gate that keeps the
// refresh affordable (Q960): a set with a free slot is delivered a message whenever GitHub
// has anything to assign, so it already reads statistics and must not spend a session
// refresh per empty poll on top. Drop the gate and this goes red.
func TestListener_DemandRefreshOnlyWhileWithholding(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	// A free slot and an empty queue: every poll comes back 202, so the only thing that
	// could call RefreshSession here is the demand path.
	startListener(t, srv, fixedCapacity(1), prov, m, func(c *scalesetlistener.Config) {
		c.DemandRefreshInterval = 20 * time.Millisecond
	})

	// Long enough for several of the stub's one-second empty polls.
	time.Sleep(3 * time.Second)
	assert.Zero(t, srv.RefreshSessionCalls(),
		"a set advertising free capacity must not refresh statistics on the empty-poll path")
}

// TestListener_DemandRefreshIsPaced pins the other half of the budget argument (Q960): the
// refresh is paced off the last reading, not taken per empty poll. At the shipped interval
// a set that has just opened its session spends nothing, however many times it polls.
func TestListener_DemandRefreshIsPaced(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	// No DemandRefreshInterval override — the default five minutes applies.
	_, ssID := startListener(t, srv, fixedCapacity(0), prov, m)
	srv.EnqueueJob(ssID)

	time.Sleep(3 * time.Second)
	assert.Zero(t, srv.RefreshSessionCalls(),
		"the session-open reading is inside the interval, so no poll may refresh it")
	_, writes := m.availableJobs()
	assert.Equal(t, 1, writes, "only the session-open reading may have been published")
}
