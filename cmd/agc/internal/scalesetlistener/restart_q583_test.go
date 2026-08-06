package scalesetlistener_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Q583: an AGC restart replays the scale-set queue from cursor 0 and
// provisions workers for jobs that concluded long ago.
//
// The two halves of the defect are measured in different places. The backend half is
// live evidence — Investigation G (2026-08-01) staged a job, acked both its messages
// by cursor without deleting them, and watched a new session polling from cursor 0
// receive them back; scalesetstub models exactly that. The listener half is here: its
// provisioned/completed/abandoned guards are process-scoped, so a second Listener
// against the same queue has nothing left that would recognise the replayed job.
//
// A second Listener over one stub is the whole restart: the queue log is scale-set
// scoped, so it survives the first listener's session going away, which is what makes
// this reproducible with no cluster and no credentials.

// startStoppableListener starts a listener the test can stop mid-run, returning a stop
// function that cancels it and waits for the loop to exit. The suite's usual helper
// cancels only at test end, which cannot express "the process went away and another
// one started against the same queue".
func startStoppableListener(t *testing.T, srv *scalesettest.Server, prov *recordingProvisioner,
	m scalesetlistener.MetricsRecorder, opts ...func(*scalesetlistener.Config)) (stop func(), ssID int) {
	t.Helper()
	cfg := scalesetlistener.Config{
		Client:       newClient(t, srv),
		ScaleSetName: "linux",
		OwnerName:    "acme/linux",
		Provision:    prov.provision,
		Capacity:     fixedCapacity(5),
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
	ssID = l.Status().ScaleSetID
	require.NotZero(t, ssID)
	prov.setScaleSetID(ssID)

	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("listener did not stop within 5s")
		}
	}
	t.Cleanup(stop)
	return stop, ssID
}

// TestListener_RestartDoesNotReprovisionAConcludedJob is the Q583 fix. A job that ran
// and completed under one listener must not get a second worker when a fresh listener
// takes over the same scale set — which is what every AGC restart is.
//
// Before the fix this fails on the second listener's count: the replayed JobAssigned
// meets an empty provisioned set, an empty completed set, and an empty abandoned set,
// so nothing stops it.
func TestListener_RestartDoesNotReprovisionAConcludedJob(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, newCountingMetrics())

	_, jobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return first.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the first listener must provision the job")
	require.Equal(t, []string{jobID}, first.jobIDs())

	// The provisioner auto-completes, so let the completion reach the listener before the
	// process goes away — a job still in flight is a different case, and one where replay
	// is the recovery path rather than the bug. The backend draining is not that signal:
	// CompleteAssignedJob runs inside Provision, so it lands while the listener is still
	// inside handleMessage for the assignment, and the JobCompleted it appends is only
	// read a poll later. Waiting on the delete attempt waits for that read, because the
	// assignment's message names an unsettled job until it happens (Q685).
	require.Eventually(t, func() bool { return srv.AssignedJobCount(ssID) == 0 }, 5*time.Second,
		10*time.Millisecond, "the job must conclude at the backend before the restart")
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 5*time.Second,
		10*time.Millisecond, "the completion must reach the listener and settle its message")

	stopFirst()

	// A new process: new listener, new provisioner, all guards empty.
	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics())

	// Give the replay every chance to happen. The second listener polls from cursor 0
	// immediately, so a re-provision lands well inside this window.
	assert.Never(t, func() bool { return second.count() > 0 }, 2*time.Second, 20*time.Millisecond,
		"a restarted listener provisioned a worker for a job that already ran and completed")
}

// TestListener_RestartStillReadsAnUnprovisionedJob is the other side of the same fix,
// and the reason a message is deleted only once every job in it is concluded rather
// than as soon as its cursor advances. Queue replay is how a restart recovers work the
// previous process accepted and never provisioned — Q551's deferred jobs among them —
// so the fix must not prune anything still owed a worker.
func TestListener_RestartStillReadsAnUnprovisionedJob(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// completeErr leaves the job assigned: provisioned, never concluded.
	first := &recordingProvisioner{srv: srv, completeErr: true}
	stopFirst, ssID := startStoppableListener(t, srv, first, newCountingMetrics())

	_, jobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return first.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the first listener must provision the job")

	stopFirst()

	second := &recordingProvisioner{srv: srv, completeErr: true}
	startStoppableListener(t, srv, second, newCountingMetrics())

	require.Eventually(t, func() bool { return second.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a restarted listener must still re-read a job that never concluded")
	assert.Equal(t, []string{jobID}, second.jobIDs())
}

// TestListener_UndeletedMessageStillReplays settles the causation claim the fix rests
// on by removing the mechanism: with the queue's DELETE refused, the fix cannot take
// effect, and the restart re-provisions exactly as it did before. Without this, the
// first test is only evidence that SOMETHING changed.
func TestListener_UndeletedMessageStillReplays(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.FailDeleteMessage(http.StatusNotFound)

	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, newCountingMetrics())

	_, jobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return first.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the first listener must provision the job")
	require.Eventually(t, func() bool { return srv.AssignedJobCount(ssID) == 0 }, 5*time.Second,
		10*time.Millisecond, "the job must conclude at the backend before the restart")
	// The same precondition as the test above, so the pair differs only in whether the
	// queue honours the delete: without it this could replay because the completion never
	// reached the listener, which proves nothing about the refusal (Q685).
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 5*time.Second,
		10*time.Millisecond, "the completion must reach the listener and settle its message")
	stopFirst()

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics())

	require.Eventually(t, func() bool { return second.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"with the delete refused, the concluded job must still replay and re-provision")
	assert.Equal(t, []string{jobID}, second.jobIDs())
}

// TestListener_RetriesADeleteThatFailed asserts a delete rejected once is not lost.
// The listener has already advanced its cursor past the message, so nothing else will
// bring it back — if the retry did not happen the queue would keep the message forever
// and the restart replay would return with it.
func TestListener_RetriesADeleteThatFailed(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.FailDeleteMessage(http.StatusInternalServerError)

	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, newCountingMetrics())

	srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return first.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the first listener must provision the job")
	require.Eventually(t, func() bool { return srv.AssignedJobCount(ssID) == 0 }, 5*time.Second,
		10*time.Millisecond, "the job must conclude at the backend")
	// Let the first delete attempt fail before the endpoint recovers.
	refused := 0
	require.Eventually(t, func() bool {
		refused = deleteAttempts(srv)
		return refused > 0
	}, 5*time.Second, 10*time.Millisecond, "the listener must have attempted a delete")
	srv.FailDeleteMessage(0)

	// The retry runs on a later poll cycle; wait for one before taking the process away.
	require.Eventually(t, func() bool { return deleteAttempts(srv) > refused }, 5*time.Second,
		20*time.Millisecond, "the failed delete must be retried on a later poll cycle")
	stopFirst()

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics())
	assert.Never(t, func() bool { return second.count() > 0 }, 1*time.Second, 20*time.Millisecond,
		"once the retry succeeded the concluded job must not replay")
}

// TestListener_FailedSecretReclaimKeepsItsMessage protects the Q373/Q575 backstop the
// delete could otherwise remove. A completion whose Secret reclaim failed is not
// finished, so its message stays in the queue and a restart retries the reclaim —
// which was the whole reason replay-from-0 was worth having.
func TestListener_FailedSecretReclaimKeepsItsMessage(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	failing := &recordingCleanup{err: errors.New("api server said no")}
	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, newCountingMetrics(),
		func(c *scalesetlistener.Config) { c.Cleanup = failing.cleanup })

	_, jobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return len(failing.seen()) == 1 }, 5*time.Second,
		10*time.Millisecond, "the first listener must attempt the reclaim")
	stopFirst()

	// The reclaim now works, as it would once the apiserver recovered.
	recovering := &recordingCleanup{}
	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(),
		func(c *scalesetlistener.Config) { c.Cleanup = recovering.cleanup })

	require.Eventually(t, func() bool { return len(recovering.seen()) == 1 }, 5*time.Second,
		10*time.Millisecond, "the completion must replay so the failed reclaim is retried")
	assert.Equal(t, []string{jobID}, recovering.seen())
}

// deleteAttempts counts the message-DELETE calls the stub has seen, successful or not.
// The stub records the call before it decides the response, which is what lets a test
// wait for a retry rather than for an outcome.
func deleteAttempts(srv *scalesettest.Server) int {
	n := 0
	for _, call := range srv.Calls() {
		if strings.HasPrefix(call, "delete-message") {
			n++
		}
	}
	return n
}

// TestListener_AbandonedJobDoesNotSurviveARestart closes the loop Q553 left open, and
// is why abandoning a job settles its message. The give-up guard is process-scoped, so
// without the delete the very assignment the guard acted on replays to the next
// session — and a restarted listener, with an empty abandoned set, provisions the
// worker with nothing to run that Q553 exists to prevent.
func TestListener_AbandonedJobDoesNotSurviveARestart(t *testing.T) {
	srv := newQuickPollServer(t)

	m := newCountingMetrics()
	first := &recordingProvisioner{srv: srv}
	stopFirst, ssID := startStoppableListener(t, srv, first, m, fastAssignmentCheck)

	// A job the listener holds but cannot provision, then ended server-side with no
	// JobCompleted — the shape the rc.3 gate recorded.
	jobID := stallJob(t, srv, ssID, m, 1)
	require.True(t, srv.DropAssignedJob(ssID, jobID), "the job must be droppable server-side")
	require.Eventually(t, func() bool { return m.abandonedCount() == 1 }, 10*time.Second,
		20*time.Millisecond, "the listener must give up on the job GitHub no longer holds")
	// The counter rises at settle, which only marks the job concluded in memory; the
	// delete half of the ack is issued by the next flushDeletes cycle. Stopping on the
	// counter alone can therefore cut the listener off before the message is released,
	// which replays it for a reason the fix is not responsible for. Wait for the delete
	// itself — the effect this test's invariant rests on.
	require.Eventually(t, func() bool { return deleteAttempts(srv) > 0 }, 10*time.Second,
		20*time.Millisecond, "abandoning the job must release its message before the restart")
	stopFirst()

	// Clear the runner-name conflict that stalled it. Without this the replayed
	// assignment could not provision for a reason unrelated to the fix, and the test
	// would pass whether or not abandoning settles the message.
	srv.ClearJITConfigConflicts()

	second := &recordingProvisioner{srv: srv}
	startStoppableListener(t, srv, second, newCountingMetrics(), fastAssignmentCheck)

	assert.Never(t, func() bool { return second.count() > 0 }, 2*time.Second, 20*time.Millisecond,
		"a restarted listener provisioned a worker for a job the previous one gave up on")
}
