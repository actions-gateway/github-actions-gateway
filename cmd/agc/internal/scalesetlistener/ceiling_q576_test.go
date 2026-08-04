package scalesetlistener_test

import (
	"context"
	"errors"
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

// Tests for the Q576 ceiling path. A saturated set on the rc.3 dogfood gate retried one
// assigned job at ~0.8/s for 14 minutes and issued a GitHub deregister call per attempt
// (704 of them), because a worker-ceiling rejection came back from Provision as an
// undifferentiated error: the listener read it as transient, held the queue cursor, and
// the long-poll queue redelivered the message immediately — each pass minting a fresh
// runner registration that the next pass's name conflict had to deregister.

// ceilingGate is a CapacityCheckFunc a test can close and open. It counts the checks the
// listener makes, so a test can wait for real re-offers to have happened rather than
// asserting into a window that may have contained none.
type ceilingGate struct {
	mu     sync.Mutex
	full   bool
	checks int
}

func (g *ceilingGate) check(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks++
	if g.full {
		return fmt.Errorf("%w: 8 active pods", scalesetlistener.ErrCapacityUnavailable)
	}
	return nil
}

func (g *ceilingGate) setFull(full bool) {
	g.mu.Lock()
	g.full = full
	g.mu.Unlock()
}

func (g *ceilingGate) checkCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checks
}

// fastCapacityRetry shortens the ceiling re-offer interval so a test drives the path in
// test time; production waits 5s between re-offers.
func fastCapacityRetry(c *scalesetlistener.Config) { c.CapacityRetryInterval = 20 * time.Millisecond }

// TestListener_CeilingBlockedJobRegistersNoRunner is the root half of the Q576 fix:
// minting a JIT config REGISTERS a runner at GitHub, so the capacity check has to run
// before the mint. However many times a ceiling-blocked job is re-offered, it must cost
// zero registrations — and therefore zero deregisters, which is what the gate observed
// 704 of.
func TestListener_CeilingBlockedJobRegistersNoRunner(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	m := newCountingMetrics()
	gate := &ceilingGate{full: true}
	// Both rungs are gated, as they are in production: the pre-check and the
	// authoritative check the provisioner makes with the pod about to be created. The
	// job is therefore held either way — what the pre-check changes is whether holding
	// it costs a GitHub registration.
	prov := &recordingProvisioner{srv: srv}
	provision := func(ctx context.Context, job scalesetlistener.Job) error {
		if err := gate.check(ctx); err != nil {
			return err
		}
		return prov.provision(ctx, job)
	}
	_, ssID := startListenerFunc(t, srv, fixedCapacity(5), provision, m, fastCapacityRetry,
		func(c *scalesetlistener.Config) { c.CheckCapacity = gate.check })
	prov.setScaleSetID(ssID)

	_, jobID := srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool {
		return m.deferredFor(scalesetlistener.DeferReasonCeiling) == 1
	}, 5*time.Second, 10*time.Millisecond,
		"a job the ceiling rejects must be held for a re-offer, under the ceiling reason")

	assert.Zero(t, srv.GenerateJITCalls(), "a ceiling-blocked job must not mint a JIT config")
	assert.Empty(t, srv.RegisteredRunners(), "nothing may be registered at GitHub for a job that cannot run")
	assert.Zero(t, srv.DeleteRunnerCalls(), "with nothing registered there is nothing to deregister")
	assert.Zero(t, m.provisionErrors(), "a full ceiling is backpressure, not a provision error")
	assert.Empty(t, prov.jobIDs(), "the job cannot provision while the ceiling is full")

	// Let several more re-offers run, so the zeroes above are the pre-check and not a
	// race the assertions happened to win.
	before := gate.checkCount()
	require.Eventually(t, func() bool { return gate.checkCount() >= before+3 }, 5*time.Second, 10*time.Millisecond,
		"the held job must keep being re-offered")
	assert.Zero(t, srv.GenerateJITCalls(), "no re-offer may mint a registration while the ceiling is full")
	assert.Zero(t, srv.DeleteRunnerCalls(), "no re-offer may need a deregister")

	// A worker finishes and the ceiling opens.
	gate.setFull(false)

	require.Eventually(t, func() bool { return m.provisionedCount() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the re-offer must provision the job once capacity frees")
	assert.Equal(t, []string{jobID}, prov.jobIDs(), "the deferred job provisions exactly once")
	assert.Equal(t, 1, srv.GenerateJITCalls(), "exactly one registration, minted when the job could actually run")
	assert.Zero(t, srv.DeleteRunnerCalls(), "one mint under a free name conflicts with nothing")
	assert.Eventually(t, func() bool { return m.deferredFor(scalesetlistener.DeferReasonCeiling) == 0 },
		5*time.Second, 10*time.Millisecond, "a provisioned job leaves the deferred set")
}

// TestListener_CeilingRejectionFromProvisionIsDeferredNotRedelivered covers the race the
// pre-check cannot close — a sibling job takes the last slot between the check and the
// pod create — and with it the retry cadence itself. The rejection must route to the
// re-offer backoff, not to the cursor hold: a held cursor redelivers the message with no
// pause at all, which is the spin the gate measured.
func TestListener_CeilingRejectionFromProvisionIsDeferredNotRedelivered(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	m := newCountingMetrics()
	gate := &ceilingGate{full: true}
	provisioned := &atomicInt{}
	provision := func(ctx context.Context, job scalesetlistener.Job) error {
		if err := gate.check(ctx); err != nil {
			return err
		}
		provisioned.set(1)
		return nil
	}
	// No CheckCapacity: every attempt reaches the mint, so each one that is retried
	// costs a registration the next attempt has to deregister — the gate's symptom,
	// and here the thing that makes the retry cadence directly countable.
	_, ssID := startListenerFunc(t, srv, fixedCapacity(5), provision, m, fastCapacityRetry)

	srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool {
		return m.deferredFor(scalesetlistener.DeferReasonCeiling) == 1
	}, 5*time.Second, 10*time.Millisecond,
		"a ceiling rejection from Provision must defer the job, not hold the cursor for a redelivery")
	assert.Zero(t, m.provisionErrors(), "a full ceiling is backpressure, not a provision error")

	// Pace: with the job deferred, re-offers are spaced by CapacityRetryInterval and each
	// costs one deregister-and-remint of the name the previous attempt left registered.
	// Holding the cursor instead would redeliver with no pause and run this into the
	// hundreds, as it did live.
	before := srv.DeleteRunnerCalls()
	time.Sleep(400 * time.Millisecond)
	assert.Less(t, srv.DeleteRunnerCalls()-before, 15,
		"re-offers must be paced by the capacity interval, not spun by immediate redelivery")

	gate.setFull(false)
	require.Eventually(t, func() bool { return provisioned.get() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the job must provision once capacity frees")
}

// TestListener_CeilingStallReportsItsOwnConditionAndEvent covers the operator surface.
// Reporting a saturated set as RunnerNameConflict would send an operator hunting stale
// registrations that do not exist, and a Warning for a set running at the concurrency its
// own spec declares is noise. When a real name conflict then joins the episode, it has to
// surface anyway rather than being swallowed by the Normal event already recorded.
func TestListener_CeilingStallReportsItsOwnConditionAndEvent(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	sink := &statusSink{}
	gate := &ceilingGate{full: true}
	_, ssID := startListenerWithSink(t, srv, sink, fastCapacityRetry, fastDeferredRetry,
		func(c *scalesetlistener.Config) {
			c.CheckCapacity = gate.check
			c.Capacity = fixedCapacity(5)
		})

	srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionTrue,
			v2alpha1.ReasonWorkerCeilingReached)
	}, 5*time.Second, 10*time.Millisecond,
		"a capacity stall must report its own reason, not a runner-name conflict")

	cond, _ := sink.last(v2alpha1.ConditionJobProvisionStalled)
	assert.Contains(t, cond.Message, "waiting for worker capacity",
		"the message must say what the jobs are waiting on")
	assert.Equal(t, 1, sink.eventCount("WorkerCeilingReached"), "one event per episode")
	assert.Zero(t, sink.eventCount("JobProvisionStalled"),
		"expected backpressure must not be reported as a provisioning stall Warning")

	// A job whose runner name will not register joins the same episode. It outranks the
	// ceiling — it is the one an operator can act on — so both the condition reason and a
	// Warning event must follow it even though the set is already stalled.
	_, stuckJobID := srv.EnqueueJob(ssID)
	srv.FailJITConfigNamePrefix("linux-" + stuckJobID)
	gate.setFull(false)

	require.Eventually(t, func() bool {
		return sink.lastIs(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionTrue,
			v2alpha1.ReasonRunnerNameConflict)
	}, 5*time.Second, 10*time.Millisecond,
		"a runner-name conflict must outrank a ceiling hold in the reported reason")
	assert.Positive(t, sink.eventCount("JobProvisionStalled"),
		"the conflict must record its Warning even though the episode was already open")
}

// TestListener_TransientProvisionErrorStillRedelivers pins the distinction Q576 turns on.
// An API blip or an apiserver 500 is exactly what the cursor hold is for — it may well be
// gone by the next delivery — so only a capacity rejection may be diverted onto the
// backoff. Collapsing the two would trade one bug for the opposite one.
func TestListener_TransientProvisionErrorStillRedelivers(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	m := newCountingMetrics()
	failing := &atomicInt{}
	failing.set(1)
	provisioned := &atomicInt{}
	provision := func(_ context.Context, job scalesetlistener.Job) error {
		if failing.get() == 1 {
			return errors.New("apiserver: internal error")
		}
		provisioned.set(1)
		return nil
	}
	_, ssID := startListenerFunc(t, srv, fixedCapacity(5), provision, m, fastCapacityRetry)

	srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool { return m.provisionErrors() >= 3 }, 5*time.Second, 10*time.Millisecond,
		"a transient failure must be counted as a provision error and retried by redelivery")
	assert.Zero(t, m.deferredCount(), "a transient failure must not be diverted onto the re-offer backoff")

	failing.set(0)
	require.Eventually(t, func() bool { return provisioned.get() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the redelivered job must provision once the transient failure clears")
}
