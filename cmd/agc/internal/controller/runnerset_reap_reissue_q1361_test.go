package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestRunnerSetReaper_NamesAWorkerReplacedBeforeStart is Q1361 at the reap site. A
// completed_pending reap of a worker that never started, while a newer worker of the set
// carries its run and job name, is GitHub's withdrawal and re-issue: the Event names the
// node and the replacement. The same stamp with no replacement is an ordinary
// cancellation and keeps WorkerPodCompletedPending.
func TestRunnerSetReaper_NamesAWorkerReplacedBeforeStart(t *testing.T) {
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)
	stamp := now.Add(-time.Minute).UTC().Format(time.RFC3339)

	worker := func(name string, createdAgo time.Duration, runID, completedAt string) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
				CreationTimestamp: metav1.NewTime(now.Add(-createdAgo)),
				Labels:            map[string]string{provisioner.LabelRunnerSet: "set"},
				Annotations: map[string]string{
					provisioner.AnnotationRunID:    runID,
					"actions-gateway.com/job-name": "build",
				}},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		}
		if completedAt != "" {
			p.Annotations[provisioner.AnnotationJobCompletedAt] = completedAt
		}
		return p
	}
	withdrawn := worker("worker-withdrawn", 6*time.Minute, "4242", stamp)
	withdrawn.Spec.NodeName = "node-cannot-run"
	replacement := worker("worker-replacement", time.Minute, "4242", "")
	cancelled := worker("worker-cancelled", 2*time.Minute, "5353", stamp)

	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).
		WithObjects(rs, withdrawn, replacement, cancelled).Build()
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

	byPod := map[string]string{}
	for len(rec.Events) > 0 {
		e := <-rec.Events
		for _, name := range []string{"worker-withdrawn", "worker-cancelled", "worker-replacement"} {
			if strings.Contains(e, name+" ") {
				byPod[name] = e
			}
		}
	}
	require.Contains(t, byPod, "worker-withdrawn")
	assert.Contains(t, byPod["worker-withdrawn"], "WorkerReplacedBeforeStart")
	assert.Contains(t, byPod["worker-withdrawn"], "on node node-cannot-run")
	assert.Contains(t, byPod["worker-withdrawn"], "worker-replacement")
	require.Contains(t, byPod, "worker-cancelled")
	assert.Contains(t, byPod["worker-cancelled"], "WorkerPodCompletedPending",
		"a completion with no re-issue beside it is a cancellation")
}
