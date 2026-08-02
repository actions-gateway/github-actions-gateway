package scalesetlistener_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Q603, the seam Q583's fix left. Acking is two halves: settle concludes a
// job in memory, and a later flushDeletes removes its message from the queue. A process
// that stops in between leaves the message behind, and the next one reads it with
// provisioned/completed/abandoned all empty — the Q583 defect, arriving through the fix.
//
// The row came off Q602's flake: the Q583 regression test stopped on the abandoned
// counter, which rises at settle, and sometimes took the listener away before the
// delete. Q602 taught the test to wait. Production cannot.

// parkInPoll raises the stub's poll window and waits until the listener is inside a
// poll held open by it, where it cannot reach its own flushDeletes. That is what makes
// "stopped between concluding a job and deleting its message" a state a test can
// arrange rather than race for.
//
// The park shows up as the poll count going quiet: the stub records a poll on arrival
// and only then holds it, and under the short window the loop polls every
// minPollInterval, so two samples a few hundred milliseconds apart reading the same
// count mean the current poll is being held rather than that the loop is between two.
func parkInPoll(t *testing.T, srv *scalesettest.Server) {
	t.Helper()
	srv.SetPollTimeout(30 * time.Second)
	last := -1
	require.Eventually(t, func() bool {
		n := pollCalls(srv)
		parked := n > 0 && n == last
		last = n
		return parked
	}, 10*time.Second, 300*time.Millisecond,
		"the listener must enter a poll held open by the raised window")
}

// TestListener_StopIssuesTheDeleteForAConcludedJob is the narrow assertion the fix
// rests on: a listener going away must issue the delete half of the ack for anything
// that concluded since its last flush. Before the fix the loop returned on cancellation
// with only the session delete deferred, so that attempt did not exist at all.
//
// The queue refuses the delete throughout, which is what makes the state deterministic
// rather than raced for: the message is guaranteed to be settled and still pending when
// the process stops, so the count can only move if the exit path issues the call.
func TestListener_StopIssuesTheDeleteForAConcludedJob(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.FailDeleteMessage(http.StatusInternalServerError)

	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, newCountingMetrics())

	srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return first.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the listener must provision the job")
	require.Eventually(t, func() bool { return srv.AssignedJobCount(ssID) == 0 }, 5*time.Second,
		10*time.Millisecond, "the job must conclude at the backend")
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 5*time.Second,
		10*time.Millisecond, "the completion must settle its message and attempt the delete")

	parkInPoll(t, srv)
	before := deleteAttempts(srv)
	stopFirst()

	assert.Greater(t, deleteAttempts(srv), before,
		"a listener stopped holding a settled message must issue its delete on the way out")
}

// TestListener_StopBetweenAbandonAndDeleteDoesNotReprovision is the row's scenario end
// to end. Q553 gives up on an assignment GitHub no longer holds and settles its
// message; the delete follows separately. Stopping in that window used to leave the
// assignment in the queue, where a restart — abandoned set empty — provisions the
// worker with nothing to run that Q553 exists to prevent.
func TestListener_StopBetweenAbandonAndDeleteDoesNotReprovision(t *testing.T) {
	srv := newQuickPollServer(t)
	// Refused for now, so the abandon settles the message but cannot remove it: the
	// listener reaches the stop still owing the delete.
	srv.FailDeleteMessage(http.StatusInternalServerError)

	m := newCountingMetrics()
	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, m, fastAssignmentCheck)

	jobID := stallJob(t, srv, ssID, m, 1)
	require.True(t, srv.DropAssignedJob(ssID, jobID), "the job must be droppable server-side")
	require.Eventually(t, func() bool { return m.abandonedCount() == 1 }, 10*time.Second,
		20*time.Millisecond, "the listener must give up on the job GitHub no longer holds")
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 10*time.Second,
		20*time.Millisecond, "abandoning must settle the message and attempt its delete")

	// Park the loop, then let the queue accept deletes again. The running listener
	// cannot use the recovered endpoint from inside a held poll, so the only path left
	// to the delete is the one the stop takes.
	parkInPoll(t, srv)
	srv.FailDeleteMessage(0)
	stopFirst()

	// Clear the runner-name conflict that stalled it, and shorten the window again, so
	// a replayed assignment has every chance to provision. Without this the test would
	// pass whether or not the stop flushed.
	srv.ClearJITConfigConflicts()
	srv.SetPollTimeout(20 * time.Millisecond)

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), fastAssignmentCheck)

	assert.Never(t, func() bool { return second.count() > 0 }, 2*time.Second, 20*time.Millisecond,
		"a listener restarted after a stop mid-abandon provisioned a worker for a job GitHub had dropped")
}

// TestListener_StopWithTheDeleteRefusedStillReplays removes the mechanism the test
// above depends on. With the queue refusing the delete through the stop as well, the
// exit flush runs and fails, the message stays, and the restart re-provisions exactly
// as it did before the fix. Without this the passing test above is only evidence that
// something changed, not that the exit delete is what carries it.
func TestListener_StopWithTheDeleteRefusedStillReplays(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.FailDeleteMessage(http.StatusInternalServerError)

	m := newCountingMetrics()
	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, m, fastAssignmentCheck)

	jobID := stallJob(t, srv, ssID, m, 1)
	require.True(t, srv.DropAssignedJob(ssID, jobID), "the job must be droppable server-side")
	require.Eventually(t, func() bool { return m.abandonedCount() == 1 }, 10*time.Second,
		20*time.Millisecond, "the listener must give up on the job GitHub no longer holds")
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 10*time.Second,
		20*time.Millisecond, "abandoning must settle the message and attempt its delete")

	parkInPoll(t, srv)
	stopFirst()

	srv.ClearJITConfigConflicts()
	srv.SetPollTimeout(20 * time.Millisecond)

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), fastAssignmentCheck)

	require.Eventually(t, func() bool { return second.count() == 1 }, 10*time.Second,
		20*time.Millisecond, "with the delete still refused the abandoned assignment must replay")
	assert.Equal(t, []string{jobID}, second.jobIDs())
}
