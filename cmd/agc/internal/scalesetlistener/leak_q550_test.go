package scalesetlistener_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registration-leak class (Q550). Every generatejitconfig pre-registers a runner
// record; the tests here pin what must and must not be left behind at GitHub, because
// a leftover record holds the deterministic {scaleSet}-{jobID} name that the job's own
// retries need.

// flakyProvisioner fails the first n provision calls, then succeeds — the shape of any
// transient provisioning failure (quota, stockout, admission) that makes the listener
// re-drive the same job.
type flakyProvisioner struct {
	srv        *scalesettest.Server
	scaleSetID int
	failFirst  int

	mu     sync.Mutex
	calls  int
	names  []string
	jobIDs []string
}

func (p *flakyProvisioner) provision(_ context.Context, job scalesetlistener.Job) error {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.names = append(p.names, job.RunnerName)
	p.mu.Unlock()
	if n <= p.failFirst {
		return errors.New("provision failed: no quota headroom")
	}
	p.mu.Lock()
	p.jobIDs = append(p.jobIDs, job.JobID)
	p.mu.Unlock()
	p.srv.CompleteAssignedJob(p.scaleSetID, job.JobID, "succeeded")
	return nil
}

func (p *flakyProvisioner) attemptedNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.names...)
}

func (p *flakyProvisioner) succeeded() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.jobIDs)
}

// TestListener_RetryReclaimsItsOwnLeftoverRegistration pins the case that already
// works, so the boundary of the leak is explicit: when the leftover record RESOLVES by
// name, the Q334 reclaim deletes it and re-registers under the same base name, and a
// job that fails to provision several times still ends with exactly one record.
func TestListener_RetryReclaimsItsOwnLeftoverRegistration(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &flakyProvisioner{srv: srv, failFirst: 3}
	_, ssID := startListenerFunc(t, srv, fixedCapacity(2), prov.provision, nil)
	prov.scaleSetID = ssID

	srv.EnqueueJob(ssID)

	require.Eventually(t, func() bool { return prov.succeeded() == 1 }, 10*time.Second, 10*time.Millisecond,
		"the job must eventually provision a worker despite the transient failures")

	attempts := prov.attemptedNames()
	require.GreaterOrEqual(t, len(attempts), 4, "the test needs the retry path to have run")

	// Every attempt re-minted, so every attempt registered — but each one reclaimed the
	// previous record, so only the surviving worker's name is left.
	live := attempts[len(attempts)-1]
	assert.Equal(t, []string{live}, srv.RegisteredRunners(),
		"a resolvable leftover is reclaimed by the next attempt, leaving exactly one record")
	assert.Equal(t, prov.attemptedNames()[0], live,
		"the reclaim reuses the deterministic base name rather than escalating to a suffix")
}

// TestListener_UnresolvableLeftoverAccumulatesSuffixedRecords is the leak the per-name
// reclaim cannot reach. When a record holds the name at generatejitconfig but the REST
// name filter does not return it, DeregisterRunnerByName finds nothing to delete and
// every retry escalates to a fresh suffixed name — each of which registers a record that
// no later attempt ever revisits. This is the shape that accumulates records for one
// scale set, and it is only collectable by the sweep.
func TestListener_UnresolvableLeftoverAccumulatesSuffixedRecords(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// Advertise no capacity until the stale record is in place, so the listener cannot
	// be assigned the job before the test has set up the conflict on its name.
	var open atomic.Bool
	capacity := func(context.Context) int {
		if open.Load() {
			return 1
		}
		return 0
	}

	prov := &recordingProvisioner{srv: srv}
	_, ssID := startListener(t, srv, capacity, prov, nil)

	// The job's base name is taken by a record the name filter will not resolve.
	_, jobID := srv.EnqueueJob(ssID)
	base := "linux-" + jobID
	srv.FailJITConfigName(base)
	srv.HideRunnerFromNameFilter(base)
	open.Store(true)

	require.Eventually(t, func() bool { return prov.count() == 1 }, 10*time.Second, 10*time.Millisecond,
		"the job must still provision, under a suffixed fresh name")

	assert.NotEqual(t, base, prov.runnerNames()[0],
		"an unresolvable base name forces the fresh-name fallback")

	// The unresolvable record is still there, and so is the suffixed one the worker
	// actually uses. Nothing in the provision path will ever revisit either name.
	assert.Equal(t, []string{base, prov.runnerNames()[0]}, srv.RegisteredRunners(),
		"the stranded record survives every retry; only a sweep can collect it")
}

// TestListener_SweepsUnclaimedRecordsOnStart pins the recovery half: records left by a
// previous process (an AGC that crashed, or every attempt made before this fix shipped)
// are collected when a listener starts, so a scale set already wedged by its own
// leftovers heals without operator intervention.
func TestListener_SweepsUnclaimedRecordsOnStart(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// Leftovers under this scale set's prefix, in the shapes the sweep must tell apart.
	srv.FailJITConfigName("linux-stale-1")   // offline, unclaimed  -> swept
	srv.FailJITConfigName("linux-stale-2")   // offline, unclaimed  -> swept
	srv.FailJITConfigName("linux-inflight")  // claimed by a pod    -> kept
	srv.FailJITConfigName("linux-running")   // online              -> kept
	srv.FailJITConfigName("other-set-stale") // another scale set   -> kept
	srv.SetRunnerOnline("linux-running")

	prov := &recordingProvisioner{srv: srv}
	_, ssID := startListener(t, srv, fixedCapacity(1), prov, nil, func(cfg *scalesetlistener.Config) {
		cfg.ClaimedRunnerNames = func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{"linux-inflight": {}}, nil
		}
	})
	_ = ssID

	require.Eventually(t, func() bool {
		return len(srv.RegisteredRunners()) == 3
	}, 5*time.Second, 10*time.Millisecond, "the two unclaimed offline records must be swept")

	assert.Equal(t, []string{"linux-inflight", "linux-running", "other-set-stale"},
		srv.RegisteredRunners(),
		"a claimed, an online, and another scale set's record must all survive the sweep")
}

// TestListener_SweepKeepsRecordsWhenClaimsAreUnknown is the safety property: if the
// claimed-name lookup fails, the sweep must delete nothing rather than guess. Deleting a
// record whose worker is still Pending would strand that job — the failure this fix
// exists to prevent.
func TestListener_SweepKeepsRecordsWhenClaimsAreUnknown(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	srv.FailJITConfigName("linux-stale-1")
	srv.FailJITConfigName("linux-stale-2")

	prov := &recordingProvisioner{srv: srv}
	startListener(t, srv, fixedCapacity(1), prov, nil, func(cfg *scalesetlistener.Config) {
		cfg.ClaimedRunnerNames = func(context.Context) (map[string]struct{}, error) {
			return nil, errors.New("pod list failed")
		}
	})

	// Give a sweep that was going to run the chance to run.
	require.Never(t, func() bool {
		return len(srv.RegisteredRunners()) < 2
	}, time.Second, 20*time.Millisecond,
		"an unreadable claim set must abort the sweep, not license deleting everything")
}
