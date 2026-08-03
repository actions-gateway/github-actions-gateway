package scalesetlistener_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Q597: the provisioned/completed/abandoned replay guards are bounded by the
// messages still in the queue, not by the jobs the listener has handled.
//
// Q583 made the bound available. The guards answer exactly one delivery — a redelivered
// JobAssigned — and a redelivery can only carry a message still in the queue. The cursor
// never advances past a message the listener is not holding for delete, so an entry is
// dead once every held message carrying an assignment for its job has been deleted. What
// makes that safe rather than merely plausible is the other half of Q583: a message is
// deleted only once every job it names has concluded, so the delete cannot outrun the
// work.
//
// "Carrying an assignment", not "naming the job", is load-bearing, and the repo already
// has the test that says so:
// TestListener_DoesNotProvisionAnAssignmentReplayedAfterItsCompletion hands the listener
// a completion whose assignment arrives afterwards, and a rule that retired on the
// completion's delete would meet that assignment with an empty completed set and build a
// worker for a job that is over (Q575).
//
// The tests below pin the population deliberately. A guard set that shrank says nothing
// on its own — the question is always which entry went and which one had to stay.

// TestListener_RetiresGuardsOfJobsWhoseMessagesAreGone is the shrink. Three jobs are
// handled; two run to completion and one stays in flight, and the guards must end up
// naming exactly the third.
func TestListener_RetiresGuardsOfJobsWhoseMessagesAreGone(t *testing.T) {
	srv := newQuickPollServer(t)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	l, ssID := startListener(t, srv, fixedCapacity(5), prov, m)

	// Two that conclude: assignment and completion both delete, so both guards go.
	_, done1 := srv.EnqueueJob(ssID)
	_, done2 := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return m.completedCount() == 2 }, 10*time.Second,
		10*time.Millisecond, "both jobs must run to completion")

	// One that does not. Switched before the enqueue, so no poll can provision it under
	// the auto-completing provisioner.
	prov.setCompleteErr(true)
	_, inFlight := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return prov.count() == 3 }, 10*time.Second,
		10*time.Millisecond, "the third job must be provisioned")

	require.Eventually(t, func() bool {
		provisioned, completed, abandoned := l.GuardedJobIDs()
		return len(provisioned) == 1 && len(completed) == 0 && len(abandoned) == 0
	}, 10*time.Second, 20*time.Millisecond,
		"the guards must retire the two concluded jobs once their messages are deleted")

	provisioned, completed, abandoned := l.GuardedJobIDs()
	assert.Equal(t, []string{inFlight}, provisioned,
		"only the job still owed a completion may keep its provisioned guard")
	assert.Empty(t, completed, "a completed job whose messages are gone keeps no guard")
	assert.Empty(t, abandoned)
	assert.NotContains(t, provisioned, done1)
	assert.NotContains(t, provisioned, done2)

	// The in-flight job's assignment is the only message left, which is what keeps its
	// guard: the pair is one fact, not two.
	assert.Equal(t, 1, l.PendingMessageCount(),
		"the in-flight job's assignment must still be held for delete")
}

// TestListener_KeepsGuardsWhileTheDeleteIsRefused removes the mechanism at the queue.
// With every DELETE refused nothing is retired, and the same three jobs leave all three
// guards populated — so the shrink above is the delete's doing and not the loop's.
func TestListener_KeepsGuardsWhileTheDeleteIsRefused(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.FailDeleteMessage(http.StatusInternalServerError)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	l, ssID := startListener(t, srv, fixedCapacity(5), prov, m)

	_, done1 := srv.EnqueueJob(ssID)
	_, done2 := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return m.completedCount() == 2 }, 10*time.Second,
		10*time.Millisecond, "both jobs must run to completion")
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 10*time.Second,
		10*time.Millisecond, "the listener must have attempted the deletes it is being refused")

	assert.Never(t, func() bool {
		provisioned, completed, _ := l.GuardedJobIDs()
		return len(provisioned) < 2 || len(completed) < 2
	}, 2*time.Second, 20*time.Millisecond,
		"a guard was retired for a job whose messages are still in the queue")

	provisioned, completed, _ := l.GuardedJobIDs()
	assert.Equal(t, []string{done1, done2}, provisioned)
	assert.Equal(t, []string{done1, done2}, completed)
}

// TestListener_KeepsAGuardWhoseAssignmentIsStillQueued is the case that decides how the
// bookkeeping is shaped. A batched assignment names two jobs; one completes in a later
// message. That completion's own message settles and deletes straight away, but the batch
// stays queued waiting on the other job — and it still carries the completed job's
// JobAssigned, so a session drop would replay it.
//
// This is the case that makes the "no other held message assigns it" check load-bearing
// rather than defensive: drop that check and the completion's delete takes the guard with
// it, leaving the queued assignment to build a worker for a job that is over (Q575).
func TestListener_KeepsAGuardWhoseAssignmentIsStillQueued(t *testing.T) {
	srv := newQuickPollServer(t)
	// One envelope naming two assignments. The stub's own model emits one job per
	// message, so a batch — which the protocol permits and the listener handles — has to
	// be seeded.
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: "batch-over"},
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: "batch-running"},
	})
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobCompleted, JobID: "batch-over", Result: "succeeded"},
	})

	// The seeded jobs have no server-side existence, so nothing completes them but the
	// seeded message itself.
	prov := &recordingProvisioner{srv: srv, completeErr: true}
	m := newCountingMetrics()
	l, _ := startListener(t, srv, fixedCapacity(5), prov, m)

	require.Eventually(t, func() bool { return prov.count() == 2 }, 10*time.Second,
		10*time.Millisecond, "both batched assignments must be provisioned")
	require.Eventually(t, func() bool { return m.completedCount() == 1 }, 10*time.Second,
		10*time.Millisecond, "the completion must be handled")
	// The completion's own message is settled the moment it is handled, so its delete is
	// the one that runs the retirement — with the batch still held behind it.
	require.Eventually(t, func() bool { return l.PendingMessageCount() == 1 }, 10*time.Second,
		20*time.Millisecond, "the completion's message must be deleted, leaving the batch held")

	assert.Never(t, func() bool {
		provisioned, completed, _ := l.GuardedJobIDs()
		return !slices.Contains(provisioned, "batch-over") || !slices.Contains(completed, "batch-over")
	}, 2*time.Second, 20*time.Millisecond,
		"the completed job's guards were retired while a queued message still names its assignment")

	provisioned, completed, _ := l.GuardedJobIDs()
	assert.Equal(t, []string{"batch-over", "batch-running"}, provisioned)
	assert.Equal(t, []string{"batch-over"}, completed)
}

// TestListener_RetiresTheAbandonedGuardWithItsMessage covers the third set. Q553's
// give-up marks a job gone and settles its assignment; once that assignment is deleted
// no session can deliver it again, so the guard has nothing left to answer.
func TestListener_RetiresTheAbandonedGuardWithItsMessage(t *testing.T) {
	srv := newQuickPollServer(t)

	prov := &recordingProvisioner{srv: srv}
	m := newCountingMetrics()
	l, ssID := startListener(t, srv, fixedCapacity(5), prov, m, fastAssignmentCheck)

	jobID := stallJob(t, srv, ssID, m, 1)
	require.True(t, srv.DropAssignedJob(ssID, jobID), "the job must be droppable server-side")
	require.Eventually(t, func() bool { return m.abandonedCount() == 1 }, 10*time.Second,
		20*time.Millisecond, "the listener must give up on the job GitHub no longer holds")

	require.Eventually(t, func() bool { return l.PendingMessageCount() == 0 }, 10*time.Second,
		20*time.Millisecond, "abandoning must release the assignment's message")

	provisioned, completed, abandoned := l.GuardedJobIDs()
	assert.Empty(t, abandoned, "the abandoned guard must go with the message that could replay it")
	assert.Empty(t, provisioned, "the job was never provisioned")
	assert.Empty(t, completed)
}
