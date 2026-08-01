package scalesetlistener_test

import (
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the Q575 ordering. A worker pod's JIT-config Secret is reclaimed by the
// job's terminal JobCompleted, so provisioning a worker for a job already reported
// complete builds a pod that can never mount its Secret — it sits Pending until the
// pendingPodDeadline reaper collects it ten minutes later, which is what the
// v1.3.0-rc.3 dogfood gate recorded (eight pods at once).
//
// The batch is the lever. handleMessage used to provision a batch's assignments before
// handling its completions, so a batch carrying both messages for one job — a run
// cancelled between two polls, and every job in the queue when a re-created session
// replays from cursor 0 — provisioned a doomed worker deterministically, not on a race.

// seedAssignedThenCompleted seeds one message carrying a JobAssigned and a JobCompleted
// for the same job, in that wire order: the batch shape the gate captured. SeedMessage is
// the only way to build it, because the model completes a job only after a worker pulls
// it. Seeding is pre-registration, so it must run before the listener creates its scale
// set. The stub mints a JIT config for any runner name, so nothing but the fix stands
// between this batch and a provisioned worker.
func seedAssignedThenCompleted(srv *scalesettest.Server, jobID string) {
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: jobID},
		{MessageType: scaleset.MessageTypeJobCompleted, JobID: jobID, Result: "cancelled"},
	})
}

// TestListener_DoesNotProvisionAJobCompletedInTheSameBatch is the core Q575 fix at the
// listener seam. The completion in the batch says the job is over and its Secret is
// reclaimed, so no worker may be built for it — the pod would have nothing to run and
// nothing to mount.
func TestListener_DoesNotProvisionAJobCompletedInTheSameBatch(t *testing.T) {
	srv := newQuickPollServer(t)

	seedAssignedThenCompleted(srv, "job-cancelled-mid-batch")

	prov := &recordingProvisioner{srv: srv, completeErr: true}
	cl := &recordingCleanup{}
	m := newCountingMetrics()
	startListener(t, srv, fixedCapacity(5), prov, m,
		func(c *scalesetlistener.Config) { c.Cleanup = cl.cleanup })

	require.Eventually(t, func() bool { return len(cl.seen()) == 1 }, 5*time.Second, 10*time.Millisecond,
		"the completion in the batch must be handled")

	// The assignment is acked past, not held: a deferred job would be re-offered forever
	// for a job that is over.
	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, prov.jobIDs(),
		"no worker may be provisioned for a job whose completion is in the same batch")
	assert.Zero(t, m.deferredCount(), "nor may the assignment be held for a re-offer")
	assert.Zero(t, m.provisionErrors(), "skipping is not a provisioning failure")
}

// TestListener_DoesNotProvisionAnAssignmentReplayedAfterItsCompletion covers the
// ordering the same-batch rule cannot reach: the completion arrives in one batch and the
// assignment in a later one. The listener has never provisioned this job, so the Q553
// `abandoned` set and the `provisioned` set are both empty — only the completion record
// stands between the replayed assignment and a worker for a job that is over.
func TestListener_DoesNotProvisionAnAssignmentReplayedAfterItsCompletion(t *testing.T) {
	srv := newQuickPollServer(t)

	const jobID = "job-completed-then-replayed"
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobCompleted, JobID: jobID, Result: "succeeded"},
	})
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: jobID},
	})

	prov := &recordingProvisioner{srv: srv, completeErr: true}
	cl := &recordingCleanup{}
	m := newCountingMetrics()
	startListener(t, srv, fixedCapacity(5), prov, m,
		func(c *scalesetlistener.Config) { c.Cleanup = cl.cleanup })

	require.Eventually(t, func() bool { return len(cl.seen()) == 1 }, 5*time.Second, 10*time.Millisecond,
		"the completion must be handled first")

	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, prov.jobIDs(),
		"an assignment replayed after its own completion must not provision a worker")
	assert.Zero(t, m.deferredCount(), "nor may it enter the deferred set")
}

// TestListener_StillProvisionsAliveJobAlongsideAnUnrelatedCompletion is the negative
// half, and the one that keeps the guard from swallowing live work: a completion for a
// DIFFERENT job must not stop this job's worker being built. Without it, "handle
// completions first" could pass by provisioning nothing at all.
func TestListener_StillProvisionsAliveJobAlongsideAnUnrelatedCompletion(t *testing.T) {
	srv := newQuickPollServer(t)

	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobCompleted, JobID: "some-other-job", Result: "succeeded"},
	})

	prov := &recordingProvisioner{srv: srv, completeErr: true}
	cl := &recordingCleanup{}
	m := newCountingMetrics()
	_, ssID := startListener(t, srv, fixedCapacity(5), prov, m,
		func(c *scalesetlistener.Config) { c.Cleanup = cl.cleanup })

	require.Eventually(t, func() bool { return len(cl.seen()) == 1 }, 5*time.Second, 10*time.Millisecond,
		"the unrelated completion is handled")
	assert.Equal(t, []string{"some-other-job"}, cl.seen())

	// A genuinely live job still provisions, and its own Secret is left alone.
	_, liveJobID := srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return prov.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a job with no completion of its own must still provision a worker")
	assert.Equal(t, []string{liveJobID}, prov.jobIDs())
	assert.Equal(t, []string{"some-other-job"}, cl.seen(),
		"the live job's Secret must not be reclaimed while its worker runs")
}
