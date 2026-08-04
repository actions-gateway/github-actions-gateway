package scalesetlistener_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the Q551 re-offer path. A job whose runner name will not register is acked
// past so it cannot wedge the queue cursor (Q270) — and nothing else will ever deliver
// it again, so the Listener holding it is the only thing between the conflict clearing
// and the workflow run staying queued at GitHub forever.

// fastDeferredRetry shortens the re-offer backoff so a test drives the path in test
// time; production waits 30s before the first re-offer.
func fastDeferredRetry(c *scalesetlistener.Config) { c.DeferredRetryBackoff = 20 * time.Millisecond }

// TestListener_DeferredJobIsReOfferedUntilItProvisions is the core Q551 fix: a job that
// exhausts the runner-name conflict ladder is re-offered on a backoff, so when the
// conflicting registration finally clears the job runs — where before, four attempts in
// the first few seconds were all it ever got.
func TestListener_DeferredJobIsReOfferedUntilItProvisions(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	// Zero capacity until the conflict is staged: the fake assigns a queued job as soon
	// as a poll advertises a slot, so staging afterwards races the poll loop.
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m, fastDeferredRetry)

	_, jobID := srv.EnqueueJob(ssID)
	// The base name and every suffixed retry share the prefix, so no attempt clears it.
	srv.FailJITConfigNamePrefix("linux-" + jobID)
	capVal.set(5)

	require.Eventually(t, func() bool { return m.deferredCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a job that cannot register a runner name must be held for a re-offer, not dropped")
	assert.Empty(t, prov.jobIDs(), "the job cannot provision while the conflict holds")

	// The stale registration clears (an operator, or the offline-record sweep).
	srv.ClearJITConfigConflicts()

	require.Eventually(t, func() bool { return m.provisionedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the re-offer must provision the job once the conflict clears")
	assert.Equal(t, []string{jobID}, prov.jobIDs(), "the deferred job provisions exactly once")
	assert.Eventually(t, func() bool { return m.deferredCount() == 0 }, 5*time.Second, 10*time.Millisecond,
		"a provisioned job leaves the deferred set")
}

// TestListener_DeferredJobStopsOnCompletion covers the other way a stall ends: GitHub
// gives up on the assignment (the job timed out, or the run was cancelled) and reports
// it complete. The Listener must stop re-offering a job that no longer exists — the
// completion is the only signal it gets that the assignment is gone.
func TestListener_DeferredJobStopsOnCompletion(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m, fastDeferredRetry)

	_, jobID := srv.EnqueueJob(ssID)
	srv.FailJITConfigNamePrefix("linux-" + jobID)
	capVal.set(5)

	require.Eventually(t, func() bool { return m.deferredCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the conflicting job must be held for a re-offer")

	require.True(t, srv.CompleteAssignedJob(ssID, jobID, "cancelled"),
		"the fake must be able to terminate the assignment the listener is holding")

	require.Eventually(t, func() bool { return m.deferredCount() == 0 }, 5*time.Second, 10*time.Millisecond,
		"a completed job must leave the deferred set")

	// Even with the conflict still in place, nothing is retried for it any more.
	errsBefore := m.provisionErrors()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, errsBefore, m.provisionErrors(),
		"a job GitHub has completed must not be re-offered")
	assert.Empty(t, prov.jobIDs(), "the cancelled job never provisions")
}

// TestListener_DeferredJobDoesNotBlockOtherJobs pins that the re-offer machinery keeps
// the Q270 property it was built on top of: a held job is retried between polls, and the
// jobs behind it in the queue still provision at their normal cadence.
func TestListener_DeferredJobDoesNotBlockOtherJobs(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m, fastDeferredRetry)

	_, stuckJobID := srv.EnqueueJob(ssID)
	srv.FailJITConfigNamePrefix("linux-" + stuckJobID)
	capVal.set(5)

	require.Eventually(t, func() bool { return m.deferredCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the first job must be held for a re-offer")

	const n = 3
	healthy := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		_, jobID := srv.EnqueueJob(ssID)
		healthy[jobID] = true
	}

	require.Eventually(t, func() bool { return m.provisionedCount() >= n }, 5*time.Second, 10*time.Millisecond,
		"jobs queued behind a held one must still provision")
	for _, id := range prov.jobIDs() {
		assert.True(t, healthy[id], "only the healthy jobs provisioned; got %s", id)
	}
	assert.Equal(t, 1, m.deferredCount(), "the stuck job is still held")
}
