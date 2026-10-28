package scalesetlistener_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// Q555: the listener reports each terminal completion, with its result and the run it
// belongs to, for flaky-job detection.

type completedLog struct {
	mu     sync.Mutex
	jobs   []scalesetlistener.Job
	result []string
	err    error
}

func (l *completedLog) completed(_ context.Context, job scalesetlistener.Job, result string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jobs = append(l.jobs, job)
	l.result = append(l.result, result)
	return l.err
}

func (l *completedLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.jobs)
}

// TestListener_CompletionCarriesItsRunAndResult pins what detection keys and labels on.
func TestListener_CompletionCarriesItsRunAndResult(t *testing.T) {
	srv := newQuickPollServer(t)
	srv.SeedMessage([]scaleset.JobMessage{{
		MessageType: scaleset.MessageTypeJobCompleted, JobID: "job-a", RunnerName: "linux-job-a", Result: "failed",
		OwnerName: "myorg", RepositoryName: "repo", WorkflowRunID: 222, JobDisplayName: "build",
		EventName: "pull_request", JobWorkflowRef: "myorg/repo/.github/workflows/ci.yml@refs/pull/7/merge",
	}})

	log := &completedLog{}
	startListener(t, srv, fixedCapacity(5), &recordingProvisioner{srv: srv, completeErr: true}, nil,
		func(c *scalesetlistener.Config) { c.Completed = log.completed })

	require.Eventually(t, func() bool { return log.count() == 1 }, 5*time.Second, 10*time.Millisecond)
	log.mu.Lock()
	defer log.mu.Unlock()
	assert.Equal(t, scalesetlistener.Job{
		JobID: "job-a", RunnerName: "linux-job-a", Owner: "myorg", Repository: "repo", RunID: "222", JobName: "build",
		EventName: "pull_request", WorkflowRef: "myorg/repo/.github/workflows/ci.yml@refs/pull/7/merge",
	}, log.jobs[0])
	assert.Equal(t, []string{"failed"}, log.result)
}

// TestListener_AFailedReclaimOrReportHoldsNothing: a completion whose Secret reclaim
// fails is still reported, once, and a report that fails stalls nothing; the job
// after it still provisions and is reported in turn.
func TestListener_AFailedReclaimOrReportHoldsNothing(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	log := &completedLog{err: errors.New("apiserver: internal error")}
	cl := &recordingCleanup{err: errors.New("secrets forbidden")}
	_, ssID := startListener(t, srv, fixedCapacity(3), prov, nil,
		func(c *scalesetlistener.Config) {
			c.Completed = log.completed
			c.Cleanup = cl.cleanup
		})

	srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return len(cl.seen()) >= 1 }, 5*time.Second, 10*time.Millisecond)
	srv.EnqueueJob(ssID)
	require.Eventually(t, func() bool { return log.count() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"a failed reclaim and a failed report must not stop the next completion")
	assert.Equal(t, 2, log.count(), "each completion is reported once")
	assert.Equal(t, 2, prov.count())
}
