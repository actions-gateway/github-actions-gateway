package scalesetlistener_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingProvisioner captures every Job the listener provisions and, by default,
// completes it against the fake (simulating the worker running its job to completion).
type recordingProvisioner struct {
	srv        *scalesettest.Server
	scaleSetID int

	mu          sync.Mutex
	provisioned []scalesetlistener.Job
	completeErr bool // if set, do not auto-complete (simulate a worker still running)
}

func (p *recordingProvisioner) provision(_ context.Context, job scalesetlistener.Job) error {
	p.mu.Lock()
	p.provisioned = append(p.provisioned, job)
	p.mu.Unlock()
	if !p.completeErr {
		// Simulate the worker pod: pull the job and report it completed.
		p.srv.CompleteAssignedJob(p.scaleSetID, job.JobID, "succeeded")
	}
	return nil
}

func (p *recordingProvisioner) jobIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.provisioned))
	for _, j := range p.provisioned {
		ids = append(ids, j.JobID)
	}
	return ids
}

func (p *recordingProvisioner) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.provisioned)
}

// countingMetrics records the listener's job accounting for assertions.
type countingMetrics struct {
	mu                           sync.Mutex
	assigned, provisionedC, errs int
	completed                    int
	completedResults             map[string]int
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{completedResults: map[string]int{}}
}
func (m *countingMetrics) IncJobAssigned()    { m.mu.Lock(); m.assigned++; m.mu.Unlock() }
func (m *countingMetrics) IncJobProvisioned() { m.mu.Lock(); m.provisionedC++; m.mu.Unlock() }
func (m *countingMetrics) IncProvisionError() { m.mu.Lock(); m.errs++; m.mu.Unlock() }
func (m *countingMetrics) IncJobCompleted(result string) {
	m.mu.Lock()
	m.completed++
	m.completedResults[result]++
	m.mu.Unlock()
}
func (m *countingMetrics) completedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.completed
}
func (m *countingMetrics) snapshot() (assigned, provisioned, errs int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.assigned, m.provisionedC, m.errs
}
func (m *countingMetrics) completedResult(result string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.completedResults[result]
}

// newClient builds a scaleset.Client against the fake with a stub token provider.
func newClient(t *testing.T, srv *scalesettest.Server) *scaleset.Client {
	t.Helper()
	c, err := scaleset.New(scaleset.Config{
		TokenProvider: stubTokenProvider("install-token"),
		ConfigURL:     "https://github.com/acme",
		APIBase:       srv.URL,
		HTTPClient:    srv.HTTPClient(),
		PollClient:    srv.HTTPClient(),
	})
	require.NoError(t, err)
	return c
}

type stubTokenProvider string

func (s stubTokenProvider) Token(context.Context) (string, error) { return string(s), nil }

// startListener registers a scale set on the fake, then starts a listener with the
// given capacity function and provisioner, returning the listener, the scale-set id,
// and a cancel that stops it and waits for the loop to exit.
func startListener(t *testing.T, srv *scalesettest.Server, capacity scalesetlistener.CapacityFunc, prov *recordingProvisioner, m scalesetlistener.MetricsRecorder) (*scalesetlistener.Listener, int) {
	t.Helper()
	client := newClient(t, srv)

	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:       client,
		ScaleSetName: "linux",
		OwnerName:    "acme/linux",
		Provision:    prov.provision,
		Capacity:     capacity,
		Metrics:      m,
		PollBackoff:  20 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done, err := l.Start(ctx)
	require.NoError(t, err)
	// The listener created the scale set on Start; wire its id into the provisioner.
	prov.scaleSetID = l.Status().ScaleSetID
	require.NotZero(t, prov.scaleSetID)

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("listener did not stop within 5s")
		}
	})
	return l, prov.scaleSetID
}

// fixedCapacity returns a CapacityFunc advertising a constant number of free slots.
func fixedCapacity(n int) scalesetlistener.CapacityFunc {
	return func(context.Context) int { return n }
}

// TestListener_ProvisionsOneWorkerPerAssignedJob is the core invariant: N jobs queued,
// N distinct workers provisioned — one per job, no duplicate, no dedup involved.
func TestListener_ProvisionsOneWorkerPerAssignedJob(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	_, ssID := startListener(t, srv, fixedCapacity(5), prov, m)

	const n = 5
	for i := 0; i < n; i++ {
		srv.EnqueueJob(ssID)
	}

	require.Eventually(t, func() bool { return prov.count() >= n }, 5*time.Second, 10*time.Millisecond,
		"every queued job must provision a worker")

	ids := prov.jobIDs()
	require.Len(t, ids, n, "exactly N workers provisioned (no over-provision)")
	distinct := map[string]bool{}
	for _, id := range ids {
		assert.False(t, distinct[id], "job %s provisioned more than once", id)
		distinct[id] = true
	}
	assert.Len(t, distinct, n, "each job provisioned exactly once")

	assigned, provisioned, errs := m.snapshot()
	assert.GreaterOrEqual(t, assigned, n, "each assigned job is counted")
	assert.Equal(t, n, provisioned, "one provisioned-worker metric per job")
	assert.Zero(t, errs, "no provision errors on the happy path")
}

// TestListener_CapacityGatesAssignment proves the advertised capacity bounds how many
// jobs GitHub assigns: with capacity 2 and 4 jobs queued, only 2 are provisioned until
// capacity widens.
func TestListener_CapacityGatesAssignment(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// Hold completion so assigned jobs occupy capacity (a worker still running).
	prov := &recordingProvisioner{srv: srv, completeErr: true}
	var capVal atomicInt
	capVal.set(2)

	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, nil)

	const n = 4
	for i := 0; i < n; i++ {
		srv.EnqueueJob(ssID)
	}

	require.Eventually(t, func() bool { return prov.count() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"capacity 2 must let exactly 2 jobs through")
	// Give the loop a few more polls; it must NOT exceed the advertised capacity.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, 2, prov.count(), "no more than the advertised capacity may be assigned")

	// Widen capacity; the remaining jobs are now assigned.
	capVal.set(4)
	require.Eventually(t, func() bool { return prov.count() >= n }, 5*time.Second, 10*time.Millisecond,
		"widening capacity releases the held-back jobs")
}

// TestListener_AssignedCountReconciliation checks the listener tracks the
// server-authoritative statistics.totalAssignedJobs and reports it in Status as jobs
// are assigned and then completed.
func TestListener_AssignedCountReconciliation(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv, completeErr: true} // hold jobs assigned
	l, ssID := startListener(t, srv, fixedCapacity(3), prov, nil)

	const n = 3
	for i := 0; i < n; i++ {
		srv.EnqueueJob(ssID)
	}
	require.Eventually(t, func() bool { return l.Status().AssignedJobs == n }, 5*time.Second, 10*time.Millisecond,
		"status must reflect the server-authoritative assigned count while jobs run")

	// Complete the assigned jobs; the assigned count must drain back to zero.
	for _, id := range prov.jobIDs() {
		srv.CompleteAssignedJob(ssID, id, "succeeded")
	}
	require.Eventually(t, func() bool { return l.Status().AssignedJobs == 0 }, 5*time.Second, 10*time.Millisecond,
		"completed jobs must drop out of the assigned count")
}

// TestListener_SessionRecreateReplayNoDoubleProvision proves the recovery model: after
// the session is dropped, the re-created session replays the unacked JobAssigned, and
// the listener does NOT double-provision the already-handled job.
func TestListener_SessionRecreateReplayNoDoubleProvision(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// Hold completion AND suppress delete-acks by never completing, so the JobAssigned
	// stays unacked in the queue and is a replay candidate.
	prov := &recordingProvisioner{srv: srv, completeErr: true}
	_, ssID := startListener(t, srv, fixedCapacity(2), prov, nil)

	srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return prov.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the queued job must provision once")

	// Force the queue token to expire and then drop the session out from under the
	// listener, so its next poll 404s and it re-creates the session (replaying the
	// unacked JobAssigned from cursor 0).
	srv.DropSession(ssID)

	// Let several poll cycles run against the re-created, replaying session.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, prov.count(), "a replayed JobAssigned must not double-provision an already-handled job")
}

// TestListener_GHESAcquireFlow proves the one-rule acquisition model on the GHES path:
// jobs are offered as JobAvailable, the listener claims them via AcquireJobs, and each
// resulting JobAssigned provisions exactly one worker.
func TestListener_GHESAcquireFlow(t *testing.T) {
	srv := scalesettest.New()
	srv.EnableGHESAcquireFlow()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	_, ssID := startListener(t, srv, fixedCapacity(5), prov, nil)

	const n = 3
	for i := 0; i < n; i++ {
		srv.EnqueueJob(ssID)
	}
	require.Eventually(t, func() bool { return prov.count() >= n }, 5*time.Second, 10*time.Millisecond,
		"GHES-offered jobs must be claimed and provisioned")
	assert.GreaterOrEqual(t, srv.AcquireJobsCalls(), 1, "the GHES path must claim via acquirejobs")
}

// TestListener_TransientRunnerNameConflictResolvesWithFreshName proves the Q270 fresh-name
// recovery: the base runner name 409s once (a stale registration), and the listener retries
// under a fresh suffixed name, which succeeds — the job provisions exactly once, with no
// permanent error.
func TestListener_TransientRunnerNameConflictResolvesWithFreshName(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	_, ssID := startListener(t, srv, fixedCapacity(5), prov, m)

	_, jobID := srv.EnqueueJob(ssID)
	// Fail only the exact base runner name; the first fresh-name retry (…-1) clears it.
	srv.FailJITConfigName("linux-" + jobID)

	require.Eventually(t, func() bool { return prov.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a transient runner-name conflict must resolve under a fresh name and provision the job")
	assert.Equal(t, []string{jobID}, prov.jobIDs(), "the job provisions exactly once")
	assert.GreaterOrEqual(t, srv.GenerateJITCalls(), 2, "the base-name 409 forces at least one fresh-name retry")

	_, provisioned, errs := m.snapshot()
	assert.Equal(t, 1, provisioned, "one worker provisioned")
	assert.Zero(t, errs, "a self-healed transient conflict is not counted as a provision error")
}

// TestListener_PersistentRunnerNameConflictDoesNotWedgeBatch is the core Q270 fix: a job
// whose runner name conflicts on every attempt (base AND every fresh-name retry) is skipped
// after a bounded number of tries, so the queue cursor advances past it and the other jobs
// still provision — the stuck assignment no longer wedges the batch.
func TestListener_PersistentRunnerNameConflictDoesNotWedgeBatch(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	_, ssID := startListener(t, srv, fixedCapacity(5), prov, m)

	// Enqueue the stuck job FIRST so its (lower-id) message sits ahead of the healthy one:
	// under the old no-skip behavior its unadvanced cursor would wedge the healthy job behind
	// it. Its base name AND every numeric-suffixed retry share the prefix, so it never clears.
	_, stuckJobID := srv.EnqueueJob(ssID)
	srv.FailJITConfigNamePrefix("linux-" + stuckJobID)
	_, healthyJobID := srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool {
		for _, id := range prov.jobIDs() {
			if id == healthyJobID {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond,
		"the healthy job must provision even though an earlier job is permanently stuck")

	assert.NotContains(t, prov.jobIDs(), stuckJobID, "the permanently-conflicting job never provisions")

	// A job enqueued AFTER the stuck one also provisions — proof the cursor advanced past the
	// stuck message rather than wedging behind it.
	_, laterJobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool {
		for _, id := range prov.jobIDs() {
			if id == laterJobID {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond,
		"a later job must still provision — the stuck assignment must not wedge the cursor")

	_, _, errs := m.snapshot()
	assert.GreaterOrEqual(t, errs, 1, "the skipped stuck job is counted as a provision error")
}

// TestListener_RequiredConfig covers constructor validation.
func TestListener_RequiredConfig(t *testing.T) {
	_, err := scalesetlistener.New(scalesetlistener.Config{})
	require.Error(t, err)

	_, err = scalesetlistener.New(scalesetlistener.Config{
		Client:       &scaleset.Client{},
		ScaleSetName: "x",
		Provision:    func(context.Context, scalesetlistener.Job) error { return nil },
	})
	require.Error(t, err, "Capacity is required")
}

// atomicInt is a tiny mutex-guarded int for capacity toggling in tests.
type atomicInt struct {
	mu sync.Mutex
	v  int
}

func (a *atomicInt) set(v int) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicInt) get() int  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
