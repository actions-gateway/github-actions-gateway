package provisioner

import (
	"context"
	"fmt"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q501: a job the listener abandons must not leave its worker pod running. The
// renew loop cancels such a job's context with listener.ErrJobAbandoned as the
// cause; a process-wide shutdown cancels the same context with no cause. Only the
// first may delete the pod — the second reaches every live job at once.

// cancellingWaiter is a PodWaiter that cancels the job context the moment provision
// starts waiting, then reports the cancellation. It stands in for the renew loop
// tearing a job down (or an AGC shutdown), with the pod already created.
type cancellingWaiter struct {
	cancel context.CancelCauseFunc
	cause  error
}

func (w *cancellingWaiter) WaitForCompletion(ctx context.Context, _, _ string) (PodOutcome, error) {
	w.cancel(w.cause)
	<-ctx.Done()
	return PodOutcome{}, ctx.Err()
}

// abandonTestMetrics builds an unregistered reap counter so each test observes only
// its own increments (runnercore.NewMetrics registers process-globally).
func abandonTestMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		WorkerPodsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_worker_pods_reaped_total",
		}, []string{"namespace", "runner_group", "runner_set", "reason"}),
	}
}

// provisionUntilCancelled runs provision with a waiter that cancels the job context
// with the given cause, and returns the fake client and the pod name provision used.
func provisionUntilCancelled(t *testing.T, cause error, metrics *runnercore.Metrics) (client.Client, string) {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	p := NewProvisioner(fc, metrics, nil)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	p.Waiter = &cancellingWaiter{cancel: cancel, cause: cause}

	target := &stubTarget{
		key:  client.ObjectKey{Namespace: "team-a", Name: "cpu"},
		spec: &ResolvedSpec{WorkerImage: "runner:test"},
	}
	_, err := p.provision(ctx, target, "plan-1", []byte("{}"), "")
	require.Error(t, err, "a cancelled wait must surface as a provisioning error")
	return fc, workerPodName(target.key.Name, "plan-1")
}

// TestProvision_AbandonedJobReclaimsWorker is the Q501 actuator: when the listener
// gives up on a job, the worker pod is deleted rather than left to burn a slot until
// spec.maxWorkerLifetime (12h by default). The delete must carry the AGC's own
// deletion stamp, or Q502's graceful-deletion recovery reads it as a disruption and
// re-runs a job GitHub is already redelivering.
func TestProvision_AbandonedJobReclaimsWorker(t *testing.T) {
	metrics := abandonTestMetrics()
	cause := fmt.Errorf("%w: job_not_found", listener.ErrJobAbandoned)
	fc, podName := provisionUntilCancelled(t, cause, metrics)

	var pod corev1.Pod
	err := fc.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: podName}, &pod)
	require.True(t, apierrors.IsNotFound(err),
		"the abandoned job's worker pod must be deleted, got %v", err)

	assert.Equal(t, 1.0,
		testutil.ToFloat64(metrics.WorkerPodsReaped.WithLabelValues("team-a", "cpu", "cpu", reapReasonJobAbandoned)),
		"the reclaim must be observable on actions_gateway_worker_pods_reaped_total")
}

// TestProvision_AbandonedWorkerIsStampedBeforeDeletion pins the exclusion the delete
// depends on: the pod carries actions-gateway.com/deletion-reason before it goes, so
// neither tier's deletion recovery re-runs it. Asserted on the object the delete
// observed rather than after the fact, since the pod itself is gone by then.
func TestProvision_AbandonedWorkerIsStampedBeforeDeletion(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithStatusSubresource(&corev1.Pod{}).Build()
	var deleted *corev1.Pod
	recording := recordingDeleteClient{Client: fc, onDelete: func(o client.Object) {
		if pod, ok := o.(*corev1.Pod); ok {
			deleted = pod.DeepCopy()
		}
	}}

	p := NewProvisioner(recording, abandonTestMetrics(), nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	p.Waiter = &cancellingWaiter{cancel: cancel, cause: fmt.Errorf("%w: consecutive_failures", listener.ErrJobAbandoned)}

	target := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "cpu"}, spec: &ResolvedSpec{WorkerImage: "runner:test"}}
	_, err := p.provision(ctx, target, "plan-1", []byte("{}"), "")
	require.Error(t, err)

	require.NotNil(t, deleted, "the abandoned worker pod must be deleted")
	assert.Equal(t, reapReasonJobAbandoned, deleted.Annotations[AnnotationDeletionReason],
		"the delete must be stamped as the AGC's own before it is issued")
}

// TestProvision_ShutdownLeavesWorkerRunning is the other half, and the one that
// makes the reclaim safe to ship: an AGC rollout cancels every job context at once
// with no cause, and must leave every live worker alone. Deleting on that signal
// would kill every running job on every restart.
func TestProvision_ShutdownLeavesWorkerRunning(t *testing.T) {
	metrics := abandonTestMetrics()
	fc, podName := provisionUntilCancelled(t, nil, metrics)

	var pod corev1.Pod
	require.NoError(t, fc.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: podName}, &pod),
		"a shutdown must not delete a live worker pod")
	assert.NotContains(t, pod.Annotations, AnnotationDeletionReason,
		"a shutdown must not stamp the pod either")
	assert.Equal(t, 0.0,
		testutil.ToFloat64(metrics.WorkerPodsReaped.WithLabelValues("team-a", "cpu", "cpu", reapReasonJobAbandoned)))
}

// recordingDeleteClient captures the object handed to Delete, so a test can assert
// what was true of the pod at the moment the delete was issued.
type recordingDeleteClient struct {
	client.Client
	onDelete func(client.Object)
}

func (c recordingDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.onDelete(obj)
	return c.Client.Delete(ctx, obj, opts...)
}
