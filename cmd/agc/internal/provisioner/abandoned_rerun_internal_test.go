package provisioner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// rerunTally is the counting rerun-failed-jobs stub. It records calls PER RUN ID,
// which is what a per-run budget assertion needs: a single total cannot tell one run
// looping from several runs each recovering once.
type rerunTally struct {
	mu    sync.Mutex
	calls map[string]int
}

func newRerunTally() *rerunTally { return &rerunTally{calls: map[string]int{}} }

func (r *rerunTally) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// .../actions/runs/<id>/rerun-failed-jobs
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		r.mu.Lock()
		if len(parts) >= 2 {
			r.calls[parts[len(parts)-2]]++
		}
		r.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}
}

func (r *rerunTally) for_(runID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[runID]
}

func abandonedRerunMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q691_eviction_retries_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q691_eviction_retries_exhausted_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		AbandonedRunRerunWaits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q691_abandoned_run_rerun_waits_total",
		}, []string{"namespace", "runner_group", "tier", "outcome"}),
	}
}

// abandonedRerunFixture builds a provisioner whose GitHub calls land on tally, with a
// fake client seeded with pods, and a clock the test drives.
type abandonedRerunFixture struct {
	p     *Provisioner
	tally *rerunTally
	m     *runnercore.Metrics
	fc    client.Client
	clock time.Time
}

func newAbandonedRerunFixture(t *testing.T, maxRetries int, pods ...*corev1.Pod) *abandonedRerunFixture {
	t.Helper()

	tally := newRerunTally()
	srv := httptest.NewServer(tally.handler())
	t.Cleanup(srv.Close)

	objs := make([]client.Object, 0, len(pods))
	for _, pod := range pods {
		objs = append(objs, pod)
	}
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()

	f := &abandonedRerunFixture{
		tally: tally,
		m:     abandonedRerunMetrics(),
		fc:    fc,
		clock: time.Unix(1_700_000_000, 0),
	}
	f.p = &Provisioner{
		Client:             fc,
		Metrics:            f.m,
		TokenFunc:          func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL:       srv.URL,
		HTTPClient:         srv.Client(),
		MaxEvictionRetries: maxRetries,
		now:                func() time.Time { return f.clock },
	}
	return f
}

// target returns a stub owner whose worker pods carry the LabelRunnerSet selector the
// seeded pods use.
func (f *abandonedRerunFixture) target(name string, maxRetries int) *stubTarget {
	return &stubTarget{
		key:  client.ObjectKey{Namespace: "ns", Name: name},
		spec: &ResolvedSpec{MaxEvictionRetries: maxRetries},
	}
}

// workerPod builds a worker pod for owner, bound (PodScheduled=True) at boundAt when
// boundAt is non-zero and unschedulable otherwise.
func workerPod(name, owner string, boundAt time.Time) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    map[string]string{LabelRunnerSet: owner},
		},
	}
	cond := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable}
	if !boundAt.IsZero() {
		cond = corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(boundAt)}
	}
	pod.Status.Conditions = []corev1.PodCondition{cond}
	return pod
}

// sweepAndWait runs one sweep pass and blocks until every recovery it armed has
// finished its GitHub calls, so the per-run tallies below are read after the work
// rather than during it. wantArmed of -1 skips the count assertion.
func sweepAndWait(t *testing.T, p *Provisioner, ctx context.Context, wantArmed int) {
	t.Helper()
	done := p.sweepAbandonedReruns(ctx)
	if wantArmed >= 0 {
		require.Len(t, done, wantArmed, "recoveries armed by this sweep")
	}
	for _, ch := range done {
		<-ch
	}
}

// failingListInterceptor makes every List fail, standing in for a transient apiserver
// or cache failure while the sweeper is deciding.
func failingListInterceptor() interceptor.Funcs {
	return interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("list failed")
		},
	}
}

// TestAbandonedRerun_WaitsForCapacityBeforeReRunning is the core Q691 behaviour: a
// force-cancelled abandoned run is NOT re-run while the owner still cannot place a
// worker pod, and IS re-run once one binds.
//
// Firing immediately is the failure this defers: the job was abandoned because its
// worker sat Pending past pendingPodDeadline, so an instant re-run re-queues it into
// the pool that was starved.
func TestAbandonedRerun_WaitsForCapacityBeforeReRunning(t *testing.T) {
	ctx := context.Background()
	stuck := workerPod("stuck", "g", time.Time{})
	f := newAbandonedRerunFixture(t, 2, stuck)
	target := f.target("g", 2)

	f.p.registerAbandonedRerun(target, "owner", "repo", "4242", evictionTierClassic)

	// Pool still starved: the only worker pod is unschedulable.
	f.clock = f.clock.Add(time.Minute)
	sweepAndWait(t, f.p, ctx, 0)
	assert.Equal(t, 0, f.tally.for_("4242"), "no re-run while the pool cannot place a worker")
	assert.Equal(t, float64(0),
		testutil.ToFloat64(f.m.AbandonedRunRerunWaits.WithLabelValues("ns", "g", evictionTierClassic, abandonedRerunOutcomeCapacityReturned)))

	// A worker binds: capacity is back.
	bound := workerPod("bound", "g", f.clock)
	require.NoError(t, f.fc.Create(ctx, bound))

	f.clock = f.clock.Add(time.Minute)
	sweepAndWait(t, f.p, ctx, 1)
	assert.Equal(t, 1, f.tally.for_("4242"), "the re-run fires once a worker pod is placed")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(f.m.AbandonedRunRerunWaits.WithLabelValues("ns", "g", evictionTierClassic, abandonedRerunOutcomeCapacityReturned)))
	assert.Equal(t, float64(1),
		testutil.ToFloat64(f.m.EvictionRetries.WithLabelValues("ns", "g", evictionTierClassic, recoveryCauseAbandoned)),
		"the recovery spends a slot of the shared per-run budget, labelled abandoned")

	// The entry is consumed, so a later sweep does not re-run the same run again.
	f.clock = f.clock.Add(time.Minute)
	sweepAndWait(t, f.p, ctx, 0)
	assert.Equal(t, 1, f.tally.for_("4242"), "recovery is at-most-once per abandonment")
}

// TestAbandonedRerun_PodBoundBeforeTheAbandonmentIsNotEvidence pins the direction of
// the capacity test. The abandoned job's own worker may itself have bound (scheduled,
// then removed before its containers started), and a pod that was already placed when
// the run was abandoned proves nothing about the pool NOW — treating it as evidence
// would make the trigger fire immediately and turn Q691 into the storm it exists to
// avoid.
func TestAbandonedRerun_PodBoundBeforeTheAbandonmentIsNotEvidence(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedRerunFixture(t, 2)
	target := f.target("g", 2)

	old := workerPod("earlier", "g", f.clock.Add(-time.Hour))
	require.NoError(t, f.fc.Create(ctx, old))

	f.p.registerAbandonedRerun(target, "owner", "repo", "77", evictionTierClassic)
	f.clock = f.clock.Add(time.Minute)

	sweepAndWait(t, f.p, ctx, 0)
	assert.Equal(t, 0, f.tally.for_("77"),
		"a binding that predates the abandonment is not evidence that capacity returned")
}

// TestAbandonedRerun_ExpiresWhenCapacityNeverReturns bounds the wait. Capacity may
// never come back (an idle group, or a pendingPodDeadline reap caused by an unpullable
// image rather than a shortage), so an entry that waits out the window is dropped and
// counted rather than held forever.
func TestAbandonedRerun_ExpiresWhenCapacityNeverReturns(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedRerunFixture(t, 2, workerPod("stuck", "g", time.Time{}))
	f.p.AbandonedRerunWaitWindow = 10 * time.Minute
	target := f.target("g", 2)

	f.p.registerAbandonedRerun(target, "owner", "repo", "555", evictionTierClassic)

	f.clock = f.clock.Add(9 * time.Minute)
	sweepAndWait(t, f.p, ctx, 0)
	assert.Equal(t, float64(0),
		testutil.ToFloat64(f.m.AbandonedRunRerunWaits.WithLabelValues("ns", "g", evictionTierClassic, abandonedRerunOutcomeExpired)),
		"still inside the window")

	f.clock = f.clock.Add(2 * time.Minute)
	sweepAndWait(t, f.p, ctx, 0)
	assert.Equal(t, float64(1),
		testutil.ToFloat64(f.m.AbandonedRunRerunWaits.WithLabelValues("ns", "g", evictionTierClassic, abandonedRerunOutcomeExpired)),
		"a wait that never saw capacity is an operator-visible ending, not a silent drop")
	assert.Equal(t, 0, f.tally.for_("555"))

	// Even with capacity back afterwards, the expired entry is gone for good.
	require.NoError(t, f.fc.Create(ctx, workerPod("bound", "g", f.clock)))
	f.clock = f.clock.Add(time.Minute)
	sweepAndWait(t, f.p, ctx, 0)
	assert.Equal(t, 0, f.tally.for_("555"))
}

// TestAbandonedRerun_LoopBudgetBindsPerRun is the loop-bound test the Q691 row demands.
//
// A re-run re-queues the job into the pool that starved it, so a run can be abandoned,
// re-run, and abandoned again indefinitely. The bound is the shared per-run_id retry
// budget (Q106): each recovery reserves one slot, and the run stops being re-run once
// maxEvictionRetries are spent.
//
// It is asserted PER RUN, over two distinct runs abandoned in the same cycles, because
// an aggregate call count cannot distinguish "each run got its budget" from "one run
// consumed everything" — the run_id is the budget's key, so the run_id has to be the
// assertion's key too. Run B's budget must survive run A exhausting its own.
//
// Deleting the reserveEvictionRetry gate in handleEviction turns these per-run counts
// from maxRetries into cycles, which is the red this test exists to produce.
func TestAbandonedRerun_LoopBudgetBindsPerRun(t *testing.T) {
	const (
		maxRetries = 2
		cycles     = 6
	)
	ctx := context.Background()
	f := newAbandonedRerunFixture(t, maxRetries)
	target := f.target("g", maxRetries)

	// Each cycle: both runs are abandoned and force-cancelled, then a worker binds
	// (capacity returned), then the sweeper re-runs whatever the budget still allows.
	// Without the budget this loop would issue `cycles` re-runs for each run.
	for i := 0; i < cycles; i++ {
		f.p.registerAbandonedRerun(target, "owner", "repo", "runA", evictionTierClassic)
		f.p.registerAbandonedRerun(target, "owner", "repo", "runB", evictionTierClassic)

		f.clock = f.clock.Add(time.Minute)
		require.NoError(t, f.fc.Create(ctx, workerPod("bound-"+strconv.Itoa(i), "g", f.clock)))

		f.clock = f.clock.Add(time.Minute)
		sweepAndWait(t, f.p, ctx, -1)
	}

	assert.Equal(t, maxRetries, f.tally.for_("runA"),
		"run A must be re-run at most maxEvictionRetries times however often it is abandoned")
	assert.Equal(t, maxRetries, f.tally.for_("runB"),
		"run B has its own budget: run A looping must not consume or extend it")

	// Both runs' exhaustions are visible, and the counter cannot be satisfied by one
	// run alone: it must have counted the cycles both runs were refused.
	exhausted := testutil.ToFloat64(
		f.m.EvictionRetriesExhausted.WithLabelValues("ns", "g", evictionTierClassic, recoveryCauseAbandoned))
	assert.Equal(t, float64(2*(cycles-maxRetries)), exhausted,
		"every refused recovery is counted, for both runs")
	assert.Contains(t, target.events, "EvictionRetriesExhausted",
		"budget exhaustion must reach the owner object, not only a metric")
}

// TestAbandonedRerun_OneRunOneEntry keeps two abandoned jobs of the same run from
// spending two budget slots. rerun-failed-jobs is a run-level call, so the second job
// has nothing of its own to re-run.
func TestAbandonedRerun_OneRunOneEntry(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedRerunFixture(t, 3)
	target := f.target("g", 3)

	f.p.registerAbandonedRerun(target, "owner", "repo", "9001", evictionTierClassic)
	f.p.registerAbandonedRerun(target, "owner", "repo", "9001", evictionTierClassic)

	f.clock = f.clock.Add(time.Minute)
	require.NoError(t, f.fc.Create(ctx, workerPod("bound", "g", f.clock)))
	f.clock = f.clock.Add(time.Minute)

	sweepAndWait(t, f.p, ctx, 1)
	assert.Equal(t, 1, f.tally.for_("9001"),
		"two abandoned jobs of one run are one run-level re-run and one budget slot")
}

// TestAbandonedRerun_OnlyTheCancelledOutcomeRegisters pins registration to the state
// the Q683 measurement covers. identity_unknown has no endpoint to address, and after
// a refused force-cancel the run was never concluded by us, so neither is a run whose
// re-run GitHub was measured to accept.
func TestAbandonedRerun_OnlyTheCancelledOutcomeRegisters(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name           string
		owner, repo    string
		runID          string
		forceCancelSt  int
		wantRegistered bool
	}{
		{name: "cancelled", owner: "o", repo: "r", runID: "1", forceCancelSt: http.StatusAccepted, wantRegistered: true},
		{name: "identity unknown", owner: "", repo: "", runID: "", forceCancelSt: http.StatusAccepted},
		{name: "force-cancel refused", owner: "o", repo: "r", runID: "3", forceCancelSt: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.forceCancelSt)
			}))
			t.Cleanup(srv.Close)

			p := &Provisioner{
				TokenFunc:    func(context.Context) (string, error) { return "tok", nil },
				GitHubAPIURL: srv.URL,
				HTTPClient:   srv.Client(),
			}
			target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}

			p.forceCancelAbandonedRun(ctx, target, tc.owner, tc.repo, tc.runID, evictionTierClassic, p.logFor())

			assert.Equal(t, tc.wantRegistered, len(p.pendingAbandonedRerunKeys()) == 1,
				"only a run we know concluded cancelled is re-runnable")
		})
	}
}

// TestAbandonedRerun_UnreadablePodsDeferRatherThanDrop keeps a transient API failure
// from consuming a recovery. The list is how capacity is observed, so a failed read
// decided nothing and the entry must survive to be re-asked.
func TestAbandonedRerun_UnreadablePodsDeferRatherThanDrop(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedRerunFixture(t, 2)
	target := f.target("g", 2)
	f.p.registerAbandonedRerun(target, "owner", "repo", "31337", evictionTierClassic)

	// A client whose List always fails.
	f.p.Client = fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(failingListInterceptor()).
		Build()

	f.clock = f.clock.Add(time.Minute)
	sweepAndWait(t, f.p, ctx, 0)
	require.Len(t, f.p.pendingAbandonedRerunKeys(), 1, "an unreadable pod list must not spend the wait")

	// Restore a readable client with capacity back: the retained entry still recovers.
	f.p.Client = f.fc
	require.NoError(t, f.fc.Create(ctx, workerPod("bound", "g", f.clock)))
	f.clock = f.clock.Add(time.Minute)
	sweepAndWait(t, f.p, ctx, 1)
	assert.Equal(t, 1, f.tally.for_("31337"))
}
