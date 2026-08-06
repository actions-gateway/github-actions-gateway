package scalesetlistener_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Q689, the third window in the restart-replay family. Q583 bounds replay with
// the delete half of the ack, Q603 issues that delete on the way out, and Q606 persists
// the conclusion the delete rests on. All three start from a conclusion the Listener has
// already read. This one does not: the job concluded at GitHub, its JobCompleted is in
// the queue, and the process goes away before the poll loop reads it. Nothing the
// Listener holds says the job is over, so its assignment replays and a restart builds a
// worker for a job that already ran.
//
// It needs no hard kill. A graceful stop is enough, which is what makes it wider than
// the residual Q606 closed.

// blockingProvisioner holds the poll loop inside Provision until the Listener's context
// is cancelled, which is how these tests put the loop where it cannot read the queue.
//
// The loop is single-goroutine: anything it is doing between two polls is a window in
// which a completion published at GitHub goes unread. Provisioning is the widest one to
// arrange and the easiest to observe, but it is not the only one — retryDeferred walks
// the runner-name ladder against the network, and reconcileDeferred spends a session
// refresh. Blocking here stands in for all of them.
type blockingProvisioner struct {
	entered chan struct{}
	once    sync.Once
}

func newBlockingProvisioner() *blockingProvisioner {
	return &blockingProvisioner{entered: make(chan struct{})}
}

func (p *blockingProvisioner) provision(ctx context.Context, _ scalesetlistener.Job) error {
	p.once.Do(func() { close(p.entered) })
	<-ctx.Done()
	// A nil return is the case that matters: the worker was created, so the job is
	// provisioned and its assignment is acked past and held for a delete that the
	// unread completion is the only thing standing between.
	return nil
}

// awaitEntry waits until the loop is inside Provision.
func (p *blockingProvisioner) awaitEntry(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the listener did not reach Provision within 10s")
	}
}

func withProvision(fn scalesetlistener.ProvisionFunc) func(*scalesetlistener.Config) {
	return func(c *scalesetlistener.Config) { c.Provision = fn }
}

// startBlockedListener starts a listener whose Provision holds the poll loop. The suite's
// helper is typed on recordingProvisioner and the option replaces its provision, so the
// one handed over here never runs.
func startBlockedListener(t *testing.T, srv *scalesettest.Server, store *fakeGuardStore,
	block *blockingProvisioner) (stop func(), ssID int) {
	t.Helper()
	return startStoppableListener(t, srv, &recordingProvisioner{srv: srv}, newCountingMetrics(),
		withGuards(store), withProvision(block.provision))
}

// TestListener_StopBeforeReadingACompletionDoesNotReprovision is the row's scenario. The
// job concludes at GitHub while the loop is held out of its poll, so the Listener stops
// holding an assignment it believes is still in flight — and every guard the earlier
// fixes rely on says exactly that. Only reading the completion the queue is already
// holding can tell it otherwise.
func TestListener_StopBeforeReadingACompletionDoesNotReprovision(t *testing.T) {
	srv := newQuickPollServer(t)

	store := &fakeGuardStore{}
	block := newBlockingProvisioner()
	stopFirst, ssID := startBlockedListener(t, srv, store, block)

	_, jobID := srv.EnqueueJob(ssID)
	block.awaitEntry(t)

	// The worker ran the job and reported it. GitHub appends the JobCompleted; the loop
	// is inside Provision and cannot read it.
	require.True(t, srv.CompleteAssignedJob(ssID, jobID, "succeeded"),
		"the job must conclude at the backend while the loop is held out of its poll")
	require.Zero(t, srv.AssignedJobCount(ssID), "the backend must no longer hold the job")

	stopFirst()

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), withGuards(store))

	assert.Never(t, func() bool { return second.count() > 0 }, 2*time.Second, 20*time.Millisecond,
		"a restarted listener provisioned a worker for a job that concluded before the stop")
}

// TestListener_StopWithTheQueueUnreadableStillReplays removes the mechanism. Everything
// is as it is above except that the queue answers the exit read 429, so the completion
// cannot be reached and the restart re-provisions exactly as it did before the fix.
// Without this the test above is only evidence that something changed.
func TestListener_StopWithTheQueueUnreadableStillReplays(t *testing.T) {
	srv := newQuickPollServer(t)

	store := &fakeGuardStore{}
	block := newBlockingProvisioner()
	stopFirst, ssID := startBlockedListener(t, srv, store, block)

	_, jobID := srv.EnqueueJob(ssID)
	block.awaitEntry(t)
	require.True(t, srv.CompleteAssignedJob(ssID, jobID, "succeeded"),
		"the job must conclude at the backend while the loop is held out of its poll")

	srv.SetRateLimitPolls(true)
	stopFirst()
	srv.SetRateLimitPolls(false)

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), withGuards(store))

	require.Eventually(t, func() bool { return second.count() == 1 }, 10*time.Second,
		20*time.Millisecond, "with the completion unreadable at the stop the assignment must replay")
	assert.Equal(t, []string{jobID}, second.jobIDs())
}

// TestListener_StopStillReplaysAJobThatIsRunning is the other direction, and the one the
// fix must not break. The same stop, in the same place, with the job still assigned at
// GitHub: nothing has concluded, so the assignment must survive the stop and the restart
// must re-read it. A drain that settled on anything weaker than a JobCompleted would
// delete this message and lose the run.
func TestListener_StopStillReplaysAJobThatIsRunning(t *testing.T) {
	srv := newQuickPollServer(t)

	store := &fakeGuardStore{}
	block := newBlockingProvisioner()
	stopFirst, ssID := startBlockedListener(t, srv, store, block)

	_, jobID := srv.EnqueueJob(ssID)
	block.awaitEntry(t)
	require.Equal(t, 1, srv.AssignedJobCount(ssID), "the job must still be assigned at the backend")

	stopFirst()

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), withGuards(store))

	require.Eventually(t, func() bool { return second.count() == 1 }, 10*time.Second,
		20*time.Millisecond, "a job still assigned at GitHub must replay to the restarted listener")
	assert.Equal(t, []string{jobID}, second.jobIDs())
}
