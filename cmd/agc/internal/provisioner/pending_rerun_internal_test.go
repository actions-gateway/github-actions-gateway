package provisioner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sweepPendingAndWait runs one pending sweep, requires it to release want parked
// re-runs, and waits for each to finish.
func sweepPendingAndWait(t *testing.T, p *Provisioner, ctx context.Context, want int) {
	t.Helper()
	released := p.sweepPendingReruns(ctx)
	require.Len(t, released, want, "parked re-runs released by this sweep")
	for _, done := range released {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a released re-run never finished")
		}
	}
}

// testClock is a settable clock for p.now.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// pendingFixture is a set whose run 4242 is still running, as a long matrix is after one
// of its jobs failed, until concluded is set.
func pendingFixture(t *testing.T) (*Provisioner, *ledgerRunAPI, *stubTarget, *atomic.Bool, *testClock, func() *Provisioner) {
	t.Helper()
	p, api, target, concluded, clock, newP, _ := pendingFixtureWith(t)
	return p, api, target, concluded, clock, newP
}

// pendingFaults are failures pendingFixtureWith's GitHub serves ahead of the run API:
// readStatus, where non-zero, answers that many run reads with its status code, and a
// rerun POST waits on rerunGate, where set, before it is answered.
type pendingFaults struct {
	mu         sync.Mutex
	readStatus int
	reads      int
	rerunGate  chan struct{}
}

func (f *pendingFaults) failReads(status, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readStatus, f.reads = status, n
}

func pendingFixtureWith(t *testing.T) (*Provisioner, *ledgerRunAPI, *stubTarget, *atomic.Bool, *testClock, func() *Provisioner, *pendingFaults) {
	t.Helper()
	c, api, _, target := ledgerFixture(t)
	target.spec = &ResolvedSpec{MaxEvictionRetries: 2}
	concluded := &atomic.Bool{}
	api.holdUntil = concluded.Load
	faults := &pendingFaults{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		faults.mu.Lock()
		status, gate := 0, faults.rerunGate
		if r.Method == http.MethodGet && faults.reads > 0 {
			status = faults.readStatus
			faults.reads--
		}
		faults.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if r.Method == http.MethodPost && gate != nil {
			<-gate
		}
		api.handle(w, r)
	}))
	t.Cleanup(srv.Close)
	clock := newTestClock()
	newP := func() *Provisioner {
		p := ledgerProvisioner(c, srv)
		p.now = clock.now
		return p
	}
	return newP(), api, target, concluded, clock, newP, faults
}

func parkOOM(p *Provisioner, target Target, pod string) {
	evictScaleSet(p, target, "4242", 2, recoveryCauseOOM, rerunDisruption{key: pod, job: "build", park: true})
}

// TestPendingRerun_WaitsForTheRunToConclude is the defect: the re-run waits past the
// arms' 15-minute window for a run whose siblings are still running, holding the slot it
// reserved when the class fired, and lands once the run concludes.
func TestPendingRerun_WaitsForTheRunToConclude(t *testing.T) {
	ctx := context.Background()
	p, api, target, concluded, clock, _ := pendingFixture(t)

	parkOOM(p, target, "pod-a")
	require.Zero(t, api.reruns.Load(), "a parked re-run is not made while the run is running")
	assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRetries.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM)), "the slot is taken when the class fires")
	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	e := l.Pending[failureKey("4242", "build")]
	require.NotNil(t, e)
	assert.Equal(t, 1, e.FailedAttempt)
	assert.Equal(t, "myorg/myrepo", e.Repository)
	assert.Equal(t, "pod-a", e.Disruption)

	clock.advance(time.Hour)
	sweepPendingAndWait(t, p, ctx, 0)
	require.Zero(t, api.reruns.Load(), "an hour in, still running: still waiting")

	concluded.Store(true)
	clock.advance(10 * time.Minute)
	sweepPendingAndWait(t, p, ctx, 1)

	assert.Equal(t, int64(1), api.reruns.Load())
	assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRetries.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM)), "the release resumes the slot rather than taking another")
	assert.Zero(t, testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM, rerunFailureReasonNeverConcluded)))
	l, _, err = p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Empty(t, l.Pending, "a released entry leaves the ledger")

	sweepPendingAndWait(t, p, ctx, 0)
	assert.Empty(t, p.pendingSets.snapshot(), "a set with nothing parked leaves the sweep")
}

// TestPendingRerun_SurvivesARestart is the open design question: a new process finds
// the set through AdoptPendingReruns, which the reconciler calls for every set, and
// releases the entry its predecessor parked.
func TestPendingRerun_SurvivesARestart(t *testing.T) {
	ctx := context.Background()
	before, api, target, concluded, clock, newP := pendingFixture(t)
	parkOOM(before, target, "pod-a")

	after := newP()
	concluded.Store(true)
	clock.advance(time.Minute)
	sweepPendingAndWait(t, after, ctx, 0)
	require.Zero(t, api.reruns.Load(), "a new process sweeps no set it has not been handed")

	require.NoError(t, after.AdoptPendingReruns(ctx, target))
	sweepPendingAndWait(t, after, ctx, 1)

	assert.Equal(t, int64(1), api.reruns.Load())
	assert.Zero(t, testutil.ToFloat64(after.Metrics.EvictionRetries.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM)), "the predecessor's slot is resumed, not a second one taken")

	sweepPendingAndWait(t, after, ctx, 0)
	require.NoError(t, after.AdoptPendingReruns(ctx, target))
	assert.Empty(t, after.pendingSets.snapshot(), "adoption is once per process; an emptied set is not re-added")
}

// TestPendingRerun_ExpiresAfterADay: a run that never concludes is reported
// run_never_concluded and its entry dropped, rather than polled forever.
func TestPendingRerun_ExpiresAfterADay(t *testing.T) {
	ctx := context.Background()
	p, api, target, _, clock, _ := pendingFixture(t)
	parkOOM(p, target, "pod-a")

	clock.advance(pendingRerunMaxWait - time.Minute)
	sweepPendingAndWait(t, p, ctx, 0)
	require.Zero(t, testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM, rerunFailureReasonNeverConcluded)))

	clock.advance(time.Minute)
	sweepPendingAndWait(t, p, ctx, 0)

	assert.Zero(t, api.reruns.Load())
	assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM, rerunFailureReasonNeverConcluded)))
	assert.Contains(t, target.events, "EvictionRerunFailed")
	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Empty(t, l.Pending)
}

// TestPendingRerun_DroppedWhenTheRunWasReRun: release judges against the attempt the job
// failed in, so a run someone re-ran while the entry waited is not re-run again, even
// once that attempt concludes.
func TestPendingRerun_DroppedWhenTheRunWasReRun(t *testing.T) {
	ctx := context.Background()
	p, api, target, concluded, clock, _ := pendingFixture(t)
	parkOOM(p, target, "pod-a")

	api.set("failure", 2)
	concluded.Store(true)
	clock.advance(time.Minute)
	sweepPendingAndWait(t, p, ctx, 0)

	assert.Zero(t, api.reruns.Load())
	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Empty(t, l.Pending)
}

// TestPendingRerun_PollCadenceBoundsTheReads: the widening cadence keeps a day-long wait
// near the design's 175 reads.
func TestPendingRerun_PollCadenceBoundsTheReads(t *testing.T) {
	var s pendingSets
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	reads := 0
	for age := time.Duration(0); age < pendingRerunMaxWait; age += 30 * time.Second {
		if s.due("e", age, t0.Add(age)) {
			reads++
		}
	}
	assert.InDelta(t, 175, reads, 5)
}

// TestPendingRerun_AFailedReadRetries is the review finding on #2216: over a day of
// polling a run read will fail now and then, a rate limit or a token that would not
// mint, and that must cost the entry one poll rather than its re-run.
func TestPendingRerun_AFailedReadRetries(t *testing.T) {
	ctx := context.Background()
	p, api, target, concluded, clock, _, faults := pendingFixtureWith(t)
	parkOOM(p, target, "pod-a")

	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusUnauthorized} {
		faults.failReads(status, 1)
		clock.advance(time.Hour)
		sweepPendingAndWait(t, p, ctx, 0)
	}
	tokenFails := &atomic.Bool{}
	tokenFails.Store(true)
	p.TokenFunc = func(context.Context) (string, error) {
		if tokenFails.CompareAndSwap(true, false) {
			return "", assert.AnError
		}
		return "tok", nil
	}
	clock.advance(time.Hour)
	sweepPendingAndWait(t, p, ctx, 0)

	assert.Zero(t, testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM, rerunFailureReasonAPIError)))
	concluded.Store(true)
	clock.advance(time.Hour)
	sweepPendingAndWait(t, p, ctx, 1)
	assert.Equal(t, int64(1), api.reruns.Load(), "the entry survived every failed read and re-ran")
}

// TestPendingRerun_ARunThatIsGoneEndsTheWait: a 404 will not change in a day, so it is
// reported at once rather than polled to expiry.
func TestPendingRerun_ARunThatIsGoneEndsTheWait(t *testing.T) {
	ctx := context.Background()
	p, api, target, _, clock, _, faults := pendingFixtureWith(t)
	parkOOM(p, target, "pod-a")

	faults.failReads(http.StatusNotFound, 1)
	clock.advance(time.Minute)
	sweepPendingAndWait(t, p, ctx, 0)

	assert.Zero(t, api.reruns.Load())
	assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM, rerunFailureReasonAPIError)))
	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Empty(t, l.Pending)
}

// TestPendingRerun_TheEntryOutlivesItsRelease: the entry leaves the ledger only once the
// release has finished, so a process that stops mid-release leaves it for its successor
// to release again, and that release resumes the same slot.
func TestPendingRerun_TheEntryOutlivesItsRelease(t *testing.T) {
	ctx := context.Background()
	p, api, target, concluded, clock, newP, faults := pendingFixtureWith(t)
	parkOOM(p, target, "pod-a")
	gate := make(chan struct{})
	faults.mu.Lock()
	faults.rerunGate = gate
	faults.mu.Unlock()

	concluded.Store(true)
	clock.advance(time.Minute)
	released := p.sweepPendingReruns(ctx)
	require.Len(t, released, 1)

	successor := newP()
	l, _, err := successor.readRerunLedger(ctx, target)
	require.NoError(t, err)
	require.Contains(t, l.Pending, failureKey("4242", "build"), "the entry is still parked while its release is in flight")

	close(gate)
	<-released[0]
	assert.Equal(t, int64(1), api.reruns.Load())
	l, _, err = successor.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Empty(t, l.Pending)
}
