package scalesetlistener_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
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
		return sink.lastIs(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionTrue,
			v2alpha1.ReasonWorkerTerminating)
	}, 5*time.Second, 10*time.Millisecond,
		"a stall on a terminating worker must report its own reason")
	cond, _ := sink.last(v2alpha1.ConditionJobProvisionStalled)
	assert.Contains(t, cond.Message, "previous worker pod to finish terminating")
	assert.Equal(t, 1, sink.eventCount("WorkerTerminating"), "one event per episode")
	assert.Zero(t, sink.eventCount("JobProvisionStalled"), "an ordinary grace period is not a Warning")

	pod.finish()
	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionFalse,
			v2alpha1.ReasonJobsProvisioning)
	}, 5*time.Second, 10*time.Millisecond, "the stall clears once the job provisions")
}
