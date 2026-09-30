package scalesetlistener_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// Q1151: GitHub hands a scale-set job to whichever of the set's runners asks first, so
// the worker minted for one job can run another. On 2026-09-30 the AGC stamped the
// worker minted for a job that finished elsewhere and reaped it five minutes later, mid
// job. The listener's half of the fix is to hand the reclaim the runner that held the
// job, and to record each JobStarted so a completion naming no runner can tell a busy
// worker from an idle one.

// lifecycleLog records Started and Cleanup calls in one sequence, so a test can assert
// the order the listener made them in.
type lifecycleLog struct {
	mu     sync.Mutex
	events []string
}

func (l *lifecycleLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *lifecycleLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *lifecycleLog) started(_ context.Context, jobID, runnerName string) error {
	l.add("started " + jobID + " on " + runnerName)
	return nil
}

func (l *lifecycleLog) cleanup(_ context.Context, jobID, runnerName string) error {
	l.add("completed " + jobID + " on " + runnerName)
	return nil
}

func withLifecycle(log *lifecycleLog) func(*scalesetlistener.Config) {
	return func(c *scalesetlistener.Config) {
		c.Started = log.started
		c.Cleanup = log.cleanup
	}
}

// TestListener_CompletionNamesTheRunnerThatHeldTheJob pins that the reclaim is told
// which runner held the job, which is the only thing that can find the right worker
// when it is not the one minted for jobID.
func TestListener_CompletionNamesTheRunnerThatHeldTheJob(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobCompleted, JobID: "job-a", RunnerName: "linux-job-b", Result: "succeeded"},
	})

	cl := &recordingCleanup{}
	startListener(t, srv, fixedCapacity(5), &recordingProvisioner{srv: srv, completeErr: true}, nil,
		func(c *scalesetlistener.Config) { c.Cleanup = cl.cleanup })

	require.Eventually(t, func() bool { return len(cl.seen()) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"job-a"}, cl.seen())
	assert.Equal(t, []string{"linux-job-b"}, cl.seenRunners(),
		"the reclaim must be keyed by the runner the completion names, not derived from jobID")
}

// TestListener_RecordsAStartBeforeARunnerlessCompletionInTheSameBatch covers the batch
// the fix turns on: job-x ended before any runner took it, while the runner minted for
// job-x started job-d. The completion sits first on the wire, and handled in wire order
// it would reclaim the worker whose runner is running job-d before anything recorded
// that it was busy.
func TestListener_RecordsAStartBeforeARunnerlessCompletionInTheSameBatch(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobCompleted, JobID: "job-x", Result: "canceled"},
		{MessageType: scaleset.MessageTypeJobStarted, JobID: "job-d", RunnerName: "linux-job-x"},
	})

	log := &lifecycleLog{}
	startListener(t, srv, fixedCapacity(5), &recordingProvisioner{srv: srv, completeErr: true}, nil,
		withLifecycle(log))

	require.Eventually(t, func() bool { return len(log.seen()) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"started job-d on linux-job-x", "completed job-x on "}, log.seen(),
		"a batch's starts must be recorded before its completions are reclaimed")
}

// TestListener_IgnoresAStartReplayedAfterItsCompletion pins the replay guard: a
// re-created session polls from cursor 0, so a JobStarted can arrive after its job's
// completion was handled, and recording it would mark a finished worker busy.
func TestListener_IgnoresAStartReplayedAfterItsCompletion(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobCompleted, JobID: "job-d", RunnerName: "linux-job-d", Result: "succeeded"},
	})
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobStarted, JobID: "job-d", RunnerName: "linux-job-d"},
	})
	// A later start for a live job, so the test waits on something that must happen
	// rather than on the absence of a call.
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobStarted, JobID: "job-e", RunnerName: "linux-job-e"},
	})

	log := &lifecycleLog{}
	startListener(t, srv, fixedCapacity(5), &recordingProvisioner{srv: srv, completeErr: true}, nil,
		withLifecycle(log))

	require.Eventually(t, func() bool { return len(log.seen()) >= 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"completed job-d on linux-job-d", "started job-e on linux-job-e"}, log.seen(),
		"a start for a job already complete must not be recorded")
}
