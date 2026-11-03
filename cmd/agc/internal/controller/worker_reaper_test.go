package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
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

// workerPod builds a worker pod for the given RunnerSet with the phase,
// creation time, and (for terminal phases) container finish time set.
func workerPod(ns, setName, name string, phase corev1.PodPhase, created, finished time.Time) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			Labels:            map[string]string{provisioner.LabelRunnerSet: setName},
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

// TestReaper_ReapsExpiredWorkerPods drives the Q95 reaper over a RunnerSet's worker
// pods: terminal pods past completedPodTTL and Pending pods past pendingPodDeadline
// are deleted; fresh terminal/Pending pods and Running pods are kept; the returned
// duration is the time until the earliest retained pod becomes due.
func TestReaper_ReapsExpiredWorkerPods(t *testing.T) {
	now := time.Now()
	rs := rsObj("reap-rs", "default", func(rs *v2alpha1.RunnerSet) {
		rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Minute}
		rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 5 * time.Minute}
	})

	fb := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).
		WithObjects(
			rs,
			// Terminal, finished 2m ago, TTL 1m → reaped.
			workerPod("default", "reap-rs", "succeeded-old", corev1.PodSucceeded, now.Add(-10*time.Minute), now.Add(-2*time.Minute)),
			// Terminal, finished 30s ago → kept, due in ~30s (the earliest).
			workerPod("default", "reap-rs", "failed-fresh", corev1.PodFailed, now.Add(-10*time.Minute), now.Add(-30*time.Second)),
			// Pending for 6m, deadline 5m → reaped.
			workerPod("default", "reap-rs", "pending-old", corev1.PodPending, now.Add(-6*time.Minute), time.Time{}),
			// Pending for 1m → kept, due in ~4m.
			workerPod("default", "reap-rs", "pending-fresh", corev1.PodPending, now.Add(-time.Minute), time.Time{}),
			// Running since 8h ago, no job-completion stamp → never reaped (bounded by
			// GitHub's job timeout).
			workerPod("default", "reap-rs", "running-long", corev1.PodRunning, now.Add(-8*time.Hour), time.Time{}),
			// Same-namespace pod without the worker label → untouched.
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "bystander", Namespace: "default",
					CreationTimestamp: metav1.NewTime(now.Add(-24 * time.Hour))},
				Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
			},
		).
		Build()

	rec := events.NewFakeRecorder(16)
	r := &RunnerSetReconciler{
		Client:   fb,
		Log:      slog.Default(),
		Now:      func() time.Time { return now },
		Metrics:  reapTestMetrics(),
		Recorder: rec,
	}

	next, counts, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	ctx := context.Background()
	gone := func(name string) bool {
		err := fb.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &corev1.Pod{})
		return apierrors.IsNotFound(err)
	}
	assert.True(t, gone("succeeded-old"), "terminal pod past TTL must be reaped")
	assert.True(t, gone("pending-old"), "Pending pod past deadline must be reaped")
	assert.False(t, gone("failed-fresh"), "terminal pod within TTL must be retained")
	assert.False(t, gone("pending-fresh"), "Pending pod within deadline must be retained")
	assert.False(t, gone("running-long"), "Running pods with no job-completion stamp must never be reaped")
	assert.False(t, gone("bystander"), "pods without the worker label must be untouched")

	// Earliest retained due time is failed-fresh: 1m TTL − 30s elapsed ≈ 30s.
	assert.InDelta(t, (30 * time.Second).Seconds(), next.Seconds(), 1.0,
		"the returned duration must be the time until the earliest retained pod is due")

	assert.Equal(t, 1.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues("default", "reap-rs", "reap-rs", "completed_ttl")))
	assert.Equal(t, 1.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues("default", "reap-rs", "reap-rs", "pending_deadline")))

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

	// The phase counts feed status.activeJobs/pendingJobs. pending-old is counted
	// before it is deleted: the count is the snapshot taken at reconcile time.
	assert.Equal(t, int32(1), counts.active, "one Running pod must be counted as active")
	assert.Equal(t, int32(2), counts.pending, "both Pending pods counted (including the one just reaped)")
}

// TestReaper_StampsDeletionReason pins the reaper half of Q502's exclusion: every pod
// the reaper deletes is first stamped with AnnotationDeletionReason, so a reaper delete
// can never read as an external disruption and re-run a job the AGC gave up on. The
// finalizer holds the reaped pod in the API so the stamp is observable after the
// delete, the way recovery would observe it.
func TestReaper_StampsDeletionReason(t *testing.T) {
	now := time.Now()
	rs := rsObj("stamp-rs", "default", func(rs *v2alpha1.RunnerSet) {
		rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Minute}
	})

	expired := workerPod("default", "stamp-rs", "failed-old", corev1.PodFailed, now.Add(-10*time.Minute), now.Add(-2*time.Minute))
	expired.Finalizers = []string{"test.actions-gateway.com/hold"}

	fb := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs, expired).Build()
	r := &RunnerSetReconciler{Client: fb, Log: slog.Default(), Now: func() time.Time { return now }}

	_, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	var pod corev1.Pod
	require.NoError(t, fb.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "failed-old"}, &pod))
	require.False(t, pod.DeletionTimestamp.IsZero(), "the expired pod must have been deleted")
	assert.Equal(t, "completed_ttl", pod.Annotations[provisioner.AnnotationDeletionReason],
		"the reaper must stamp its deletion as the AGC's own before issuing it")
}

// TestReaper_Defaults pins the defaulting contract: with both fields omitted, a
// terminal pod younger than DefaultCompletedPodTTL and a Pending pod younger than
// DefaultPendingPodDeadline are retained, and the requeue is scheduled from the
// defaults.
func TestReaper_Defaults(t *testing.T) {
	now := time.Now()
	rs := rsObj("dflt-rs", "default", nil)

	fb := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).
		WithObjects(
			rs,
			// Finished 1m ago; default TTL 5m → kept, due in ~4m (the earliest).
			workerPod("default", "dflt-rs", "succeeded-recent", corev1.PodSucceeded, now.Add(-10*time.Minute), now.Add(-time.Minute)),
			// Pending for 5m; default deadline 10m → kept, due in ~5m.
			workerPod("default", "dflt-rs", "pending-recent", corev1.PodPending, now.Add(-5*time.Minute), time.Time{}),
		).
		Build()
	r := &RunnerSetReconciler{Client: fb, Log: slog.Default(), Now: func() time.Time { return now }}

	next, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, fb.Get(ctx, types.NamespacedName{Namespace: "default", Name: "succeeded-recent"}, &corev1.Pod{}))
	require.NoError(t, fb.Get(ctx, types.NamespacedName{Namespace: "default", Name: "pending-recent"}, &corev1.Pod{}))

	expected := provisioner.DefaultCompletedPodTTL - time.Minute // 4m
	assert.InDelta(t, expected.Seconds(), next.Seconds(), 1.0,
		"the returned duration must derive from the default TTL when the field is omitted")
}
