package provisioner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Q417: eviction recovery on the scale-set tier. The capability existed only on
// classic, where provision() watches the pod it created and still holds the acquired
// payload's run identity. The scale-set tier has neither, so identity is stamped on the
// worker pod at creation and the owning reconciler drives detection —
// RecoverEvictedScaleSetWorkers. These tests pin both halves plus the properties that
// keep the mechanism from firing twice or firing on the wrong tier.

// evictionRecoveryMetrics builds unregistered counters for the eviction paths, with the
// tier label the real ones carry.
func evictionRecoveryMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q417_eviction_retries_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q417_eviction_retries_exhausted_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRecoveryIdentityUnknown: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q417_eviction_recovery_identity_unknown_total",
		}, []string{"namespace", "runner_group", "cause"}),
		EvictionRecoveryEvidenceLost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q809_eviction_recovery_evidence_lost_total",
		}, []string{"namespace", "runner_group", "cause"}),
	}
}

// scaleSetWorkerPod builds a worker pod as ProvisionScaleSetWorker would have created
// it: the owner label, the scale-set tier marker, and the run-identity annotations.
func scaleSetWorkerPod(name string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "team-a",
			Labels: map[string]string{
				LabelRunnerSet:           "gpu",
				LabelAcquisitionProtocol: AcquisitionProtocolScaleSet,
			},
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
	}
}

// evicted marks a pod as the kubelet does when it evicts it under node pressure.
func evicted(pod *corev1.Pod) *corev1.Pod {
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = podReasonEvicted
	return pod
}

// identityAnnotations is the run identity a scale-set assignment carrying full
// JobMessageBase fields produces on the worker pod.
func identityAnnotations() map[string]string {
	return map[string]string{
		AnnotationRunID:      "4242",
		AnnotationRepository: "myorg/myrepo",
	}
}

// recoveryFixture wires a Provisioner against a fake client and a counting stand-in for
// the rerun-failed-jobs endpoint, returning the paths it was called with.
func recoveryFixture(t *testing.T, pods ...*corev1.Pod) (*Provisioner, *stubTarget, *runnercore.Metrics, *atomic.Int64, chan string) {
	t.Helper()
	return recoveryFixtureWith(t, interceptor.Funcs{}, pods...)
}

// recoveryFixtureWith is recoveryFixture with the fake client's calls intercepted, for
// the apiserver races the claim path has to classify (Q809).
func recoveryFixtureWith(t *testing.T, ic interceptor.Funcs, pods ...*corev1.Pod) (*Provisioner, *stubTarget, *runnercore.Metrics, *atomic.Int64, chan string) {
	t.Helper()

	rerunCount := &atomic.Int64{}
	paths := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answeredRunConclusion(w, r, runConcludedFailure) {
			return
		}
		rerunCount.Add(1)
		select {
		case paths <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	builder := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme)
	for _, pod := range pods {
		builder = builder.WithObjects(pod)
	}
	fc := builder.WithInterceptorFuncs(ic).Build()

	m := evictionRecoveryMetrics()
	p := NewProvisioner(fc, m, nil)
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	target := &stubTarget{
		key: client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		spec: &ResolvedSpec{
			WorkerImage:        "runner:test",
			MaxEvictionRetries: 2,
			// Zero delay keeps the test fast; the delay itself is handleEviction's,
			// already covered by the classic tests.
			EvictionRetryDelay: 0,
		},
	}
	return p, target, m, rerunCount, paths
}

// TestRecoverEvictedScaleSetWorkers_RerunsTheEvictedRun is the headline behaviour: an
// evicted scale-set worker gets its run re-run automatically, which before Q417 required
// a human. It asserts the run named in the API call comes from the pod's own
// annotations — the only place this tier records it.
func TestRecoverEvictedScaleSetWorkers_RerunsTheEvictedRun(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, paths := recoveryFixture(t,
		evicted(scaleSetWorkerPod("runner-gpu-job1", identityAnnotations())))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	require.Equal(t, int64(1), rerunCount.Load(), "the evicted run must be re-run exactly once")
	select {
	case path := <-paths:
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs/4242/rerun-failed-jobs", path)
	default:
		t.Fatal("rerun API path was not recorded")
	}

	// The recovery is attributed to the scale-set tier, so an operator can assert the
	// mechanism works on the tier a v2beta1 tenant actually runs on.
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction)))
	assert.Equal(t, float64(0),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierClassic, recoveryCauseEviction)))

	// The pod is claimed, which is what makes the next reconcile skip it.
	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "runner-gpu-job1"}, &pod))
	handled, ok := pod.Annotations[AnnotationEvictionHandledAt]
	require.True(t, ok, "the evicted pod must be stamped as handled")
	_, perr := time.Parse(time.RFC3339, handled)
	assert.NoError(t, perr, "the handled stamp must be a parseable RFC 3339 time")
}

// TestRecoverEvictedScaleSetWorkers_AtMostOncePerPod is the property that makes the
// mechanism safe to run every reconcile. The reconciler sees the same terminal pod for
// as long as completedPodTTL retains it, so without the set-once claim each reconcile
// would spend another slot of the run's retry budget on one eviction.
func TestRecoverEvictedScaleSetWorkers_AtMostOncePerPod(t *testing.T) {
	ctx := context.Background()
	p, target, _, rerunCount, _ := recoveryFixture(t,
		evicted(scaleSetWorkerPod("runner-gpu-job1", identityAnnotations())))

	for i := 0; i < 3; i++ {
		done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
		require.NoError(t, err)
		<-done
	}

	assert.Equal(t, int64(1), rerunCount.Load(),
		"repeated reconciles of one evicted pod must produce exactly one re-run")
}

// TestRecoverEvictedScaleSetWorkers_IgnoresClassicWorkers guards the double-report the
// design constraint calls out. A classic worker's eviction is already handled inline by
// the provision() goroutine that owns it; if this pass also fired on classic pods, one
// eviction would consume two slots of the run's budget.
func TestRecoverEvictedScaleSetWorkers_IgnoresClassicWorkers(t *testing.T) {
	ctx := context.Background()
	classic := evicted(scaleSetWorkerPod("runner-gpu-classic", identityAnnotations()))
	delete(classic.Labels, LabelAcquisitionProtocol)

	p, target, _, rerunCount, _ := recoveryFixture(t, classic)

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(),
		"a worker with no scale-set tier marker is the classic path's to recover")
	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "runner-gpu-classic"}, &pod))
	assert.NotContains(t, pod.Annotations, AnnotationEvictionHandledAt)
}

// TestRecoverEvictedScaleSetWorkers_IgnoresNonEvictionOutcomes pins the phase/reason
// test. A pod that failed for any reason other than a kubelet eviction ran its job and
// reported its own outcome, so re-running it would both double-report and burn budget.
func TestRecoverEvictedScaleSetWorkers_IgnoresNonEvictionOutcomes(t *testing.T) {
	ctx := context.Background()

	failedNotEvicted := scaleSetWorkerPod("runner-gpu-failed", identityAnnotations())
	failedNotEvicted.Status.Phase = corev1.PodFailed
	failedNotEvicted.Status.Reason = "" // an ordinary non-zero runner exit

	succeeded := scaleSetWorkerPod("runner-gpu-ok", identityAnnotations())
	succeeded.Status.Phase = corev1.PodSucceeded

	running := scaleSetWorkerPod("runner-gpu-live", identityAnnotations())
	running.Status.Phase = corev1.PodRunning

	p, target, _, rerunCount, _ := recoveryFixture(t, failedNotEvicted, succeeded, running)

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(),
		"only PodFailed/Evicted is a kubelet eviction; nothing else may trigger a re-run")
}

// TestRecoverEvictedScaleSetWorkers_IdentityUnknownIsSurfaced covers the one failure
// mode that makes the whole mechanism inert: an assignment that carried no workflow-run
// identity leaves nothing to re-run. It must be loud — a counter and an owner Event —
// rather than a silent no-op that looks indistinguishable from "no evictions happened".
func TestRecoverEvictedScaleSetWorkers_IdentityUnknownIsSurfaced(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, _ := recoveryFixture(t,
		evicted(scaleSetWorkerPod("runner-gpu-noid", nil)))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(), "there is no run to re-run")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRecoveryIdentityUnknown.WithLabelValues("team-a", "gpu", recoveryCauseEviction)))
	assert.Contains(t, target.events, "EvictionRecoveryIdentityUnknown")

	// Claimed anyway: the verdict is final for this pod, so later reconciles must not
	// re-adjudicate it and re-emit the same warning every time.
	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "runner-gpu-noid"}, &pod))
	assert.Contains(t, pod.Annotations, AnnotationEvictionHandledAt)
}

// TestRecoverEvictedScaleSetWorkers_BudgetExhaustionSurfaces verifies the shared Q106
// budget governs this tier too: past maxEvictionRetries evictions of one run, further
// evicted workers of that run get no re-run and the exhausted counter is attributed to
// the scale-set tier.
func TestRecoverEvictedScaleSetWorkers_BudgetExhaustionSurfaces(t *testing.T) {
	ctx := context.Background()
	pods := []*corev1.Pod{
		evicted(scaleSetWorkerPod("runner-gpu-a", identityAnnotations())),
		evicted(scaleSetWorkerPod("runner-gpu-b", identityAnnotations())),
		evicted(scaleSetWorkerPod("runner-gpu-c", identityAnnotations())),
	}
	p, target, m, rerunCount, _ := recoveryFixture(t, pods...)
	target.spec.MaxEvictionRetries = 2

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	// Three evicted workers, one run, a budget of two.
	assert.Equal(t, int64(2), rerunCount.Load())
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction)))
	assert.Contains(t, target.events, "EvictionRetriesExhausted")
}

// TestRecoverEvictedScaleSetWorkers_NoEvictedPodsIsCheap asserts the common path returns
// a usable closed channel and never resolves the spec — this runs on every reconcile of
// every ScaleSet RunnerSet, so it must cost one cached List and nothing more.
func TestRecoverEvictedScaleSetWorkers_NoEvictedPodsIsCheap(t *testing.T) {
	ctx := context.Background()
	p, target, _, rerunCount, _ := recoveryFixture(t,
		scaleSetWorkerPod("runner-gpu-live", identityAnnotations()))
	target.spec = nil // Resolve would nil-panic if it were consulted

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done // must not block

	assert.Equal(t, int64(0), rerunCount.Load())
}

// TestProvisionScaleSetWorker_StampsIdentityAndTier is the other half of the port: a
// scale-set worker pod must carry the run identity and the tier marker, because the pod
// is the only durable record either one has. Without this, recovery has nothing to read.
func TestProvisionScaleSetWorker_StampsIdentityAndTier(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)
	target := &stubTarget{
		key:  client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		spec: &ResolvedSpec{WorkerImage: "runner:test"},
	}

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{
		JobID:      "job-uuid-1",
		JITConfig:  "eyJ4IjoxfQ==",
		Owner:      "myorg",
		Repository: "myrepo",
		RunID:      "4242",
		JobName:    "build",
	}))

	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{
		Namespace: "team-a", Name: scaleSetPodName("gpu", "job-uuid-1"),
	}, &pod))

	assert.Equal(t, AcquisitionProtocolScaleSet, pod.Labels[LabelAcquisitionProtocol])
	assert.Equal(t, "4242", pod.Annotations[AnnotationRunID])
	assert.Equal(t, "myorg/myrepo", pod.Annotations[AnnotationRepository],
		"the protocol splits owner and repo; the annotation is the joined owner/repo the REST API takes")
	assert.Equal(t, "build", pod.Annotations[annotationJobName])
}

// TestProvisionScaleSetWorker_ProvisionsWithoutIdentity guards the degrade path: an
// assignment with no identity still gets a worker and still runs its job. Only automatic
// eviction recovery is lost, and RecoverEvictedScaleSetWorkers reports that loss.
func TestProvisionScaleSetWorker_ProvisionsWithoutIdentity(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := NewProvisioner(fc, nil, nil)
	target := &stubTarget{
		key:  client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		spec: &ResolvedSpec{WorkerImage: "runner:test"},
	}

	require.NoError(t, p.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{
		JobID:     "job-uuid-2",
		JITConfig: "eyJ4IjoxfQ==",
	}))

	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{
		Namespace: "team-a", Name: scaleSetPodName("gpu", "job-uuid-2"),
	}, &pod))
	assert.Equal(t, AcquisitionProtocolScaleSet, pod.Labels[LabelAcquisitionProtocol])
	assert.NotContains(t, pod.Annotations, AnnotationRunID)
}

// TestClaimEvictionRecovery_OptimisticLockRejectsStaleWriter is the test the
// at-most-once property actually rests on. Two AGC replicas reconciling the same
// RunnerSet both read the pod before either writes, so both see an unhandled eviction;
// only the optimistic lock stops both from claiming it and calling rerun-failed-jobs.
// A plain merge patch would let the second write succeed against the newer object.
func TestClaimEvictionRecovery_OptimisticLockRejectsStaleWriter(t *testing.T) {
	ctx := context.Background()
	p, _, _, _, _ := recoveryFixture(t,
		evicted(scaleSetWorkerPod("runner-gpu-race", identityAnnotations())))
	key := client.ObjectKey{Namespace: "team-a", Name: "runner-gpu-race"}

	// Two readers of the same pre-write generation — the replica race, deterministically.
	var first, second corev1.Pod
	require.NoError(t, p.Client.Get(ctx, key, &first))
	require.NoError(t, p.Client.Get(ctx, key, &second))

	require.NoError(t, p.claimEvictionRecovery(ctx, &first), "the first claim must win")

	err := p.claimEvictionRecovery(ctx, &second)
	require.Error(t, err, "the second claim must be rejected, not silently applied")
	assert.True(t, apierrors.IsConflict(err),
		"a lost claim must be a Conflict so the caller recognises it and skips: got %v", err)
}

// drainedWorker builds the disruption arm Q809's flake lives on: a scale-set worker
// deleted while running, whose container then recorded an exit — the drain shape
// externallyDeletedBeforeTerminal admits.
func drainedWorker(name string) *corev1.Pod {
	pod := scaleSetWorkerPod(name, identityAnnotations())
	deletedAt := metav1.NewTime(time.Now().Add(-40 * time.Second))
	grace := int64(30)
	pod.DeletionTimestamp = &deletedAt
	pod.DeletionGracePeriodSeconds = &grace
	pod.Finalizers = []string{"test.actions-gateway.com/hold"} // a fake client rejects a deletionTimestamp without one
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runner",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode:   1,
			FinishedAt: metav1.NewTime(deletedAt.Add(-25 * time.Second)),
		}},
	}}
	return pod
}

// TestClaimEvictionRecovery_RetriesAConflictFromANonClaimant is the Q809 fix on the
// conflict arm. The optimistic lock is there to arbitrate between claimants, but the
// apiserver raises the same Conflict for any concurrent write — and the kubelet
// publishing the terminal phase is guaranteed to be racing, because that transition is
// the edge that triggers the reconcile. Before the fix that conflict was read as
// "already claimed elsewhere" and the recovery was dropped, which is how run
// 31556806760 lost a drained worker's re-run.
func TestClaimEvictionRecovery_RetriesAConflictFromANonClaimant(t *testing.T) {
	ctx := context.Background()
	var patches atomic.Int64
	p, target, _, rerunCount, _ := recoveryFixtureWith(t, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if patches.Add(1) == 1 {
				// The kubelet's status write landing between our read and our patch.
				return apierrors.NewConflict(
					corev1.Resource("pods"), obj.GetName(), errors.New("the object has been modified"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}, drainedWorker("runner-gpu-drained"))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(1), rerunCount.Load(),
		"a conflict raised by a writer that is not a claimant must not cost the disruption its re-run")
	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "runner-gpu-drained"}, &pod))
	assert.Contains(t, pod.Annotations, AnnotationEvictionHandledAt)
}

// TestClaimEvictionRecovery_ConflictFromARealClaimantStillSkips is the other half, and
// the one the retry must not break: when the fresh object shows someone else already
// stamped the claim, this is the replica race the optimistic lock exists to lose, and
// retrying would spend a second slot of the run's retry budget on one disruption.
func TestClaimEvictionRecovery_ConflictFromARealClaimantStillSkips(t *testing.T) {
	ctx := context.Background()
	var patches atomic.Int64
	p, target, _, rerunCount, _ := recoveryFixtureWith(t, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if patches.Add(1) > 1 {
				return c.Patch(ctx, obj, patch, opts...)
			}
			// The other replica wins the claim, then our stale patch is rejected.
			var winner corev1.Pod
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &winner); err != nil {
				return err
			}
			winner.Annotations[AnnotationEvictionHandledAt] = "2026-08-12T00:00:00Z"
			if err := c.Update(ctx, &winner); err != nil {
				return err
			}
			return apierrors.NewConflict(
				corev1.Resource("pods"), obj.GetName(), errors.New("the object has been modified"))
		},
	}, drainedWorker("runner-gpu-raced"))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(),
		"the replica that lost the claim must not also re-run the job")
	assert.Equal(t, int64(1), patches.Load(), "a claim already held elsewhere must not be retried")
	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "runner-gpu-raced"}, &pod))
	assert.Equal(t, "2026-08-12T00:00:00Z", pod.Annotations[AnnotationEvictionHandledAt],
		"the winner's stamp must survive")
}

// TestRecoverEvictedScaleSetWorkers_EvidenceLostIsSurfaced covers the arm that cost
// Q809 two of its three CI failures: the kubelet finished tearing the drained pod down
// between the cached List and the claim patch. The pod is the disruption's only record,
// so nothing recovers it and no later reconcile can — which makes silence the whole
// problem. It must be loud, exactly as an unknown identity is.
//
// Deliberately still not recovered: recovering from the in-memory copy would let two
// replicas each spend a slot of one run's retry budget for one disruption, which is the
// regression the claim exists to prevent.
func TestRecoverEvictedScaleSetWorkers_EvidenceLostIsSurfaced(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, _ := recoveryFixtureWith(t, interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return apierrors.NewNotFound(corev1.Resource("pods"), obj.GetName())
		},
	}, drainedWorker("runner-gpu-vanished"))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(), "an unclaimable disruption must not be re-run")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRecoveryEvidenceLost.WithLabelValues("team-a", "gpu", recoveryCauseDeletion)))
	assert.Contains(t, target.events, "EvictionRecoveryEvidenceLost")
	assert.NotContains(t, target.events, "EvictionRecoveryIdentityUnknown",
		"the identity was fine; only the pod went away")
}

// TestRunIdentityFromPod rejects every partial identity. A run is addressed by all three
// of owner, repo, and run_id, so a missing or malformed piece must yield "unknown"
// rather than a request built from a guess.
func TestRunIdentityFromPod(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantOK      bool
		wantOwner   string
		wantRepo    string
		wantRunID   string
	}{
		{
			name:        "complete",
			annotations: map[string]string{AnnotationRunID: "7", AnnotationRepository: "o/r"},
			wantOK:      true, wantOwner: "o", wantRepo: "r", wantRunID: "7",
		},
		{name: "no annotations", annotations: nil},
		{
			name:        "run id only",
			annotations: map[string]string{AnnotationRunID: "7"},
		},
		{
			name:        "repository only",
			annotations: map[string]string{AnnotationRepository: "o/r"},
		},
		{
			// A run_id of 0 is what an absent workflowRunId decodes to; it addresses no
			// run, so it must be treated as unknown rather than posted to GitHub.
			name:        "zero run id",
			annotations: map[string]string{AnnotationRunID: "0", AnnotationRepository: "o/r"},
		},
		{
			name:        "repository without owner",
			annotations: map[string]string{AnnotationRunID: "7", AnnotationRepository: "justrepo"},
		},
		{
			name:        "repository with empty owner",
			annotations: map[string]string{AnnotationRunID: "7", AnnotationRepository: "/r"},
		},
		{
			name:        "repository with empty name",
			annotations: map[string]string{AnnotationRunID: "7", AnnotationRepository: "o/"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			owner, repo, runID, ok := runIdentityFromPod(pod)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantOwner, owner)
			assert.Equal(t, tc.wantRepo, repo)
			assert.Equal(t, tc.wantRunID, runID)
		})
	}
}

// cleanupDeletedWorker builds the shape the deletion arm exists to REJECT: a scale-set
// worker whose container exited on its own and which was deleted afterwards. It is
// externally deleted, terminal and unclaimed — every precondition the drain arm shares —
// so the only thing separating it from a real drain is the ordering of the delete
// against the exit.
func cleanupDeletedWorker(name string) *corev1.Pod {
	pod := drainedWorker(name)
	requested := pod.DeletionTimestamp.Add(-time.Duration(*pod.DeletionGracePeriodSeconds) * time.Second)
	pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(requested.Add(-10 * time.Second))
	return pod
}

// debugLogger points p's logger at a buffer at Debug verbosity and returns a reader over
// what it captured.
func debugLogger(p *Provisioner) func() string {
	var buf bytes.Buffer
	var mu sync.Mutex
	p.Log = slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// TestRecoverEvictedScaleSetWorkers_DeclinedDisruptionIsLogged pins the Q549 signal: a
// scan that judged a terminating worker and declined must say so, because an
// unrecovered pod looks identical whether a scan judged it or no scan ever saw it. The
// e2e drain spec re-stages on the second and fails on the first, and before this line
// both were the same silence — three sightings on main and in the merge queue were
// unattributable to either.
func TestRecoverEvictedScaleSetWorkers_DeclinedDisruptionIsLogged(t *testing.T) {
	ctx := context.Background()
	p, target, _, rerunCount, _ := recoveryFixture(t, cleanupDeletedWorker("runner-gpu-cleanup"))
	logged := debugLogger(p)

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(), "a job that failed on its own must not be re-run")
	assert.Contains(t, logged(), "did not qualify as a recoverable disruption")
	assert.Contains(t, logged(), "runner-gpu-cleanup")
}

// TestRecoverEvictedScaleSetWorkers_DeclineIsNotLoggedForNonCandidates keeps the line
// above narrow. It fires only for the ambiguous shape; every pod the scan skips for a
// reason already visible elsewhere stays silent, so the e2e spec's "no verdict" reading
// means what it says and an operator's Debug log does not fill with reaped workers.
func TestRecoverEvictedScaleSetWorkers_DeclineIsNotLoggedForNonCandidates(t *testing.T) {
	reapedByAGC := func() *corev1.Pod {
		pod := cleanupDeletedWorker("runner-gpu-reaped")
		pod.Annotations[AnnotationDeletionReason] = "completed_ttl"
		return pod
	}
	alreadyClaimed := func() *corev1.Pod {
		pod := cleanupDeletedWorker("runner-gpu-claimed")
		pod.Annotations[AnnotationEvictionHandledAt] = time.Now().UTC().Format(time.RFC3339)
		return pod
	}
	stillRunning := func() *corev1.Pod {
		pod := cleanupDeletedWorker("runner-gpu-running")
		pod.Status.Phase = corev1.PodRunning
		return pod
	}
	recoverable := func() *corev1.Pod { return drainedWorker("runner-gpu-drained") }

	for _, tc := range []struct {
		name string
		pod  func() *corev1.Pod
	}{
		{"the AGC's own reap", reapedByAGC},
		{"a recovery already claimed", alreadyClaimed},
		{"a worker still terminating", stillRunning},
		{"a genuine drain, which is recovered instead", recoverable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, target, _, _, _ := recoveryFixture(t, tc.pod())
			logged := debugLogger(p)

			done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
			require.NoError(t, err)
			<-done

			assert.NotContains(t, logged(), "did not qualify as a recoverable disruption")
		})
	}
}
