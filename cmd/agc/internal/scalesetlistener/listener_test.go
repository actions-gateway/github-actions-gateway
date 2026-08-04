package scalesetlistener_test

import (
	"context"
	"errors"
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
	// Read under the lock: a listener started against a queue that already holds a
	// message provisions before setScaleSetID returns, so these race the setter
	// otherwise.
	ssID, complete := p.scaleSetID, !p.completeErr
	p.mu.Unlock()
	if complete {
		// Simulate the worker pod: pull the job and report it completed.
		p.srv.CompleteAssignedJob(ssID, job.JobID, "succeeded")
	}
	return nil
}

// setScaleSetID wires in the id the listener created on Start. It cannot be set at
// construction — the listener registers the scale set itself — so it is written after
// the run goroutine is already live, and therefore under the lock.
func (p *recordingProvisioner) setScaleSetID(id int) {
	p.mu.Lock()
	p.scaleSetID = id
	p.mu.Unlock()
}

// setCompleteErr switches auto-completion on and off mid-run, so a test can decide per
// job whether it concludes or stays in flight. Set it before the job is enqueued: after,
// a poll can provision in the gap.
func (p *recordingProvisioner) setCompleteErr(v bool) {
	p.mu.Lock()
	p.completeErr = v
	p.mu.Unlock()
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

// jobs returns a copy of every Job handed to Provision, so a test can assert on fields
// beyond the job id.
func (p *recordingProvisioner) jobs() []scalesetlistener.Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]scalesetlistener.Job(nil), p.provisioned...)
}

// runnerNames returns the RunnerName the listener registered for each provisioned job,
// in order — the base {scaleSet}-{jobID} name, or a suffixed fresh name on fallback.
func (p *recordingProvisioner) runnerNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.provisioned))
	for _, j := range p.provisioned {
		names = append(names, j.RunnerName)
	}
	return names
}

// gateProvisioner blocks in Provision until unblocked, so a test can observe the
// Listener's Status while the first worker is still mid-provision.
type gateProvisioner struct {
	release   chan struct{}
	closeOnce sync.Once

	mu        sync.Mutex
	enteredN  int
	completed []string
}

func (p *gateProvisioner) provision(_ context.Context, job scalesetlistener.Job) error {
	p.mu.Lock()
	p.enteredN++
	p.mu.Unlock()
	<-p.release
	p.mu.Lock()
	p.completed = append(p.completed, job.JobID)
	p.mu.Unlock()
	return nil
}

// unblock opens the gate; it is safe to call more than once.
func (p *gateProvisioner) unblock() { p.closeOnce.Do(func() { close(p.release) }) }

// entered counts the Provision calls started (including the one currently gated).
func (p *gateProvisioner) entered() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enteredN
}

// finishedJobIDs returns the jobs whose Provision call has returned.
func (p *gateProvisioner) finishedJobIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.completed...)
}

// countingMetrics records the listener's job accounting for assertions.
type countingMetrics struct {
	mu                           sync.Mutex
	assigned, provisionedC, errs int
	completed                    int
	abandoned                    int
	deferred                     map[string]int
	completedResults             map[string]int
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{completedResults: map[string]int{}, deferred: map[string]int{}}
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
func (m *countingMetrics) SetDeferredJobs(byReason map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for reason, n := range byReason {
		m.deferred[reason] = n
	}
}
func (m *countingMetrics) IncJobsAbandoned(n int) { m.mu.Lock(); m.abandoned += n; m.mu.Unlock() }

// abandonedCount returns how many assignments the listener has given up on because
// GitHub stopped counting them (Q553).
func (m *countingMetrics) abandonedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.abandoned
}

func (m *countingMetrics) completedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.completed
}

// deferredCount returns the most recently published deferred-job gauge, summed over
// every reason.
//
// A test asserting that a provisioned job LEFT the set must wait on this gauge, not
// read it after waiting on provisionedCount: retryDeferred increments the provisioned
// metric inside provisionAssigned and calls resolveDeferred — which republishes this
// gauge — only after it returns, so the two settle in that order and a bare read lands
// in the gap. Asserting a job is still held is safe; nothing is removing it.
func (m *countingMetrics) deferredCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, n := range m.deferred {
		total += n
	}
	return total
}

// deferredFor returns the most recently published deferred-job gauge for one
// DeferReason*, which is what separates expected backpressure from an anomaly.
func (m *countingMetrics) deferredFor(reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deferred[reason]
}

// provisionedCount returns the provisioned-worker metric. A test that asserts on this
// counter must also *synchronize* on it — never on the provisioner stub, which records a
// job at Provision entry while the listener counts the metric only after Provision
// returns. Waiting on the stub and then reading the metric races the gap between the two
// (Q350); waiting on the metric implies the stub already recorded, so the stub-side
// assertions stay valid.
func (m *countingMetrics) provisionedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.provisionedC
}

// provisionErrors returns the provision-error counter on its own, for a test that
// watches whether further attempts are still being made.
func (m *countingMetrics) provisionErrors() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.errs
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

// startListenerFunc registers a scale set on the fake, then starts a listener with the
// given capacity and provision functions, returning the listener and the scale-set id.
// A cleanup stops it and waits for the loop to exit.
// Optional opts mutate the Config before it is built (e.g. to wire a Cleanup hook).
func startListenerFunc(t *testing.T, srv *scalesettest.Server, capacity scalesetlistener.CapacityFunc, provision scalesetlistener.ProvisionFunc, m scalesetlistener.MetricsRecorder, opts ...func(*scalesetlistener.Config)) (*scalesetlistener.Listener, int) {
	t.Helper()
	client := newClient(t, srv)

	cfg := scalesetlistener.Config{
		Client:       client,
		ScaleSetName: "linux",
		OwnerName:    "acme/linux",
		Provision:    provision,
		Capacity:     capacity,
		Metrics:      m,
		PollBackoff:  20 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	l, err := scalesetlistener.New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done, err := l.Start(ctx)
	require.NoError(t, err)
	ssID := l.Status().ScaleSetID
	require.NotZero(t, ssID)

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("listener did not stop within 5s")
		}
	})
	return l, ssID
}

// startListener starts a listener driving the given recordingProvisioner, wiring the
// scale-set id the listener created on Start back into it.
func startListener(t *testing.T, srv *scalesettest.Server, capacity scalesetlistener.CapacityFunc, prov *recordingProvisioner, m scalesetlistener.MetricsRecorder, opts ...func(*scalesetlistener.Config)) (*scalesetlistener.Listener, int) {
	t.Helper()
	l, ssID := startListenerFunc(t, srv, capacity, prov.provision, m, opts...)
	prov.setScaleSetID(ssID)
	return l, ssID
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

	require.Eventually(t, func() bool { return m.provisionedCount() >= n }, 5*time.Second, 10*time.Millisecond,
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

// TestListener_CarriesRunIdentityToProvision covers the Q417 handoff. The assignment
// message is the only point at which this tier learns which workflow run a job belongs
// to, and the listener keeps no per-job state, so an identity dropped here is
// unrecoverable — the provisioner would stamp an empty run on the worker pod and
// eviction recovery would have nothing to re-run.
func TestListener_CarriesRunIdentityToProvision(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	_, ssID := startListener(t, srv, fixedCapacity(1), prov, newCountingMetrics())

	_, jobID := srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool { return prov.count() >= 1 }, 5*time.Second, 10*time.Millisecond)

	jobs := prov.jobs()
	require.Len(t, jobs, 1)
	got := jobs[0]
	assert.Equal(t, jobID, got.JobID)
	assert.Equal(t, scalesettest.DefaultJobOwner, got.Owner)
	assert.Equal(t, scalesettest.DefaultJobRepository, got.Repository)
	assert.NotEmpty(t, got.RunID, "the assignment's workflowRunId must reach the provisioner")
	assert.NotEqual(t, "0", got.RunID, "a zero run id addresses no run and must not be passed on as one")
}

// TestListener_ProvisionsWithoutRunIdentity is the degrade path: an assignment carrying
// no JobMessageBase identity — seeded as a raw body so the fake's own model cannot
// supply one — must still provision a worker and run the job. Only automatic eviction
// recovery is lost, and the provisioner reports that loss separately.
func TestListener_ProvisionsWithoutRunIdentity(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	srv.SeedRawMessage(`[{"messageType":"JobAssigned","jobId":"bare-job"}]`)

	prov := &recordingProvisioner{srv: srv, completeErr: true}
	startListener(t, srv, fixedCapacity(1), prov, newCountingMetrics())

	require.Eventually(t, func() bool { return prov.count() >= 1 }, 5*time.Second, 10*time.Millisecond,
		"a job with no run identity must still provision a worker")

	got := prov.jobs()[0]
	assert.Equal(t, "bare-job", got.JobID)
	assert.Empty(t, got.Owner)
	assert.Empty(t, got.Repository)
	assert.Empty(t, got.RunID, "an incomplete identity must arrive empty, never partial")
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
	queued := make([]string, 0, n)
	for i := 0; i < n; i++ {
		_, jobID := srv.EnqueueJob(ssID)
		queued = append(queued, jobID)
	}
	require.Eventually(t, func() bool { return l.Status().AssignedJobs == n }, 5*time.Second, 10*time.Millisecond,
		"status must reflect the server-authoritative assigned count while jobs run")

	// Complete every job the *server* assigned, keyed by the id EnqueueJob returned.
	// Reading the ids back off the provisioner would race: AssignedJobs reaches n the
	// moment the first message is handled (that one poll assigned all n and every
	// envelope carries the fresh statistics), while the other JobAssigned messages —
	// one per poll — have not reached the provisioner yet. Completing its short list
	// leaves the rest assigned forever and the count never drains (Q285).
	for _, id := range queued {
		require.True(t, srv.CompleteAssignedJob(ssID, id, "succeeded"),
			"job %s must be assigned server-side before it can complete", id)
	}
	require.Eventually(t, func() bool { return l.Status().AssignedJobs == 0 }, 5*time.Second, 10*time.Millisecond,
		"completed jobs must drop out of the assigned count")
}

// TestListener_StatusReportsServerCountAheadOfProvisioning pins the semantic Q285's
// flake tripped over: Status.AssignedJobs is the server-authoritative
// statistics.totalAssignedJobs read off every envelope — NOT a count of provisioned
// workers. It reaches n while only the first job is even in flight, so no test (and no
// reconciler) may infer "n workers exist" from it.
func TestListener_StatusReportsServerCountAheadOfProvisioning(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &gateProvisioner{release: make(chan struct{})}

	// Advertise no capacity until every job is queued, so a single poll assigns all n
	// at once — the shape that makes the first envelope report totalAssignedJobs == n.
	var capVal atomicInt
	l, ssID := startListenerFunc(t, srv, func(context.Context) int { return capVal.get() }, prov.provision, nil)
	// Registered after the listener-stop cleanup, so it runs before it (cleanups are
	// LIFO): the loop must leave the blocked Provision call before it can observe the
	// cancelled context, including on an early t.FailNow.
	t.Cleanup(prov.unblock)

	const n = 3
	for i := 0; i < n; i++ {
		srv.EnqueueJob(ssID)
	}
	capVal.set(n)

	// The listener is single-threaded and blocks inside the first Provision, so it can
	// never hand over a second job while gated.
	require.Eventually(t, func() bool { return prov.entered() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the first assigned job must reach the provisioner")
	require.Eventually(t, func() bool { return l.Status().AssignedJobs == n }, 5*time.Second, 10*time.Millisecond,
		"the server-authoritative assigned count must reach n on the first envelope")

	assert.Equal(t, 1, prov.entered(), "exactly one job is in flight while the provisioner is gated")
	assert.Empty(t, prov.finishedJobIDs(), "no worker has finished provisioning yet")

	// Releasing the gate lets the remaining JobAssigned messages drain through.
	prov.unblock()
	require.Eventually(t, func() bool { return len(prov.finishedJobIDs()) == n }, 5*time.Second, 10*time.Millisecond,
		"every assigned job provisions once the gate opens")
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

// TestListener_RunnerNameConflictReclaimsBaseNameByDeregister is the Q334 fix: the base
// runner name 409s because a reaped never-started worker left an offline record under it,
// and the listener recovers by deleting that stale record and re-registering under the
// SAME base name — so the job provisions once, under the base name (not a suffixed one),
// with no permanent error and no orphaned suffix records.
func TestListener_RunnerNameConflictReclaimsBaseNameByDeregister(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	// Advertise zero capacity until the conflict is staged: the fake assigns queued jobs
	// as soon as a poll advertises free slots, so configuring the conflict after the job
	// is assignable races the poll loop (the job can provision conflict-free first).
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m)

	_, jobID := srv.EnqueueJob(ssID)
	// A stale record holds the base name (a reaped never-started worker's) — deletable
	// via the REST API, so the listener reclaims the base name.
	srv.FailJITConfigName("linux-" + jobID)
	capVal.set(5)

	require.Eventually(t, func() bool { return m.provisionedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the stale record must be deleted and the base name reclaimed so the job provisions")
	assert.Equal(t, []string{jobID}, prov.jobIDs(), "the job provisions exactly once")
	assert.Equal(t, []string{"linux-" + jobID}, prov.runnerNames(),
		"the job provisions under the reclaimed base name, not a suffixed fresh name")
	assert.Equal(t, 1, srv.DeleteRunnerCalls(), "the stale record is deregistered exactly once")

	_, provisioned, errs := m.snapshot()
	assert.Equal(t, 1, provisioned, "one worker provisioned")
	assert.Zero(t, errs, "a self-healed conflict is not counted as a provision error")
}

// TestListener_RunnerNameConflictBusyRecordFallsBackToFreshName covers the Q334 busy path:
// the base name's record cannot be deleted because a live runner is still using it (422),
// so the listener falls back to the bounded Q270 fresh-name retry — provisioning once under
// a suffixed name rather than deleting a record that is legitimately in use.
func TestListener_RunnerNameConflictBusyRecordFallsBackToFreshName(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	// Advertise zero capacity until the conflict is staged (see the deregister test):
	// staging it after the job is assignable races the poll loop — the listener can hit
	// the 409 between FailJITConfigName and SetRunnerBusy and reclaim the base name.
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m)

	_, jobID := srv.EnqueueJob(ssID)
	// The base name conflicts AND its record is busy — the delete 422s, so only a fresh
	// suffixed name clears it.
	srv.FailJITConfigName("linux-" + jobID)
	srv.SetRunnerBusy("linux-" + jobID)
	capVal.set(5)

	require.Eventually(t, func() bool { return m.provisionedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a busy stale record must not block the job — the fresh-name fallback provisions it")
	assert.Equal(t, []string{jobID}, prov.jobIDs(), "the job provisions exactly once")
	assert.Equal(t, []string{"linux-" + jobID + "-1"}, prov.runnerNames(),
		"a busy record is left in place; the job provisions under a fresh suffixed name")

	_, provisioned, errs := m.snapshot()
	assert.Equal(t, 1, provisioned, "one worker provisioned")
	assert.Zero(t, errs, "the fresh-name fallback is not counted as a provision error")
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
	// Advertise zero capacity until the conflict is staged (see the deregister test):
	// staging it after the stuck job is assignable races the poll loop — the stuck job
	// can provision conflict-free first.
	var capVal atomicInt
	_, ssID := startListener(t, srv, func(context.Context) int { return capVal.get() }, prov, m)

	// Enqueue the stuck job FIRST so its (lower-id) message sits ahead of the healthy one:
	// under the old no-skip behavior its unadvanced cursor would wedge the healthy job behind
	// it. Its base name AND every numeric-suffixed retry share the prefix, so it never clears.
	_, stuckJobID := srv.EnqueueJob(ssID)
	srv.FailJITConfigNamePrefix("linux-" + stuckJobID)
	_, healthyJobID := srv.EnqueueJob(ssID)
	capVal.set(5)

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

// recordingCleanup captures the jobIDs the listener asks to reclaim, and can be made
// to fail to prove a reclaim error does not wedge the poll loop.
type recordingCleanup struct {
	mu     sync.Mutex
	jobIDs []string
	err    error
}

func (r *recordingCleanup) cleanup(_ context.Context, jobID string) error {
	r.mu.Lock()
	r.jobIDs = append(r.jobIDs, jobID)
	err := r.err
	r.mu.Unlock()
	return err
}

func (r *recordingCleanup) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.jobIDs...)
}

// TestListener_ReclaimsCompletedJobResources is the Q373 contract at the listener seam:
// the per-job JIT-config Secret the provisioner staged cannot be deleted when the pod is
// created (the pod mounts it), so the terminal JobCompleted is its reclaim point. Every
// completed job must be handed to Cleanup — otherwise a credential-bearing Secret
// accumulates per job for the RunnerSet's whole lifetime.
func TestListener_ReclaimsCompletedJobResources(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv} // auto-completes each job it provisions
	cl := &recordingCleanup{}
	_, ssID := startListener(t, srv, fixedCapacity(3), prov, nil,
		func(c *scalesetlistener.Config) { c.Cleanup = cl.cleanup })

	const n = 3
	queued := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		_, jobID := srv.EnqueueJob(ssID)
		queued[jobID] = true
	}

	require.Eventually(t, func() bool { return len(cl.seen()) >= n }, 5*time.Second, 10*time.Millisecond,
		"every completed job must have its staged worker resources reclaimed")

	for _, id := range cl.seen() {
		assert.True(t, queued[id], "reclaim was asked for a job that was never queued: %s", id)
	}
}

// TestListener_ReclaimFailureDoesNotWedgeTheLoop pins that a failed reclaim is a logged
// best-effort miss — the Secret falls back to the RunnerSet's cascade-GC — and never
// holds the queue cursor, which would redeliver the batch and stall later assignments.
func TestListener_ReclaimFailureDoesNotWedgeTheLoop(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	cl := &recordingCleanup{err: errors.New("secrets forbidden")}
	_, ssID := startListener(t, srv, fixedCapacity(3), prov, nil,
		func(c *scalesetlistener.Config) { c.Cleanup = cl.cleanup })

	srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return len(cl.seen()) >= 1 }, 5*time.Second, 10*time.Millisecond,
		"the first job must complete and attempt a reclaim")

	// A later job still provisions: the failing reclaim did not stall the loop.
	srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return prov.count() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"a reclaim failure must not stop the listener provisioning later jobs")
}
