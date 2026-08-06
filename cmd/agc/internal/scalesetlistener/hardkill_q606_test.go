package scalesetlistener_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Q606, the residual Q603 left. A hard kill — SIGKILL at grace expiry, OOM,
// node loss — between concluding a job and its message's DELETE landing loses the
// conclusion with the process, and the next listener replays the assignment and
// provisions a worker for a job that is over. No ordering closes that (the conclusion
// is in memory, the ack is at GitHub), so the conclusion is persisted through a
// GuardStore, written ahead of every delete: once a DELETE is even attempted, the
// state it rested on survives the process.
//
// The queue's DELETE being refused is the hard-kill window held open: the message is
// certainly still in the queue when the process goes away, exactly as it is when the
// kill lands before the DELETE. The store shared across the two listeners is the etcd
// that survives it.

// fakeGuardStore is an in-memory GuardStore shared across a test's listener
// generations.
type fakeGuardStore struct {
	mu      sync.Mutex
	state   scalesetlistener.GuardState
	saves   int
	loadErr error
	saveErr error
}

func (f *fakeGuardStore) Load(context.Context) (scalesetlistener.GuardState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.loadErr
}

func (f *fakeGuardStore) Save(_ context.Context, state scalesetlistener.GuardState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.state = state
	f.saves++
	return nil
}

// saved returns the last successfully saved state.
func (f *fakeGuardStore) saved() scalesetlistener.GuardState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func withGuards(f *fakeGuardStore) func(*scalesetlistener.Config) {
	return func(c *scalesetlistener.Config) { c.Guards = f }
}

// TestListener_HardKillBetweenAbandonAndDeleteDoesNotReprovision is the row's scenario:
// the Q603 exit flush cannot run (every DELETE is refused, before and through the
// stop), so only the persisted conclusion separates the restart from re-provisioning
// the abandoned assignment. Waiting for a delete *attempt* before the stop is the
// write-ahead ordering doing the synchronizing: an attempt exists only after the save
// succeeded.
func TestListener_HardKillBetweenAbandonAndDeleteDoesNotReprovision(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.FailDeleteMessage(http.StatusInternalServerError)

	store := &fakeGuardStore{}
	m := newCountingMetrics()
	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, m, fastAssignmentCheck, withGuards(store))

	jobID := stallJob(t, srv, ssID, m, 1)
	require.True(t, srv.DropAssignedJob(ssID, jobID), "the job must be droppable server-side")
	require.Eventually(t, func() bool { return m.abandonedCount() == 1 }, 10*time.Second,
		20*time.Millisecond, "the listener must give up on the job GitHub no longer holds")
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 10*time.Second,
		20*time.Millisecond, "a delete attempt means the write-ahead save already landed")
	stopFirst()

	require.Equal(t, []string{jobID}, store.saved().Abandoned,
		"the store must hold the conclusion the kill would otherwise take")

	// The queue accepts deletes again and the stall is cleared, so a replayed
	// assignment has every chance to provision — the restart must decline on the
	// loaded guard alone.
	srv.FailDeleteMessage(0)
	srv.ClearJITConfigConflicts()

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), fastAssignmentCheck, withGuards(store))

	assert.Never(t, func() bool { return second.count() > 0 }, 2*time.Second, 20*time.Millisecond,
		"a restarted listener provisioned a worker for a job its predecessor had concluded")

	// The replayed message is settled off the loaded guard, deleted, and the guard
	// retired with it — the persisted set drains instead of accreting.
	require.Eventually(t, func() bool { return len(store.saved().Abandoned) == 0 }, 10*time.Second,
		20*time.Millisecond, "the loaded guard must be retired once its message is deleted")
}

// TestListener_HardKillBetweenCompleteAndDeleteDoesNotReprovision is the completed
// half: the job ran and completed, its messages are still queued when the process goes
// away, and the restart must not provision a worker for it. (The completion metric may
// still count once more across the restart — delivery order can retire the loaded
// guard with the assignment's message before the completion replays — which is the
// pre-Q606 at-least-once behaviour, deliberately not pinned here.)
func TestListener_HardKillBetweenCompleteAndDeleteDoesNotReprovision(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.FailDeleteMessage(http.StatusInternalServerError)

	store := &fakeGuardStore{}
	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, newCountingMetrics(), withGuards(store))

	_, jobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return first.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the first listener must provision the job")
	require.Eventually(t, func() bool { return srv.AssignedJobCount(ssID) == 0 }, 5*time.Second,
		10*time.Millisecond, "the job must conclude at the backend")
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 5*time.Second,
		10*time.Millisecond, "a delete attempt means the write-ahead save already landed")
	stopFirst()

	require.Equal(t, []string{jobID}, store.saved().Completed)

	srv.FailDeleteMessage(0)

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), withGuards(store))

	assert.Never(t, func() bool { return second.count() > 0 }, 2*time.Second, 20*time.Millisecond,
		"a restarted listener provisioned a worker for a job that already ran and completed")

	require.Eventually(t, func() bool { return len(store.saved().Completed) == 0 }, 10*time.Second,
		20*time.Millisecond, "the loaded guard must be retired once its messages are deleted")
}

// TestListener_HardKillWithTheSaveRefusedStillReplays removes the mechanism. With the
// store refusing every save, no delete may be issued (the write-ahead ordering), the
// messages stay queued, and the restart re-provisions exactly as it did before the fix
// — so the two tests above are carried by the persisted state and by nothing else.
func TestListener_HardKillWithTheSaveRefusedStillReplays(t *testing.T) {
	srv := newQuickPollServer(t)

	store := &fakeGuardStore{saveErr: errors.New("etcd said no")}
	m := newCountingMetrics()
	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, m, withGuards(store))

	_, jobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return m.completedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the job must run to completion")

	// The write-ahead half on its own: the queue would accept the delete, but the
	// conclusion cannot be made durable, so the listener must not issue it.
	assert.Never(t, func() bool { return deleteAttempts(srv) > 0 }, time.Second, 20*time.Millisecond,
		"no delete may be issued while the conclusion that authorises it cannot be persisted")
	stopFirst()

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), withGuards(store))

	require.Eventually(t, func() bool { return second.count() == 1 }, 10*time.Second,
		20*time.Millisecond, "with nothing persisted the concluded job must replay and re-provision")
	assert.Equal(t, []string{jobID}, second.jobIDs())
}

// TestListener_SweepsLoadedGuardsWhoseMessagesAreGone is the retirement rule that
// keeps the store bounded. An entry whose message delete landed but whose store
// retirement was lost to the kill has no message left to replay it; by the first empty
// poll the queue is drained, so whatever no replayed message assigned is swept and the
// next save garbage-collects it.
func TestListener_SweepsLoadedGuardsWhoseMessagesAreGone(t *testing.T) {
	srv := newQuickPollServer(t)

	store := &fakeGuardStore{state: scalesetlistener.GuardState{
		Completed: []string{"ghost-completed"},
		Abandoned: []string{"ghost-abandoned"},
	}}
	prov := &recordingProvisioner{srv: srv}
	l, _ := startListener(t, srv, fixedCapacity(5), prov, newCountingMetrics(), withGuards(store))

	require.Eventually(t, func() bool {
		st := store.saved()
		return len(st.Completed) == 0 && len(st.Abandoned) == 0
	}, 5*time.Second, 20*time.Millisecond,
		"loaded guards no queued message can replay must be swept from the store")

	_, completed, abandoned := l.GuardedJobIDs()
	assert.Empty(t, completed)
	assert.Empty(t, abandoned)
}

// TestListener_StartFailsWhenTheGuardsCannotBeLoaded pins the failure direction:
// polling without the persisted guards would silently reopen the replay window, so an
// unreadable store keeps the listener down — before a session exists — and the
// reconciler retries.
func TestListener_StartFailsWhenTheGuardsCannotBeLoaded(t *testing.T) {
	srv := newQuickPollServer(t)

	store := &fakeGuardStore{loadErr: errors.New("apiserver said no")}
	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:       newClient(t, srv),
		ScaleSetName: "linux",
		OwnerName:    "acme/linux",
		Provision:    (&recordingProvisioner{srv: srv}).provision,
		Capacity:     fixedCapacity(5),
		Guards:       store,
	})
	require.NoError(t, err)

	_, err = l.Start(context.Background())
	require.ErrorContains(t, err, "load concluded-job guards")
	for _, call := range srv.Calls() {
		assert.NotContains(t, call, "create-session",
			"a Start that fails on the guard load must not have opened a session")
	}
}
