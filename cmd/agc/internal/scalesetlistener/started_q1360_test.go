package scalesetlistener_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// Q1360: GitHub withdraws an assignment no runner took in five minutes and re-issues the
// job under a new job ID, which the set's first idle runner can take within seconds. When
// the re-issue and its start arrive in one batch, a worker built for the re-issue would
// have nothing to run and nothing to give it a deadline.

// TestListener_DoesNotProvisionAJobARunnerAlreadyStarted pins that the start, handled
// ahead of the batch's assignments, keeps the assignment from building a worker.
func TestListener_DoesNotProvisionAJobARunnerAlreadyStarted(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: "job-y"},
		{MessageType: scaleset.MessageTypeJobStarted, JobID: "job-y", RunnerName: "linux-job-x"},
	})
	// A later assignment, so the test waits on something that must happen rather than on
	// the absence of a call.
	srv.SeedMessage([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: "job-z"},
	})

	prov := &recordingProvisioner{srv: srv, completeErr: true}
	log := &lifecycleLog{}
	startListener(t, srv, fixedCapacity(5), prov, nil, withLifecycle(log))

	provisioned := func() []string {
		prov.mu.Lock()
		defer prov.mu.Unlock()
		var ids []string
		for _, j := range prov.provisioned {
			ids = append(ids, j.JobID)
		}
		return ids
	}
	require.Eventually(t, func() bool { return len(provisioned()) >= 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"job-z"}, provisioned(), "a job a runner already holds needs no worker of its own")
	assert.Equal(t, []string{"started job-y on linux-job-x"}, log.seen())
}
