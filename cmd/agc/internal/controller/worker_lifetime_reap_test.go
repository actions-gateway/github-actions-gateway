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

// TestRunnerSetReaper_LifetimeExceededIsDistinctFromCompletedTTL is the
// legibility half of Q438. The kubelet, not the AGC, kills a worker that
// exceeds its activeDeadlineSeconds, so the AGC only ever sees the aftermath: a
// Failed pod with reason DeadlineExceeded. It must report that as its own reap
// reason instead of folding it into completed_ttl, because an operator debugging
// a killed long job needs to see the lifetime cap as the cause rather than a
// mystery termination.
//
// The three pods together pin the classification boundary: only the
// DeadlineExceeded pod gets the new reason, an ordinary failure still gets
// completed_ttl, and an Evicted pod is untouched by the new arm (its reason is
// what the eviction-recovery path matches on, so mislabelling it would be a
// cross-wire between two unrelated recovery paths).
func TestRunnerSetReaper_LifetimeExceededIsDistinctFromCompletedTTL(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)

	// All three are terminal and well past the default 5m completedPodTTL, so the
	// only thing that differs between them is Status.Reason.
	terminalPod := func(name, reason string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         ns,
				Name:              name,
				CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
				Labels:            map[string]string{provisioner.LabelRunnerSet: "set"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: reason},
		}
	}

	killed := terminalPod("runner-killed", "DeadlineExceeded")
	failed := terminalPod("runner-failed", "")
	evicted := terminalPod("runner-evicted", "Evicted")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, killed, failed, evicted).Build()
	rec := events.NewFakeRecorder(8)
	r := &RunnerSetReconciler{
		Client:   c,
		Log:      slog.Default(),
		Now:      func() time.Time { return now },
		Metrics:  reapTestMetrics(),
		Recorder: rec,
	}

	_, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	ctx := context.Background()
	get := func(name string) error {
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Pod{})
	}
	// All three are terminal and past the TTL, so all three are reclaimed — the
	// lifetime cap changes how a reap is reported, not whether it happens.
	assert.Error(t, get("runner-killed"))
	assert.Error(t, get("runner-failed"))
	assert.Error(t, get("runner-evicted"))

	assert.Equal(t, 1.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues(ns, "set", "set", "lifetime_exceeded")),
		"a DeadlineExceeded pod must be counted under its own reap reason")
	assert.Equal(t, 2.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues(ns, "set", "set", "completed_ttl")),
		"an ordinary failure and an eviction must still be counted under completed_ttl")

	var lifetimeEvent string
	var lifetimeEvents int
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.Contains(e, "WorkerPodLifetimeExceeded") {
			lifetimeEvent = e
			lifetimeEvents++
		}
	}
	require.Equal(t, 1, lifetimeEvents,
		"exactly the DeadlineExceeded pod may emit WorkerPodLifetimeExceeded")
	assert.Contains(t, lifetimeEvent, "runner-killed")
	assert.Contains(t, lifetimeEvent, "Warning")
	// The Event has to be actionable on its own: it must name the field to raise,
	// otherwise the operator is left knowing only that something killed the pod.
	assert.Contains(t, lifetimeEvent, "maxWorkerLifetime")
	assert.Contains(t, lifetimeEvent, "12h",
		"the Event must state the lifetime that actually applied")
}
