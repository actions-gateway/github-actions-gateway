package scalesetlistener_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// Q1360: GitHub withdraws an assignment no runner took in five minutes and re-issues the
// job under a new job ID, which an idle runner of the set can take within seconds. The
// re-issue's start can then arrive in the same batch as its assignment.

// TestListener_ProvisionsAJobWhoseStartArrivedInTheSameBatch pins that the assignment
// still gets a worker. The start is handled first, and the surplus it moves may already
// have been put on another job's still-Pending worker, on the reasoning that the re-issue's
// own worker will serve that job. Skipping the re-issue's worker leaves that job with
// none.
func TestListener_ProvisionsAJobWhoseStartArrivedInTheSameBatch(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: "job-y"},
		{MessageType: scaleset.MessageTypeJobStarted, JobID: "job-y", RunnerName: "linux-job-x"},
	})

	prov := &recordingProvisioner{srv: srv, completeErr: true}
	log := &lifecycleLog{}
	startListener(t, srv, fixedCapacity(5), prov, nil, withLifecycle(log))

	require.Eventually(t, func() bool {
		prov.mu.Lock()
		defer prov.mu.Unlock()
		return len(prov.provisioned) == 1
	}, 5*time.Second, 10*time.Millisecond, "the re-issue must get a worker even though a runner already started it")
	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Equal(t, "job-y", prov.provisioned[0].JobID)
	assert.Equal(t, []string{"started job-y on linux-job-x"}, log.seen())
}
