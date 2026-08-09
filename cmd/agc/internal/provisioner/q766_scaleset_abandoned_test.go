package provisioner

import (
	"context"
	"net/http"
	"net/http/httptest"
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
)

// Q766: the abandoned-run force-cancel (Q683) and capacity-gated auto re-run (Q691) on
// the scale-set tier. Both were wired into the classic provision() goroutine only, so no
// v2beta1 tenant had either.
//
// The assertions below split on WHICH GitHub endpoint is called, because that is the
// whole safety argument. A never-started worker's job produced no failure, so sending it
// to rerun-failed-jobs — the disruption path — would re-run a job that never ran. It has
// to be force-cancelled first, and only then re-run.

// githubCalls records the endpoint each POST addressed, so a test can tell a
// force-cancel from a re-run rather than counting calls.
type githubCalls struct {
	mu          sync.Mutex
	forceCancel []string
	rerun       []string
}

func (g *githubCalls) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/force-cancel"):
			g.forceCancel = append(g.forceCancel, r.URL.Path)
		case strings.HasSuffix(r.URL.Path, "/rerun-failed-jobs"):
			g.rerun = append(g.rerun, r.URL.Path)
		}
		g.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (g *githubCalls) snapshot() (forceCancel, rerun []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.forceCancel...), append([]string(nil), g.rerun...)
}

// abandonedScaleSetMetrics carries the tier label Q766 added to both abandoned-run
// counters, alongside the eviction counters the re-run continues on.
func abandonedScaleSetMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q766_eviction_retries_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q766_eviction_retries_exhausted_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRecoveryIdentityUnknown: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q766_eviction_recovery_identity_unknown_total",
		}, []string{"namespace", "runner_group", "cause"}),
		AbandonedRunForceCancels: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q766_abandoned_run_force_cancels_total",
		}, []string{"namespace", "runner_group", "tier", "outcome"}),
		AbandonedRunRerunWaits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q766_abandoned_run_rerun_waits_total",
		}, []string{"namespace", "runner_group", "tier", "outcome"}),
	}
}

// abandonedScaleSetFixture wires a Provisioner whose GitHub calls land on calls, with a
// fake client seeded with pods and a clock the test drives.
type abandonedScaleSetFixture struct {
	p      *Provisioner
	calls  *githubCalls
	m      *runnercore.Metrics
	fc     client.Client
	target *stubTarget
	clock  time.Time
}

func newAbandonedScaleSetFixture(t *testing.T, status int, pods ...*corev1.Pod) *abandonedScaleSetFixture {
	t.Helper()

	calls := &githubCalls{}
	srv := httptest.NewServer(calls.handler(status))
	t.Cleanup(srv.Close)

	builder := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme)
	for _, pod := range pods {
		builder = builder.WithObjects(pod)
	}
	fc := builder.Build()

	f := &abandonedScaleSetFixture{
		calls: calls,
		m:     abandonedScaleSetMetrics(),
		fc:    fc,
		clock: time.Unix(1_700_000_000, 0),
		target: &stubTarget{
			key:  client.ObjectKey{Namespace: "team-a", Name: "gpu"},
			spec: &ResolvedSpec{MaxEvictionRetries: 2},
		},
	}
	f.p = NewProvisioner(fc, f.m, nil)
	f.p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	f.p.GitHubAPIURL = srv.URL
	f.p.HTTPClient = srv.Client()
	f.p.now = func() time.Time { return f.clock }
	return f
}

// reapedPendingWorker is a scale-set worker the pendingPodDeadline reap just deleted: it
// never left Pending, so no container status exists, and the reaper stamped the deletion
// as the AGC's own before issuing it.
func reapedPendingWorker(name string) *corev1.Pod {
	pod := scaleSetWorkerPod(name, identityAnnotations())
	pod.Annotations[AnnotationDeletionReason] = "pending_deadline"
	pod.Status.Phase = corev1.PodPending
	return pod
}

// TestQ766_ReapedPendingScaleSetWorker_ForceCancelsThenRerunsOnCapacity is the headline
// port: on the scale-set tier a worker reaped while Pending now gets the same one-second
// cancelled ending and the same deferred re-run the classic tier has had since Q683/Q691.
//
// It asserts the ORDER, not just that both calls happen. The force-cancel has to land
// first — a run that concluded green refuses rerun-failed-jobs with a 403 (measured
// 2026-08-05), so a re-run fired before the cancel would silently do nothing.
func TestQ766_ReapedPendingScaleSetWorker_ForceCancelsThenRerunsOnCapacity(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted)

	<-f.p.RecoverAbandonedScaleSetWorker(ctx, f.target, reapedPendingWorker("runner-gpu-job1"))

	forceCancel, rerun := f.calls.snapshot()
	require.Len(t, forceCancel, 1, "the reaped worker's run must be force-cancelled")
	assert.Equal(t, "/repos/myorg/myrepo/actions/runs/4242/force-cancel", forceCancel[0],
		"the run identity comes off the worker pod's own annotations; this tier records it nowhere else")
	assert.Empty(t, rerun, "the re-run must wait for capacity, not fire alongside the cancel")

	assert.Equal(t, 1.0, testutil.ToFloat64(
		f.m.AbandonedRunForceCancels.WithLabelValues("team-a", "gpu", evictionTierScaleSet, "cancelled")),
		"the force-cancel is attributed to the scale-set tier, so an operator can tell the two apart")

	// Capacity returns: a worker pod of the same owner binds after the abandonment.
	f.clock = f.clock.Add(time.Minute)
	bound := workerPod("bound", "gpu", f.clock)
	bound.Namespace = "team-a"
	require.NoError(t, f.fc.Create(ctx, bound))
	f.clock = f.clock.Add(time.Minute)

	for _, ch := range f.p.sweepAbandonedReruns(ctx) {
		<-ch
	}

	_, rerun = f.calls.snapshot()
	require.Len(t, rerun, 1, "the cancelled run is re-run once the owner can place a worker again")
	assert.Equal(t, "/repos/myorg/myrepo/actions/runs/4242/rerun-failed-jobs", rerun[0])
	assert.Equal(t, 1.0, testutil.ToFloat64(
		f.m.AbandonedRunRerunWaits.WithLabelValues("team-a", "gpu", evictionTierScaleSet, abandonedRerunOutcomeCapacityReturned)))
	assert.Equal(t, 1.0, testutil.ToFloat64(
		f.m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseAbandoned)),
		"the re-run spends a slot of the shared per-run budget, labelled by the tier that detected it")
}

// TestQ766_ClassicWorkerIsNotRecoveredTwice keeps the port from double-spending the
// shared retry budget. A classic worker's abandonment is already handled by the
// goroutine that owns the pod; the reaper runs over both tiers' pods, so without the
// label gate the same run would be cancelled and re-run twice.
func TestQ766_ClassicWorkerIsNotRecoveredTwice(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted)

	classic := reapedPendingWorker("runner-classic-job1")
	delete(classic.Labels, LabelAcquisitionProtocol)

	<-f.p.RecoverAbandonedScaleSetWorker(ctx, f.target, classic)

	forceCancel, _ := f.calls.snapshot()
	assert.Empty(t, forceCancel, "a classic worker's recovery belongs to its own provision goroutine")
	assert.Empty(t, f.p.pendingAbandonedRerunKeys(), "and so does its re-run registration")
}

// TestQ766_IdentityUnknownIsSurfaced covers the failure mode that makes the whole
// mechanism silently inert on this tier: an assignment message that carried no run
// identity leaves the worker pod unannotated, so there is no run to address.
//
// It is counted as the tier's identity-unknown failure rather than as a force-cancel
// outcome, which keeps the two series disjoint — on this tier
// abandoned_run_force_cancels_total{outcome="identity_unknown"} is unreachable.
func TestQ766_IdentityUnknownIsSurfaced(t *testing.T) {
	ctx := context.Background()
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted)

	pod := reapedPendingWorker("runner-gpu-noident")
	delete(pod.Annotations, AnnotationRunID)

	<-f.p.RecoverAbandonedScaleSetWorker(ctx, f.target, pod)

	forceCancel, _ := f.calls.snapshot()
	assert.Empty(t, forceCancel, "there is no run to address, so no endpoint is guessed at")
	assert.Equal(t, 1.0, testutil.ToFloat64(
		f.m.EvictionRecoveryIdentityUnknown.WithLabelValues("team-a", "gpu", recoveryCauseAbandoned)))
	assert.Equal(t, 0.0, testutil.ToFloat64(
		f.m.AbandonedRunForceCancels.WithLabelValues("team-a", "gpu", evictionTierScaleSet, "identity_unknown")),
		"the two identity-unknown series must not both count the same pod")
	assert.Contains(t, f.target.events, "EvictionRecoveryIdentityUnknown",
		"an operator seeing abandoned jobs stay cancelled must be told why, not left to infer it")
}

// TestQ766_AbandonedAwaitingRecovery is the predicate boundary for the scan's arm — the
// externally-deleted never-started worker, the one shape
// externallyDeletedBeforeTerminal deliberately excludes.
//
// Each false case is a shape that must NOT be force-cancelled, and each would be a
// distinct defect: re-running a live job, cancelling a run the AGC deliberately gave up
// on twice over, or re-running a job that genuinely failed.
func TestQ766_AbandonedAwaitingRecovery(t *testing.T) {
	deleted := func(pod *corev1.Pod) *corev1.Pod {
		pod.DeletionTimestamp = &metav1.Time{Time: time.Unix(1_700_000_000, 0)}
		pod.Finalizers = []string{"test/keep"}
		pod.Status.Phase = corev1.PodFailed
		return pod
	}
	started := func(pod *corev1.Pod) *corev1.Pod {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "runner",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				FinishedAt: metav1.NewTime(time.Unix(1_700_000_000, 0)),
			}},
		}}
		return pod
	}

	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "externally deleted before any container ran",
			pod:  deleted(scaleSetWorkerPod("a", identityAnnotations())),
			want: true,
		},
		{
			name: "the AGC's own reap",
			// The reaper hook already recovers this one, synchronously with the delete.
			// Recovering the transient Failed it publishes would be the second time.
			pod: func() *corev1.Pod {
				pod := deleted(scaleSetWorkerPod("b", identityAnnotations()))
				pod.Annotations[AnnotationDeletionReason] = "pending_deadline"
				return pod
			}(),
		},
		{
			name: "already adjudicated",
			pod: func() *corev1.Pod {
				pod := deleted(scaleSetWorkerPod("c", identityAnnotations()))
				pod.Annotations[AnnotationEvictionHandledAt] = "2026-08-09T00:00:00Z"
				return pod
			}(),
		},
		{
			name: "deleted mid-run",
			// A container ran, so there is a real failed job: the disruption path owns
			// it, and rerun-failed-jobs can act on it without a cancel.
			pod: started(deleted(scaleSetWorkerPod("d", identityAnnotations()))),
		},
		{
			name: "not being deleted at all",
			pod: func() *corev1.Pod {
				pod := scaleSetWorkerPod("e", identityAnnotations())
				pod.Status.Phase = corev1.PodFailed
				return pod
			}(),
		},
		{
			name: "still Pending under a deletion",
			// The terminal phase has not published yet; nothing is decided.
			pod: func() *corev1.Pod {
				pod := deleted(scaleSetWorkerPod("f", identityAnnotations()))
				pod.Status.Phase = corev1.PodPending
				return pod
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, abandonedAwaitingRecovery(tc.pod))
		})
	}
}

// TestQ766_ScanSendsNeverStartedToForceCancelAndDisruptedToRerun is the "do not add a
// fourth arm" assertion, made on the endpoints rather than on the code shape.
//
// Both pods are externally deleted scale-set workers in PodFailed. The only difference is
// whether a container ever ran, and that difference has to route them to DIFFERENT GitHub
// calls: the one that ran has a failed job to re-run, the one that never started has a
// run to cancel first.
func TestQ766_ScanSendsNeverStartedToForceCancelAndDisruptedToRerun(t *testing.T) {
	ctx := context.Background()

	deletedAt := time.Unix(1_700_000_000, 0)
	mark := func(pod *corev1.Pod) *corev1.Pod {
		pod.DeletionTimestamp = &metav1.Time{Time: deletedAt}
		pod.DeletionGracePeriodSeconds = ptrInt64(30)
		pod.Finalizers = []string{"test/keep"}
		pod.Status.Phase = corev1.PodFailed
		return pod
	}

	neverStarted := mark(scaleSetWorkerPod("runner-gpu-pending", map[string]string{
		AnnotationRunID: "111", AnnotationRepository: "myorg/myrepo",
	}))
	ranAndDied := mark(scaleSetWorkerPod("runner-gpu-running", map[string]string{
		AnnotationRunID: "222", AnnotationRepository: "myorg/myrepo",
	}))
	// The delete request (deletionTimestamp less the grace period) predates the exit, so
	// this is the drained-worker shape Q502 recovers.
	ranAndDied.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runner",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			FinishedAt: metav1.NewTime(deletedAt.Add(10 * time.Second)),
		}},
	}}

	f := newAbandonedScaleSetFixture(t, http.StatusAccepted, neverStarted, ranAndDied)

	done, err := f.p.RecoverEvictedScaleSetWorkers(ctx, f.target)
	require.NoError(t, err)
	<-done

	forceCancel, rerun := f.calls.snapshot()
	assert.Equal(t, []string{"/repos/myorg/myrepo/actions/runs/111/force-cancel"}, forceCancel,
		"a worker that never started has no failed job to re-run, so its run is concluded instead")
	assert.Equal(t, []string{"/repos/myorg/myrepo/actions/runs/222/rerun-failed-jobs"}, rerun,
		"a worker deleted mid-run keeps the unchanged Q502 disruption path")

	// The never-started run is queued for its capacity-gated re-run; the disrupted one
	// was re-run at once and never enters the wait.
	keys := f.p.pendingAbandonedRerunKeys()
	require.Len(t, keys, 1)
	assert.Equal(t, "111", keys[0].runID)
}

func ptrInt64(v int64) *int64 { return &v }
