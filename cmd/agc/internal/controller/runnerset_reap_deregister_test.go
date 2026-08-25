package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type reapTokenProvider string

func (p reapTokenProvider) Token(context.Context) (string, error) { return string(p), nil }

func newReapScaleSetClient(t *testing.T, srv *scalesettest.Server) *scaleset.Client {
	t.Helper()
	c, err := scaleset.New(scaleset.Config{
		TokenProvider: reapTokenProvider("install-token"),
		ConfigURL:     "https://github.com/acme",
		APIBase:       srv.URL,
		HTTPClient:    srv.HTTPClient(),
		PollClient:    srv.HTTPClient(),
	})
	require.NoError(t, err)
	return c
}

// TestRunnerSetReaper_DeregistersReapedWorkersRunnerRecord is the Q550 fix at the reap
// site. A scale-set worker's runner record is pre-registered before the pod exists and
// nothing else removes it when the pod is reaped without ever running its job — and
// because the name derives from the job ID, that leftover is exactly what the job's own
// retry collides with.
//
// The three pods pin the boundary: a scale-set pod's record goes, a pod with no
// runner-name annotation (a classic worker, or one provisioned before the annotation
// existed) is reaped with nothing deregistered, and a record marked busy is kept
// because a live runner must never be deleted out from under its job.
func TestRunnerSetReaper_DeregistersReapedWorkersRunnerRecord(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)

	// Records the stub holds, as generatejitconfig would have left them.
	srv.FailJITConfigName("set-job-reaped")
	srv.FailJITConfigName("set-job-busy")
	srv.SetRunnerBusy("set-job-busy")

	// Every pod is terminal and well past the default completedPodTTL, so all three
	// are reaped and only the deregistration differs.
	pod := func(name, runnerName string) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         ns,
				Name:              name,
				CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
				Labels: map[string]string{
					provisioner.LabelRunnerSet:           "set",
					provisioner.LabelAcquisitionProtocol: provisioner.AcquisitionProtocolScaleSet,
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		}
		if runnerName != "" {
			p.Annotations = map[string]string{provisioner.AnnotationRunnerName: runnerName}
		}
		return p
	}

	reaped := pod("worker-reaped", "set-job-reaped")
	busy := pod("worker-busy", "set-job-busy")
	unstamped := pod("worker-unstamped", "")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, reaped, busy, unstamped).Build()
	r := &RunnerSetReconciler{
		Client:   c,
		Log:      slog.Default(),
		Now:      func() time.Time { return now },
		Metrics:  reapTestMetrics(),
		Recorder: events.NewFakeRecorder(8),
	}
	// Stand in for a running listener: the reaper reaches GitHub through the client the
	// listener owns.
	key := types.NamespacedName{Namespace: ns, Name: "set"}
	r.scaleSetListeners = map[types.NamespacedName]*scaleSetListenerHandle{
		key: {client: newReapScaleSetClient(t, srv)},
	}

	_, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)

	ctx := context.Background()
	get := func(name string) error {
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Pod{})
	}
	assert.Error(t, get("worker-reaped"))
	assert.Error(t, get("worker-busy"), "a record that cannot be deleted must not hold up the reap")
	assert.Error(t, get("worker-unstamped"))

	assert.Equal(t, []string{"set-job-busy"}, srv.RegisteredRunners(),
		"the reaped worker's record must be gone and the busy one kept")
}

// TestRunnerSetReaper_DeregisterIsSkippedWithoutAListener pins the ordering the
// reconciler actually runs in: the reaper runs before the listener is ensured, so on a
// set with no running listener there is no client to deregister through. That must reap
// normally rather than fail — the records it could not clear are collected by the sweep
// the next listener start runs.
func TestRunnerSetReaper_DeregisterIsSkippedWithoutAListener(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)

	worker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              "worker-1",
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
			Labels:            map[string]string{provisioner.LabelRunnerSet: "set"},
			Annotations:       map[string]string{provisioner.AnnotationRunnerName: "set-job-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs, worker).Build()
	r := &RunnerSetReconciler{
		Client:   c,
		Log:      slog.Default(),
		Now:      func() time.Time { return now },
		Metrics:  reapTestMetrics(),
		Recorder: events.NewFakeRecorder(8),
	}

	_, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
	require.NoError(t, err)
	assert.Error(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "worker-1"}, &corev1.Pod{}),
		"the pod is reaped whether or not its record could be deregistered")
}
