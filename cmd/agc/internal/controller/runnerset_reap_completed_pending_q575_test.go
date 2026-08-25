package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestRunnerSetReaper_ReapsCompletedPendingPods covers Q575, the Pending twin of the
// Q420 Running arm. A worker pod whose job went terminal at GitHub while it was still
// Pending can never start: the completion reclaims the JIT-config Secret the pod mounts,
// and a pod that has not mounted yet cannot mount one that is gone. The reaper read the
// completion stamp only in the Running arm, so such a pod sat out the whole
// pendingPodDeadline — ten minutes by default, holding a concurrency slot and a node —
// and was then reported as a scheduling stall it never had.
func TestRunnerSetReaper_ReapsCompletedPendingPods(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)

	pendingPod := func(name string, createdAgo time.Duration, completedAt string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
				CreationTimestamp: metav1.NewTime(now.Add(-createdAgo)),
				Labels:            map[string]string{provisioner.LabelRunnerSet: "set"}},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		}
		if completedAt != "" {
			pod.Annotations = map[string]string{provisioner.AnnotationJobCompletedAt: completedAt}
		}
		return pod
	}
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }

	// Job over 2m ago, grace 30s → reaped, though it is nowhere near the 10m deadline.
	stranded := pendingPod("worker-stranded", time.Minute, stamp(-2*time.Minute))
	// Job over 10s ago → inside the grace, so it may still be reaching Running.
	starting := pendingPod("worker-starting", time.Minute, stamp(-10*time.Second))
	// No stamp and young: an ordinary pod waiting on a scheduler or an image pull.
	scheduling := pendingPod("worker-scheduling", time.Minute, "")
	// No stamp and past the deadline: the pre-existing arm, which must still fire.
	stuck := pendingPod("worker-stuck", 20*time.Minute, "")
	// An unparseable stamp must not be read as "completed long ago".
	garbled := pendingPod("worker-garbled", time.Minute, "yesterday")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, stranded, starting, scheduling, stuck, garbled).Build()
	rec := events.NewFakeRecorder(8)
	r := &RunnerSetReconciler{
		Client:   c,
		Log:      slog.Default(),
		Now:      func() time.Time { return now },
		Metrics:  reapTestMetrics(),
		Recorder: rec,
	}

	_, counts, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	ctx := context.Background()
	get := func(name string) error {
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Pod{})
	}
	assert.Error(t, get("worker-stranded"),
		"a Pending pod past its job's completion grace must be reaped, not left to the pending deadline")
	assert.NoError(t, get("worker-starting"), "a Pending pod within the completion grace must be retained")
	assert.NoError(t, get("worker-scheduling"), "an unstamped Pending pod within the deadline must be retained")
	assert.Error(t, get("worker-stuck"), "the pending-deadline arm must still reap an unstamped stale pod")
	assert.NoError(t, get("worker-garbled"), "an unparseable completion stamp must not reap the pod")

	assert.Equal(t, int32(5), counts.pending,
		"every Pending pod counts as pending, including those reaped this pass")

	assert.Equal(t, 1.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues(ns, "set", "set", "completed_pending")),
		"the reap is attributed to the job's completion, not to the pending deadline")
	assert.Equal(t, 1.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues(ns, "set", "set", "pending_deadline")),
		"the genuinely stuck pod is still attributed to the pending deadline")

	var completedEvent, stuckEvent string
	for len(rec.Events) > 0 {
		e := <-rec.Events
		switch {
		case strings.Contains(e, "WorkerPodCompletedPending"):
			completedEvent = e
		case strings.Contains(e, "WorkerPodStuckPending"):
			stuckEvent = e
		}
	}
	require.NotEmpty(t, completedEvent, "reaping a completed Pending pod must emit WorkerPodCompletedPending")
	assert.Contains(t, completedEvent, "worker-stranded")
	assert.Contains(t, completedEvent, "Warning")
	assert.NotContains(t, completedEvent, "scheduling constraints",
		"the operator must not be sent after a scheduling problem the pod never had")

	require.NotEmpty(t, stuckEvent, "the genuinely stuck pod still gets WorkerPodStuckPending")
	assert.Contains(t, stuckEvent, "worker-stuck")
}

// TestRunnerSetReaper_CompletedPendingReapIsAttributedToTheAGC pins the Q502 half at
// this new reap site: the deletion is stamped as the AGC's own before it is issued, so
// eviction recovery does not read it as a disruption and re-run a job that is already
// over. The reaper stamps every reason it reaps under, but a new arm is a new chance to
// bypass that, and the cost of missing it is a spurious workflow re-run.
func TestRunnerSetReaper_CompletedPendingReapIsAttributedToTheAGC(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)

	// A finalizer holds the pod in the API after the delete, so the annotation the
	// reaper patched on is still readable.
	stranded := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "worker-stranded",
			CreationTimestamp: metav1.NewTime(now.Add(-time.Minute)),
			Finalizers:        []string{"test.actions-gateway.com/hold"},
			Labels:            map[string]string{provisioner.LabelRunnerSet: "set"},
			Annotations: map[string]string{
				provisioner.AnnotationJobCompletedAt: now.Add(-2 * time.Minute).UTC().Format(time.RFC3339),
			}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs, stranded).Build()
	r := &RunnerSetReconciler{
		Client:   c,
		Log:      slog.Default(),
		Now:      func() time.Time { return now },
		Metrics:  reapTestMetrics(),
		Recorder: events.NewFakeRecorder(8),
	}

	_, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	var got corev1.Pod
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "worker-stranded"}, &got))
	assert.False(t, got.DeletionTimestamp.IsZero(), "the pod must have been deleted")
	assert.Equal(t, "completed_pending", got.Annotations[provisioner.AnnotationDeletionReason],
		"the reap must be attributed to the AGC so eviction recovery does not re-run the job")
}
