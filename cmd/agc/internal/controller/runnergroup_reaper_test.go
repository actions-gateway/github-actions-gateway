package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// reaperMetrics builds a Metrics with only the reaper counter, unregistered
// (not added to the global Prometheus registry) for per-test isolation.
func reaperMetrics() *listener.Metrics {
	return &listener.Metrics{
		WorkerPodsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_reaper_worker_pods_reaped_total",
		}, []string{"namespace", "runner_group", "reason"}),
	}
}

// workerPod builds a worker pod for the given RunnerGroup with the phase,
// creation time, and (for terminal phases) container finish time set.
func workerPod(ns, rgName, name string, phase corev1.PodPhase, created, finished time.Time) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			Labels:            map[string]string{provisioner.LabelRunnerGroup: rgName},
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "img"}}},
		Status: corev1.PodStatus{Phase: phase},
	}
	if !finished.IsZero() {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "runner",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.NewTime(finished)},
			},
		}}
	}
	return pod
}

// TestReconcile_ReapsExpiredWorkerPods drives the Q95 reaper through a full
// Reconcile: terminal pods past completedPodTTL and Pending pods past
// pendingPodDeadline are deleted; fresh terminal/Pending pods and Running pods
// are kept; the result's RequeueAfter is the time until the earliest retained
// pod becomes due.
func TestReconcile_ReapsExpiredWorkerPods(t *testing.T) {
	now := time.Now()
	rg := newRunnerGroup("default", "reap-rg", 1)
	rg.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Minute}
	rg.Spec.PendingPodDeadline = &metav1.Duration{Duration: 5 * time.Minute}

	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(
			rg,
			// Terminal, finished 2m ago, TTL 1m → reaped.
			workerPod("default", "reap-rg", "succeeded-old", corev1.PodSucceeded, now.Add(-10*time.Minute), now.Add(-2*time.Minute)),
			// Terminal, finished 30s ago → kept, due in ~30s (the earliest).
			workerPod("default", "reap-rg", "failed-fresh", corev1.PodFailed, now.Add(-10*time.Minute), now.Add(-30*time.Second)),
			// Pending for 6m, deadline 5m → reaped.
			workerPod("default", "reap-rg", "pending-old", corev1.PodPending, now.Add(-6*time.Minute), time.Time{}),
			// Pending for 1m → kept, due in ~4m.
			workerPod("default", "reap-rg", "pending-fresh", corev1.PodPending, now.Add(-time.Minute), time.Time{}),
			// Running since 8h ago → never reaped (bounded by GitHub's job timeout).
			workerPod("default", "reap-rg", "running-long", corev1.PodRunning, now.Add(-8*time.Hour), time.Time{}),
			// Same-namespace pod without the worker label → untouched.
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "bystander", Namespace: "default",
					CreationTimestamp: metav1.NewTime(now.Add(-24 * time.Hour))},
				Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
			},
		).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	r.Now = func() time.Time { return now }
	r.Metrics = reaperMetrics()
	rec := events.NewFakeRecorder(16)
	r.Recorder = rec

	key := types.NamespacedName{Namespace: "default", Name: "reap-rg"}
	reconcile(t, r, key) // adds finalizer
	res := reconcile(t, r, key)

	ctx := context.Background()
	gone := func(name string) bool {
		err := fb.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &corev1.Pod{})
		return apierrors.IsNotFound(err)
	}
	assert.True(t, gone("succeeded-old"), "terminal pod past TTL must be reaped")
	assert.True(t, gone("pending-old"), "Pending pod past deadline must be reaped")
	assert.False(t, gone("failed-fresh"), "terminal pod within TTL must be retained")
	assert.False(t, gone("pending-fresh"), "Pending pod within deadline must be retained")
	assert.False(t, gone("running-long"), "Running pods must never be reaped")
	assert.False(t, gone("bystander"), "pods without the worker label must be untouched")

	// Earliest retained due time is failed-fresh: 1m TTL − 30s elapsed ≈ 30s.
	assert.InDelta(t, (30 * time.Second).Seconds(), res.RequeueAfter.Seconds(), 1.0,
		"RequeueAfter must be the time until the earliest retained pod is due")

	assert.Equal(t, 1.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues("default", "reap-rg", "completed_ttl")))
	assert.Equal(t, 1.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues("default", "reap-rg", "pending_deadline")))

	// The Pending reap is operator-visible as a Warning event; TTL reaps are
	// routine and emit none. Drain events and find the stuck-Pending one.
	var stuckEvent string
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.Contains(e, "WorkerPodStuckPending") {
			stuckEvent = e
		}
	}
	require.NotEmpty(t, stuckEvent, "reaping a stuck-Pending pod must emit a WorkerPodStuckPending event")
	assert.Contains(t, stuckEvent, "pending-old")
	assert.Contains(t, stuckEvent, "Warning")
}

// TestReconcile_ReaperDefaults pins the defaulting contract: with both fields
// omitted, a terminal pod younger than DefaultCompletedPodTTL and a Pending
// pod younger than DefaultPendingPodDeadline are retained, and the requeue is
// scheduled from the defaults.
func TestReconcile_ReaperDefaults(t *testing.T) {
	now := time.Now()
	rg := newRunnerGroup("default", "dflt-rg", 1)

	fb := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(
			rg,
			// Finished 1m ago; default TTL 5m → kept, due in ~4m (the earliest).
			workerPod("default", "dflt-rg", "succeeded-recent", corev1.PodSucceeded, now.Add(-10*time.Minute), now.Add(-time.Minute)),
			// Pending for 5m; default deadline 10m → kept, due in ~5m.
			workerPod("default", "dflt-rg", "pending-recent", corev1.PodPending, now.Add(-5*time.Minute), time.Time{}),
		).
		WithStatusSubresource(rg).
		Build()

	r := newTestReconciler(fb)
	r.Now = func() time.Time { return now }

	key := types.NamespacedName{Namespace: "default", Name: "dflt-rg"}
	reconcile(t, r, key) // adds finalizer
	res := reconcile(t, r, key)

	ctx := context.Background()
	require.NoError(t, fb.Get(ctx, types.NamespacedName{Namespace: "default", Name: "succeeded-recent"}, &corev1.Pod{}))
	require.NoError(t, fb.Get(ctx, types.NamespacedName{Namespace: "default", Name: "pending-recent"}, &corev1.Pod{}))

	expected := provisioner.DefaultCompletedPodTTL - time.Minute // 4m
	assert.InDelta(t, expected.Seconds(), res.RequeueAfter.Seconds(), 1.0,
		"RequeueAfter must derive from the default TTL when the field is omitted")
}
