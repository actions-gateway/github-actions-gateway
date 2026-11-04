package scalesetlistener_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Q1199: a job replayed while its previous worker pod is still terminating cannot have a
// worker until that pod is gone. A graceful termination lasts seconds to minutes, and a
// pod on a lost node stays terminating until the node is removed, so the rejection has to
// go onto the re-offer backoff: a held cursor redelivers immediately, minting a runner
// registration per pass for as long as the pod lingers.

// terminatingPod is a ProvisionFunc stand-in whose job pod is terminating until the test
// says it is gone. It counts the attempts so a test can measure their pace.
type terminatingPod struct {
	mu       sync.Mutex
	gone     bool
	attempts int
	done     int
	// checks counts the pre-checks, a re-offer the pre-check held before Provision.
	checks int
}

func (p *terminatingPod) provision(context.Context, scalesetlistener.Job) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	if !p.gone {
		return fmt.Errorf("%w: pod linux-job-1 is still terminating", scalesetlistener.ErrWorkerTerminating)
	}
	p.done++
	return nil
}

func (p *terminatingPod) finish() {
	p.mu.Lock()
	p.gone = true
	p.mu.Unlock()
}

func (p *terminatingPod) counts() (attempts, done int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts, p.done
}

func TestListener_TerminatingWorkerIsDeferredNotRedelivered(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	m := newCountingMetrics()
	pod := &terminatingPod{}
	_, ssID := startListenerFunc(t, srv, fixedCapacity(5), pod.provision, m, fastDeferredRetry)

	srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool {
		return m.deferredFor(scalesetlistener.DeferReasonWorkerTerminating) == 1
	}, 5*time.Second, 10*time.Millisecond,
		"a terminating previous worker must defer the job, not hold the cursor for a redelivery")
	assert.Zero(t, m.provisionErrors(), "waiting out a termination is backpressure, not a provision error")

	// Pace: the conflict ladder from a 20ms base allows a handful of re-offers in this
	// window; immediate redelivery would run it into the hundreds.
	before, _ := pod.counts()
	time.Sleep(400 * time.Millisecond)
	after, _ := pod.counts()
	assert.Less(t, after-before, 15, "re-offers must be paced by the backoff, not spun by immediate redelivery")

	pod.finish()
	require.Eventually(t, func() bool { return m.provisionedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the re-offer must provision the job once the previous worker is gone")
	_, done := pod.counts()
	assert.Equal(t, 1, done, "the job provisions exactly once")
	assert.Eventually(t, func() bool { return m.deferredFor(scalesetlistener.DeferReasonWorkerTerminating) == 0 },
		5*time.Second, 10*time.Millisecond, "a provisioned job leaves the deferred set")
}

// The stall names its own cause, so an operator looks at the terminating pod rather than
// at runner registrations or the worker ceiling, and a Normal event rather than a Warning
// marks it: the ordinary case is a grace period that ends on its own.
func TestListener_TerminatingWorkerStallReportsItsOwnConditionAndEvent(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	sink := &statusSink{}
	pod := &terminatingPod{}
	_, ssID := startListenerWithSink(t, srv, sink, fastDeferredRetry,
		func(c *scalesetlistener.Config) {
			c.Provision = pod.provision
			c.Capacity = fixedCapacity(5)
		})

	srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool {
		return sink.lastIs(v2.ConditionJobProvisionStalled, metav1.ConditionTrue,
			v2.ReasonWorkerTerminating)
	}, 5*time.Second, 10*time.Millisecond,
		"a stall on a terminating worker must report its own reason")
	cond, _ := sink.last(v2.ConditionJobProvisionStalled)
	assert.Contains(t, cond.Message, "previous worker pod to finish terminating")
	assert.Equal(t, 1, sink.eventCount("WorkerTerminating"), "one event per episode")
	assert.Zero(t, sink.eventCount("JobProvisionStalled"), "an ordinary grace period is not a Warning")

	pod.finish()
	require.Eventually(t, func() bool {
		return sink.lastIs(v2.ConditionJobProvisionStalled, metav1.ConditionFalse,
			v2.ReasonJobsProvisioning)
	}, 5*time.Second, 10*time.Millisecond, "the stall clears once the job provisions")
}

// check is a WorkerCheckFunc reading the same pod, so the pre-check and the provisioner's
// authoritative check agree, as they do in production.
func (p *terminatingPod) check(context.Context, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks++
	if !p.gone {
		return fmt.Errorf("%w: pod linux-job-1 is still terminating", scalesetlistener.ErrWorkerTerminating)
	}
	return nil
}

// reoffers counts the times the listener tried the job, at whichever check held it.
func (p *terminatingPod) reoffers() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checks + p.attempts
}

// Q1233: a job held for a terminating worker is re-offered on the backoff, and minting a
// JIT config registers a runner at GitHub, so the pod check has to run before the mint.
// Without it each re-offer registers a runner that the next one's name conflict
// deregisters.
func TestListener_TerminatingWorkerRegistersNoRunner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		preCheck bool
	}{
		{name: "with the pre-check", preCheck: true},
		{name: "without it, each re-offer mints", preCheck: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := scalesettest.New()
			t.Cleanup(srv.Close)

			m := newCountingMetrics()
			pod := &terminatingPod{}
			opts := []func(*scalesetlistener.Config){fastDeferredRetry}
			if tc.preCheck {
				opts = append(opts, func(c *scalesetlistener.Config) { c.CheckWorker = pod.check })
			}
			_, ssID := startListenerFunc(t, srv, fixedCapacity(5), pod.provision, m, opts...)
			srv.EnqueueJob(ssID)

			require.Eventually(t, func() bool {
				return m.deferredFor(scalesetlistener.DeferReasonWorkerTerminating) == 1
			}, 5*time.Second, 10*time.Millisecond, "the job is held for the terminating worker")
			// Several re-offers, so a zero below is the pre-check and not a window that
			// held none.
			require.Eventually(t, func() bool { return pod.reoffers() >= 3 }, 5*time.Second, 10*time.Millisecond,
				"the held job must keep being re-offered")

			attempts, _ := pod.counts()
			if !tc.preCheck {
				t.Logf("attempts=%d mints=%d deregisters=%d", attempts, srv.GenerateJITCalls(), srv.DeleteRunnerCalls())
				assert.GreaterOrEqual(t, srv.GenerateJITCalls(), attempts, "every re-offer reaches the mint")
				assert.GreaterOrEqual(t, srv.DeleteRunnerCalls(), attempts-1, "each later re-offer deregisters the last one's runner")
				return
			}
			assert.Zero(t, srv.GenerateJITCalls(), "no re-offer may mint while the previous worker terminates")
			assert.Zero(t, srv.DeleteRunnerCalls(), "with nothing registered there is nothing to deregister")
			assert.Zero(t, attempts, "the pre-check holds the job before Provision")

			pod.finish()
			require.Eventually(t, func() bool { return m.provisionedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
				"the re-offer provisions the job once the previous worker is gone")
			assert.Equal(t, 1, srv.GenerateJITCalls(), "exactly one registration, minted when the job could run")
		})
	}
}
