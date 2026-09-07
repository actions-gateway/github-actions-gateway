package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Q1029: the RunnerSet reconciler's worker-pod watch recovers a disrupted scale-set
// worker off the event itself, ahead of the reconcile the same event enqueues. These
// tests drive the handler directly, with no reconcile loop, so a rerun can only have
// come from the event path.

// drainedScaleSetWorkerPod is a scale-set worker as ProvisionScaleSetWorker stamps it,
// in the state a real kubelet publishes for a drained running worker: PodFailed with an
// empty reason, the deletion mark, and a container exit the mark predates. The finalizer
// is what lets the fake client hold an object carrying a deletionTimestamp.
func drainedScaleSetWorkerPod(name, ns, set string) *corev1.Pod {
	deletedAt := metav1.NewTime(time.Now().Add(-40 * time.Second))
	grace := int64(30)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{
				provisioner.LabelRunnerSet:           set,
				provisioner.LabelAcquisitionProtocol: provisioner.AcquisitionProtocolScaleSet,
			},
			Annotations: map[string]string{
				provisioner.AnnotationRunID:      "4242",
				provisioner.AnnotationRepository: "myorg/myrepo",
			},
			DeletionTimestamp:          &deletedAt,
			DeletionGracePeriodSeconds: &grace,
			Finalizers:                 []string{"test.actions-gateway.com/hold"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "runner",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   1,
					FinishedAt: metav1.NewTime(deletedAt.Add(-25 * time.Second)),
				}},
			}},
		},
	}
}

// fakeRerunAPI is the two run endpoints a deletion-cause recovery reaches: the run GET
// the Q811 conclusion check makes, answered as a run that concluded failure, and the
// rerun-failed-jobs POST, counted.
func fakeRerunAPI(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var reruns atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"completed","conclusion":"failure"}`)
			return
		}
		reruns.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return srv, &reruns
}

// podWatchFixture wires a reconciler whose only moving part is the worker-pod watch
// handler: a fake client holding the set, its referents, and pod; a provisioner pointed
// at a counting rerun API; and a real work queue to read the enqueue off.
func podWatchFixture(t *testing.T, pod *corev1.Pod) (*RunnerSetReconciler, workqueue.TypedRateLimitingInterface[reconcile.Request], *atomic.Int64) {
	t.Helper()
	ns := pod.Namespace
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.UID = "uid-set"
		rs.Spec.AcquisitionProtocol = v2alpha1.AcquisitionProtocolScaleSet
	})
	// The fake client answers a cancelled context where a real one refuses the call,
	// so the interceptor restores that: it is what lets a test here pin that the
	// recovery does not run on the handler's context.
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).
		WithObjects(rs, gwObj("gw", ns, ""), tmplObj("tmpl", ns), pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	srv, reruns := fakeRerunAPI(t)
	prov := provisioner.NewProvisioner(c, nil, slog.Default())
	prov.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	prov.GitHubAPIURL = srv.URL
	prov.HTTPClient = srv.Client()
	prov.MaxEvictionRetries = 2
	prov.EvictionRetryDelay = 0

	r := &RunnerSetReconciler{Client: c, Provisioner: prov, Log: slog.Default()}
	r.ensureMaps()
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	t.Cleanup(q.ShutDown)
	return r, q, reruns
}

// TestWorkerPodHandler_RecoversDisruptedScaleSetWorkerOffTheEvent is the controller
// half of Q1029: the phase-change event for a drained scale-set worker claims the pod
// and re-runs its run without Reconcile ever running — and still enqueues the
// reconcile it always did, so nothing downstream of the watch changes.
//
// The handler's context is cancelled the moment it returns, as controller-runtime's
// event handler does per delivery, and the recovery has to outlive it: the envtest
// twin went red on exactly that before the goroutine's context was detached. The
// context is cancelled before the call rather than after it, because the fake client
// answers the claim faster than a cancel racing the goroutine could land, and a live
// apiserver does not.
func TestWorkerPodHandler_RecoversDisruptedScaleSetWorkerOffTheEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pod := drainedScaleSetWorkerPod("runner-set-abc", "team-a", "set")
	r, q, reruns := podWatchFixture(t, pod)

	running := pod.DeepCopy()
	running.Status.Phase = corev1.PodRunning
	r.workerPodHandler().Update(ctx, event.UpdateEvent{ObjectOld: running, ObjectNew: pod}, q)

	assert.Equal(t, 1, q.Len(), "the reconcile the event has always enqueued must still be enqueued")
	require.Eventually(t, func() bool { return reruns.Load() == 1 }, 5*time.Second, 20*time.Millisecond,
		"the drained worker's run must be re-run off the watch event, with no reconcile")

	var claimed corev1.Pod
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(pod), &claimed))
	assert.Contains(t, claimed.Annotations, provisioner.AnnotationEvictionHandledAt,
		"the event path must claim through the annotation the scan honours, or the reconcile it enqueued would re-run the job again")
	assert.Empty(t, pod.Annotations[provisioner.AnnotationEvictionHandledAt],
		"the event's own object must not be mutated: the informer owns it")
}

// TestWorkerPodHandler_LeavesTheRestToTheReconcile keeps the event path off everything
// the scan would also decline: a classic worker, which carries no tier label, and a
// scale-set worker whose phase change is the ordinary Pending→Running one. Both still
// enqueue their reconcile.
func TestWorkerPodHandler_LeavesTheRestToTheReconcile(t *testing.T) {
	classic := drainedScaleSetWorkerPod("runner-set-classic", "team-a", "set")
	delete(classic.Labels, provisioner.LabelAcquisitionProtocol)
	running := drainedScaleSetWorkerPod("runner-set-running", "team-a", "set")
	running.DeletionTimestamp, running.DeletionGracePeriodSeconds, running.Finalizers = nil, nil, nil
	running.Status = corev1.PodStatus{Phase: corev1.PodRunning}

	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
	}{
		{"a classic worker", classic},
		{"a scale-set worker that just started", running},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, q, reruns := podWatchFixture(t, tc.pod)

			r.workerPodHandler().Update(ctx, event.UpdateEvent{ObjectOld: tc.pod, ObjectNew: tc.pod}, q)

			assert.Equal(t, 1, q.Len())
			assert.Never(t, func() bool { return reruns.Load() > 0 }, 300*time.Millisecond, 20*time.Millisecond)
		})
	}
}
