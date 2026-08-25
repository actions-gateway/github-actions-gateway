package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q766: the reaper is the scale-set tier's only sighting of a worker abandoned before it
// ran. The classic tier learns of it from the informer's delete event, on the goroutine
// that still holds the acquired payload; a scale-set worker has neither, and the pod is
// gone the instant the reap lands, so a later scan can never find it.
//
// These tests exercise the wiring end to end — the real reaper, the real Provisioner,
// and a stand-in GitHub — because a hook that is defined but never reached fails exactly
// like one that was never written.

// forceCancelSpy records the runs a force-cancel was issued for.
type forceCancelSpy struct {
	mu   sync.Mutex
	runs []string
}

func (s *forceCancelSpy) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// .../actions/runs/<id>/force-cancel
		if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); strings.HasSuffix(r.URL.Path, "/force-cancel") {
			s.mu.Lock()
			s.runs = append(s.runs, parts[len(parts)-2])
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *forceCancelSpy) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.runs...)
}

// scaleSetPendingPod is a scale-set worker pod as ProvisionScaleSetWorker created it,
// stuck in Pending since createdAgo with the run identity stamped on it.
func scaleSetPendingPod(ns, set, name, runID string, createdAgo time.Duration, now time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			CreationTimestamp: metav1.NewTime(now.Add(-createdAgo)),
			Labels: map[string]string{
				provisioner.LabelRunnerSet:           set,
				provisioner.LabelAcquisitionProtocol: provisioner.AcquisitionProtocolScaleSet,
			},
			Annotations: map[string]string{
				provisioner.AnnotationRunID:      runID,
				provisioner.AnnotationRepository: "myorg/myrepo",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

// TestRunnerSetReaper_AbandonedRunRecoveryFiresOnPendingDeadline is the port's wiring
// test: a scale-set worker reaped for sitting past pendingPodDeadline has its run
// force-cancelled and queued for the capacity-gated re-run, exactly as the classic tier
// has done since Q683/Q691.
//
// The pod that is reaped for the OTHER Pending reason must not: its job already went
// terminal at GitHub (Q575), so there is no open run to cancel and a re-run would re-run
// finished work.
func TestRunnerSetReaper_AbandonedRunRecoveryFiresOnPendingDeadline(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)

	stuck := scaleSetPendingPod(ns, "set", "worker-stuck", "4242", 20*time.Minute, now)
	// Same tier, same phase, reaped in the same pass — but for its job's completion.
	completed := scaleSetPendingPod(ns, "set", "worker-completed", "9999", time.Minute, now)
	completed.Annotations[provisioner.AnnotationJobCompletedAt] =
		now.Add(-2 * time.Minute).UTC().Format(time.RFC3339)

	spy := &forceCancelSpy{}
	srv := spy.server(t)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs, stuck, completed).Build()
	p := provisioner.NewProvisioner(c, nil, nil)
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	r := &RunnerSetReconciler{
		Client:      c,
		Log:         slog.Default(),
		Now:         func() time.Time { return now },
		Metrics:     reapTestMetrics(),
		Recorder:    events.NewFakeRecorder(8),
		Provisioner: p,
	}

	_, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(spy.seen()) > 0 }, 5*time.Second, 10*time.Millisecond,
		"the deadline reap must force-cancel the run its worker never ran")
	assert.Equal(t, []string{"4242"}, spy.seen(),
		"only the deadline reap has an open run to conclude; a completed_pending reap does not")
}

// TestRunnerSetReaper_AbandonedRunRecoverySkipsClassicWorkers keeps the reaper hook off
// the tier that already handles this itself. The reaper lists both tiers' pods, and the
// classic provision() goroutine force-cancels the same run off the informer's delete
// event — recovering it here too would spend a second slot of the run's shared retry
// budget for one abandonment.
func TestRunnerSetReaper_AbandonedRunRecoverySkipsClassicWorkers(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)

	classic := scaleSetPendingPod(ns, "set", "worker-classic", "4242", 20*time.Minute, now)
	delete(classic.Labels, provisioner.LabelAcquisitionProtocol)

	spy := &forceCancelSpy{}
	srv := spy.server(t)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs, classic).Build()
	p := provisioner.NewProvisioner(c, nil, nil)
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()

	r := &RunnerSetReconciler{
		Client:      c,
		Log:         slog.Default(),
		Now:         func() time.Time { return now },
		Metrics:     reapTestMetrics(),
		Recorder:    events.NewFakeRecorder(8),
		Provisioner: p,
	}

	_, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	// The reap itself still happened, so an empty spy is a decision rather than an
	// absence of work.
	assert.Error(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "worker-classic"}, &corev1.Pod{}),
		"the classic worker is still reaped")
	assert.Empty(t, spy.seen(), "a classic worker's run is force-cancelled by its own provision goroutine")
}
