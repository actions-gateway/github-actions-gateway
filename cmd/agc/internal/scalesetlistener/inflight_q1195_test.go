package scalesetlistener_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListener_PersistsThatARunnerTookTheJob pins the Started mark on the in-flight
// record (Q1195). The reaper's stranded-job walk reads the record for a job no runner
// took; a finished job whose pod is deleted before its removal is saved would otherwise
// read as one, and the walk would force-cancel a run whose other jobs are still running.
func TestListener_PersistsThatARunnerTookTheJob(t *testing.T) {
	srv := newQuickPollServer(t)
	store := &fakeGuardStore{}
	prov := &recordingProvisioner{srv: srv}
	prov.setCompleteErr(true)
	log := &lifecycleLog{}
	_, ssID := startStoppableListener(t, srv, prov, newCountingMetrics(), withGuards(store), withLifecycle(log))

	job := srv.Enqueue(ssID)
	require.Eventually(t, func() bool { return len(store.saved().InFlight) == 1 }, 10*time.Second,
		20*time.Millisecond, "the job must be in flight before its start can mark it")
	require.False(t, store.saved().InFlight[0].Started, "a job no runner has taken must not read as started")

	require.True(t, srv.StartAssignedJob(ssID, job.JobID, "linux-elsewhere"))
	require.Eventually(t, func() bool { return len(log.seen()) == 1 }, 10*time.Second,
		20*time.Millisecond, "the start must reach the listener")
	assert.Eventually(t, func() bool {
		saved := store.saved().InFlight
		return len(saved) == 1 && saved[0].Started
	}, 10*time.Second, 20*time.Millisecond, "a JobStarted must be persisted on the job's in-flight record")
}
