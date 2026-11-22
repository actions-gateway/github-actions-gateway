package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// idleRunnerPod is a Running scale-set worker whose runner container came up upAgo
// before now and whose runner was pre-registered as runnerName. A zero upAgo reports
// no runner container status at all.
func idleRunnerPod(ns, name, runnerName string, now time.Time, upAgo time.Duration) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              name,
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
			Labels: map[string]string{
				provisioner.LabelRunnerSet:           "set",
				provisioner.LabelAcquisitionProtocol: provisioner.AcquisitionProtocolScaleSet,
			},
			Annotations: map[string]string{provisioner.AnnotationRunnerName: runnerName},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if upAgo > 0 {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: provisioner.WorkerContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(now.Add(-upAgo)),
			}},
		}}
	}
	return p
}

// runnerRecordCalls counts the stub's runner-record REST calls, the cost the busy stamp
// exists to bound.
func runnerRecordCalls(srv *scalesettest.Server) int {
	n := 0
	for _, c := range srv.Calls() {
		if strings.HasPrefix(c, "list-runners") || strings.HasPrefix(c, "delete-runner") {
			n++
		}
	}
	return n
}

// TestRunnerSetReaper_ReapsIdleRunnerGitHubReportsIdle is Q1360: a Running scale-set
// worker that no completion stamp will reach — the AGC never recorded a start for it,
// and its job, if it had one, went to another runner — had no deadline but the 12h
// lifetime cap. It is now reaped once its runner has been up idleRunnerGrace and
// GitHub agrees the runner is idle.
//
// The busy pods are the trap: an AGC that was down across a job's start holds a pod
// with no started-job-id whose runner is mid-job. GitHub refusing to delete a runner
// running a job is what keeps it, including when the exact-name filter misses the
// record and only the full listing finds it.
func TestRunnerSetReaper_ReapsIdleRunnerGitHubReportsIdle(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil) // runnerLabels ["self-hosted"]: the scale set's name

	const (
		idle       = "self-hosted-0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
		busy       = "self-hosted-1b2c3d4e-5f6a-4b7c-9d8e-0f1a2b3c4d5e"
		hiddenIdle = "self-hosted-2c3d4e5f-6a7b-4c8d-ae9f-1a2b3c4d5e6f"
		hiddenBusy = "self-hosted-3d4e5f6a-7b8c-4d9e-bfa0-2b3c4d5e6f7a"
		fresh      = "self-hosted-4e5f6a7b-8c9d-4eaf-80b1-3c4d5e6f7a8b"
		started    = "self-hosted-5f6a7b8c-9dae-4fb0-91c2-4d5e6f7a8b9c"
		noStatus   = "self-hosted-6a7b8c9d-aebf-40c1-a2d3-5e6f7a8b9cad"
	)
	for _, n := range []string{idle, busy, hiddenIdle, hiddenBusy, fresh, started, noStatus} {
		srv.FailJITConfigName(n) // registers the record generatejitconfig would have left
	}
	srv.SetRunnerBusy(busy)
	srv.SetRunnerBusy(hiddenBusy)
	srv.HideRunnerFromNameFilter(hiddenIdle)
	srv.HideRunnerFromNameFilter(hiddenBusy)

	past := idleRunnerGrace + time.Minute
	startedPod := idleRunnerPod(ns, "worker-started", started, now, time.Hour)
	startedPod.Annotations[provisioner.AnnotationStartedJobID] = "job-7"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs,
		idleRunnerPod(ns, "worker-idle", idle, now, past),
		idleRunnerPod(ns, "worker-busy", busy, now, past),
		idleRunnerPod(ns, "worker-hidden-idle", hiddenIdle, now, past),
		idleRunnerPod(ns, "worker-hidden-busy", hiddenBusy, now, past),
		idleRunnerPod(ns, "worker-fresh", fresh, now, time.Minute),
		startedPod,
		idleRunnerPod(ns, "worker-no-status", noStatus, now, 0),
	).Build()
	recorder := events.NewFakeRecorder(16)
	metrics := reapTestMetrics()
	r := &RunnerSetReconciler{
		Client:   c,
		Log:      slog.Default(),
		Now:      func() time.Time { return now },
		Metrics:  metrics,
		Recorder: recorder,
	}
	key := types.NamespacedName{Namespace: ns, Name: "set"}
	r.scaleSetListeners = map[types.NamespacedName]*scaleSetListenerHandle{
		key: reapListenerHandle(newReapScaleSetClient(t, srv), "self-hosted"),
	}

	ctx := context.Background()
	next, counts, err := r.reapWorkerPods(ctx, slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	get := func(name string) (*corev1.Pod, error) {
		var p corev1.Pod
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p)
		return &p, err
	}
	for _, name := range []string{"worker-idle", "worker-hidden-idle"} {
		_, err := get(name)
		assert.Error(t, err, "%s: a runner GitHub reports idle past the grace must be reaped", name)
	}
	for _, name := range []string{"worker-busy", "worker-hidden-busy"} {
		p, err := get(name)
		require.NoError(t, err, "%s: a runner running a job at GitHub must never be reaped", name)
		assert.NotEmpty(t, p.Annotations[provisioner.AnnotationRunnerBusyAt],
			"%s: the busy verdict must be stamped so later passes stop asking", name)
	}
	for _, name := range []string{"worker-fresh", "worker-started", "worker-no-status"} {
		_, err := get(name)
		assert.NoError(t, err, "%s: not an idle-runner candidate, so it must be kept", name)
	}

	assert.ElementsMatch(t, []string{busy, hiddenBusy, fresh, started, noStatus}, srv.RegisteredRunners(),
		"only the idle runners' records may be deleted")
	assert.Equal(t, int32(7), counts.active, "every Running pod counts as active in the pass that sees it")
	assert.Positive(t, next)
	assert.LessOrEqual(t, next, idleRunnerGrace-time.Minute,
		"the fresh pod's idle deadline must schedule the next pass")
	assert.InDelta(t, 2, testutil.ToFloat64(metrics.WorkerPodsReaped.WithLabelValues(ns, "set", "set", reapReasonIdleRunner)), 0)
	assert.Contains(t, <-recorder.Events, "WorkerPodRunnerIdle")

	// A second pass must not ask GitHub about the busy pods again: an ephemeral runner
	// running a job ends with that job, and re-asking every reconcile would spend the
	// installation's REST budget on a settled answer.
	before := runnerRecordCalls(srv)
	_, _, err = r.reapWorkerPods(ctx, slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)
	assert.Equal(t, before, runnerRecordCalls(srv), "a stamped busy verdict must not be re-checked")
}

// TestRunnerSetReaper_IdleRunnerCheckRetriesOnGitHubError pins the unknown verdict: a
// check GitHub could not answer keeps the pod and asks again after idleRunnerRetry,
// rather than reaping on no answer or never asking again.
func TestRunnerSetReaper_IdleRunnerCheckRetriesOnGitHubError(t *testing.T) {
	ns := "team-a"
	now := time.Now()
	pod := idleRunnerPod(ns, "worker-idle", "self-hosted-0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d",
		now, idleRunnerGrace+time.Minute)
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(pod).Build()

	var asked int
	next, _, err := reapWorkerPodsByLabel(context.Background(), c, now, ns, "set",
		provisioner.LabelRunnerSet, time.Hour, time.Hour, slog.Default(), nil,
		reapHooks{releaseIdleRunner: func(context.Context, string) idleRunnerVerdict {
			asked++
			return idleRunnerUnknown
		}})
	require.NoError(t, err)
	assert.Equal(t, 1, asked)
	assert.Equal(t, idleRunnerRetry, next)
	var got corev1.Pod
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pod), &got))
	assert.Empty(t, got.Annotations[provisioner.AnnotationRunnerBusyAt],
		"no answer is not a busy verdict, so the pod stays a candidate")
}
