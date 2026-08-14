package scalesetlistener_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Q844, the listener's half of the scale-set tier's last restart-safety gap.
//
// A preempted or drained worker is readable only while it terminates, so an AGC down for
// that window loses the only record of which run to re-run — the pod. The replacement is
// a record of the runs this listener currently has workers for, written into the guard
// ConfigMap it already persists ahead of every message delete. The owning reconciler
// reads it back at startup and re-runs whatever no longer has a pod.
//
// These tests pin the record's whole lifecycle from the listener's side. What makes them
// meaningful rather than tautological is the pairing: the record must be there while the
// job is in flight AND gone once it concludes, because a record that never retires
// re-runs finished work on the next restart.

// inFlightRunIDs returns the run ids the store currently holds an in-flight record for.
func inFlightRunIDs(store *fakeGuardStore) []string {
	var ids []string
	for _, j := range store.saved().InFlight {
		ids = append(ids, j.RunID)
	}
	return ids
}

// TestListener_PersistsTheRunBehindAWorkerItBuilt is the record being written. Without
// it a preempted worker's run is unrecoverable once the pod is gone, because the pod was
// the only place this tier ever recorded which run the job belonged to.
func TestListener_PersistsTheRunBehindAWorkerItBuilt(t *testing.T) {
	srv := newQuickPollServer(t)
	store := &fakeGuardStore{}
	prov := &recordingProvisioner{srv: srv}
	// The worker is still running: no completion, which is the state a disruption
	// interrupts.
	prov.setCompleteErr(true)
	_, ssID := startStoppableListener(t, srv, prov, newCountingMetrics(), withGuards(store))

	job := srv.Enqueue(ssID)

	require.Eventually(t, func() bool { return len(store.saved().InFlight) == 1 }, 10*time.Second,
		20*time.Millisecond, "the run behind a live worker must be persisted, not held in memory")

	rec := store.saved().InFlight[0]
	assert.Equal(t, job.JobID, rec.JobID)
	assert.Equal(t, job.OwnerName, rec.Owner)
	assert.Equal(t, job.RepositoryName, rec.Repository)
	assert.Equal(t, strconv.FormatInt(job.WorkflowRunID, 10), rec.RunID,
		"the record must name the run rerun-failed-jobs addresses, off the assignment itself")
	assert.False(t, rec.ProvisionedAt.IsZero(), "the record needs an age for the accretion bound")
}

// TestListener_RetiresTheRecordWhenTheJobConcludes is the other half, and the one that
// keeps the mechanism from re-running work that finished. A job that reaches a reportable
// end is owed nothing, whatever later becomes of its pod.
func TestListener_RetiresTheRecordWhenTheJobConcludes(t *testing.T) {
	srv := newQuickPollServer(t)
	store := &fakeGuardStore{}
	// recordingProvisioner's default is to complete the job it provisioned, which is
	// the ordinary path: worker runs, job reports, queue delivers the JobCompleted.
	prov := &recordingProvisioner{srv: srv}
	_, ssID := startStoppableListener(t, srv, prov, newCountingMetrics(), withGuards(store))

	srv.Enqueue(ssID)

	require.Eventually(t, func() bool { return len(prov.jobs()) == 1 }, 10*time.Second,
		20*time.Millisecond, "the job must be provisioned before its conclusion can retire anything")
	assert.Eventually(t, func() bool { return len(store.saved().InFlight) == 0 }, 10*time.Second,
		20*time.Millisecond, "a concluded job must leave no record behind for a restart to act on")
}

// TestListener_TheRecordSurvivesTheProcess is the property the whole design exists for:
// the record has to outlive the AGC, because the window it covers is exactly the one in
// which no AGC is running. The stop here is the restart, and what the reconciler reads on
// the way back up is whatever the store still holds.
func TestListener_TheRecordSurvivesTheProcess(t *testing.T) {
	srv := newQuickPollServer(t)
	store := &fakeGuardStore{}
	prov := &recordingProvisioner{srv: srv}
	prov.setCompleteErr(true)
	stopFirst, ssID := startStoppableListener(t, srv, prov, newCountingMetrics(), withGuards(store))

	job := srv.Enqueue(ssID)
	require.Eventually(t, func() bool { return len(store.saved().InFlight) == 1 }, 10*time.Second,
		20*time.Millisecond, "the record must be persisted before the process goes away")
	stopFirst()

	require.Equal(t, []string{strconv.FormatInt(job.WorkflowRunID, 10)}, inFlightRunIDs(store),
		"the record a preempted worker's recovery depends on must survive the process that wrote it")

	// The next generation rebuilds the record rather than adopting it. A job still
	// running holds its assignment in the queue, so the new session re-reads it,
	// re-provisions idempotently, and records it afresh — which is what lets the loaded
	// copy be dropped, and is what stops a second restart re-running a run the first
	// one's reconciler already recovered.
	second := &recordingProvisioner{srv: srv}
	second.setCompleteErr(true)
	startStoppableListener(t, srv, second, newCountingMetrics(), withGuards(store))
	require.Eventually(t, func() bool { return len(second.jobs()) == 1 }, 10*time.Second,
		20*time.Millisecond, "the still-running job's assignment must replay to the new session")
	assert.Eventually(t, func() bool {
		return len(inFlightRunIDs(store)) == 1 && inFlightRunIDs(store)[0] == strconv.FormatInt(job.WorkflowRunID, 10)
	}, 10*time.Second, 20*time.Millisecond,
		"the replayed assignment must re-record the run, so the worker stays recoverable")
}

// TestListener_RecordsNothingForAJobItCouldNotProvision keeps the set honest about what
// it means. A record says "a worker exists for this run", and acting on one for a job
// that never got a worker would re-run a run nothing had disrupted.
func TestListener_RecordsNothingForAJobItCouldNotProvision(t *testing.T) {
	srv := newQuickPollServer(t)
	store := &fakeGuardStore{}
	m := newCountingMetrics()
	prov := &recordingProvisioner{srv: srv}
	_, ssID := startStoppableListener(t, srv, prov, m, fastDeferredRetry, fastAssignmentCheck, withGuards(store))

	// A job whose runner name will not register never reaches the provisioner.
	stallJob(t, srv, ssID, m, 1)

	assert.Never(t, func() bool { return len(store.saved().InFlight) > 0 }, 2*time.Second,
		50*time.Millisecond, "a job with no worker is owed no re-run")
	assert.Zero(t, prov.count(), "the stalled job must not have been provisioned")
}
