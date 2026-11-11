package scalesetlistener_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesetstub"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Q1330: a set refusing fork pull requests checks each job's source before the mint. A
// refused job registers no runner and is never checked again; a check that cannot be
// made holds the job rather than admitting it.

// sourceCheck is a SourceCheckFunc whose verdict the test sets, counting its calls and
// keeping the jobs it was asked about.
type sourceCheck struct {
	mu      sync.Mutex
	verdict error
	calls   int
	jobs    []scalesetlistener.Job
}

func (s *sourceCheck) check(_ context.Context, job scalesetlistener.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.jobs = append(s.jobs, job)
	return s.verdict
}

func (s *sourceCheck) set(err error) {
	s.mu.Lock()
	s.verdict = err
	s.mu.Unlock()
}

func (s *sourceCheck) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestListener_RefusedJobRegistersNoRunnerAndIsNotRechecked(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	m := newCountingMetrics()
	src := &sourceCheck{verdict: fmt.Errorf("%w: head repository %q", scalesetlistener.ErrJobRefused, "contributor/widgets")}
	var provisioned int
	var mu sync.Mutex
	provision := func(context.Context, scalesetlistener.Job) error {
		mu.Lock()
		provisioned++
		mu.Unlock()
		return nil
	}
	_, ssID := startListenerFunc(t, srv, fixedCapacity(5), provision, m, fastDeferredRetry,
		func(c *scalesetlistener.Config) { c.CheckSource = src.check })

	srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool { return src.count() >= 1 }, 5*time.Second, 10*time.Millisecond,
		"the assignment must reach the source check")
	// Long enough for a held cursor to redeliver many times, or a deferral to re-offer.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, src.count(), "a refused job is checked once, not re-offered")
	assert.Zero(t, srv.GenerateJITCalls(), "a refused job must register no runner")
	mu.Lock()
	assert.Zero(t, provisioned, "a refused job must not reach Provision")
	mu.Unlock()
	for _, reason := range []string{scalesetlistener.DeferReasonSourceUnverified, scalesetlistener.DeferReasonCeiling} {
		assert.Zero(t, m.deferredFor(reason), "a refusal is not a deferral (%s)", reason)
	}

	src.mu.Lock()
	job := src.jobs[0]
	src.mu.Unlock()
	assert.Equal(t, scalesetstub.DefaultJobOwner, job.Owner, "the check is handed the job's run identity")
	assert.Equal(t, scalesetstub.DefaultJobRepository, job.Repository)
	assert.NotEmpty(t, job.RunID)
	assert.Equal(t, scalesetstub.DefaultJobEvent, job.EventName, "the check is handed the message's event")
}

func TestListener_UncheckableSourceIsHeldThenProvisioned(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	sink := &statusSink{}
	m := newCountingMetrics()
	src := &sourceCheck{verdict: errors.New("run-status API returned 502")}
	_, ssID := startListenerWithSink(t, srv, sink, fastDeferredRetry,
		func(c *scalesetlistener.Config) {
			c.Provision = func(context.Context, scalesetlistener.Job) error { return nil }
			c.Capacity = fixedCapacity(5)
			c.CheckSource = src.check
			c.Metrics = m
		})

	srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool {
		return sink.lastIs(v2.ConditionJobProvisionStalled, metav1.ConditionTrue, v2.ReasonJobSourceUnverified)
	}, 5*time.Second, 10*time.Millisecond, "an uncheckable job is held under its own reason")
	cond, _ := sink.last(v2.ConditionJobProvisionStalled)
	assert.Contains(t, cond.Message, "check for a fork pull request")
	assert.Equal(t, 1, sink.eventCount("JobSourceUnverified"), "one Warning per episode")
	require.Eventually(t, func() bool { return src.count() >= 3 }, 5*time.Second, 10*time.Millisecond,
		"the held job is re-checked on the backoff")
	assert.Zero(t, srv.GenerateJITCalls(), "a held job registers no runner")

	src.set(nil)
	require.Eventually(t, func() bool { return m.provisionedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the job provisions once its source can be checked")
	require.Eventually(t, func() bool {
		return sink.lastIs(v2.ConditionJobProvisionStalled, metav1.ConditionFalse, v2.ReasonJobsProvisioning)
	}, 5*time.Second, 10*time.Millisecond, "the stall clears")
}
