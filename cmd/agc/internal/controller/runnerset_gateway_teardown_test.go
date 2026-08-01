package controller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// terminatingGateway returns a gateway carrying a deletion timestamp. The finalizer is
// what lets the object exist in that state — the GMC's own cleanup finalizer holds it
// there for real, and the fake client requires one for the same reason.
func terminatingGateway(name, ns string) *v2alpha1.ActionsGateway {
	gw := gwObj(name, ns, "")
	gw.Finalizers = []string{v2alpha1.ActionsGatewayFinalizer}
	now := metav1.Now()
	gw.DeletionTimestamp = &now
	return gw
}

func setWorkerPod(name, ns, set string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
			Labels: map[string]string{provisioner.LabelRunnerSet: set}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// TestRunnerSetReconcile_GatewayTerminatingReapsEveryWorker covers Q547. Worker pods
// are owned by the RunnerSet, which survives gateway deletion by design, so nothing
// cascades when the gateway goes — and the AGC that is their only reaper is deleted
// with it. A Running worker with no completion stamp is the exact pod the ordinary
// reaper retains indefinitely, so it is the one that used to pin a billable node until
// the kubelet's activeDeadlineSeconds fired up to 12h later.
func TestRunnerSetReconcile_GatewayTerminatingReapsEveryWorker(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	ctx := context.Background()

	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Finalizers = []string{runnerSetFinalizer}
	})
	// Each of these is retained by the ordinary reaper: no TTL has elapsed, no deadline
	// has passed, and the Running pod carries no completion stamp at all.
	running := setWorkerPod("worker-running", ns, "set", corev1.PodRunning)
	pending := setWorkerPod("worker-pending", ns, "set", corev1.PodPending)
	terminal := setWorkerPod("worker-done", ns, "set", corev1.PodSucceeded)
	// Already terminating: the kubelet finishes it with no controller involved, which is
	// the state this path exists to reach — reaping it again would be a wasted delete.
	going := setWorkerPod("worker-going", ns, "set", corev1.PodRunning)
	going.Finalizers = []string{"test/hold"}
	goingNow := metav1.Now()
	going.DeletionTimestamp = &goingNow
	// A neighbour set's worker in the same namespace must not be touched.
	neighbour := setWorkerPod("worker-neighbour", ns, "other", corev1.PodRunning)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(terminatingGateway("gw", ns), rs, running, pending, terminal, going, neighbour).
		WithStatusSubresource(rs).Build()
	rec := events.NewFakeRecorder(8)
	r := &RunnerSetReconciler{Client: c, Log: slog.Default(), Metrics: reapTestMetrics(), Recorder: rec}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "set"}})
	require.NoError(t, err)

	gone := func(name string) bool {
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Pod{}) != nil
	}
	assert.True(t, gone("worker-running"),
		"a Running worker with no completion stamp is retained forever by the ordinary reaper; teardown must delete it")
	assert.True(t, gone("worker-pending"), "a Pending worker inside its deadline must be deleted on teardown")
	assert.True(t, gone("worker-done"), "a terminal worker inside its TTL must be deleted on teardown")
	assert.False(t, gone("worker-neighbour"), "another RunnerSet's worker must not be reaped")

	assert.Equal(t, 3.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues(ns, "set", "set", reapReasonGatewayDeleted)),
		"only the three live pods of this set are reaped: the neighbour's and the already-terminating one are not")

	var got v2alpha1.RunnerSet
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "set"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, v2alpha1.ReasonGatewayTerminating, ready.Reason)
	assert.Zero(t, got.Status.ActiveJobs, "the GMC gates its teardown on these counts reaching zero")
	assert.Zero(t, got.Status.PendingJobs)

	// The operator has to be able to tell a deliberate teardown reap from a mystery
	// deletion — a job running at that moment is lost.
	require.NotEmpty(t, rec.Events)
	assert.Contains(t, <-rec.Events, "WorkerPodsReapedOnGatewayTeardown")
}

// TestRunnerSetReconcile_LiveGatewayRetainsWorkers is the control: the same Running
// worker survives every other Ready=False path. Here the template never resolves, so
// the set degrades and stops its listeners — but its workers are left alone. Only the
// gateway's deletion timestamp reaps.
func TestRunnerSetReconcile_LiveGatewayRetainsWorkers(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	ctx := context.Background()

	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Finalizers = []string{runnerSetFinalizer}
	})
	running := setWorkerPod("worker-running", ns, "set", corev1.PodRunning)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gwObj("gw", ns, ""), rs, running).WithStatusSubresource(rs).Build()
	r := &RunnerSetReconciler{Client: c, Log: slog.Default(), Metrics: reapTestMetrics(),
		Recorder: events.NewFakeRecorder(8)}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "set"}})
	require.NoError(t, err)

	assert.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "worker-running"}, &corev1.Pod{}),
		"a live gateway must never reap workers, however degraded the set is")
	assert.Equal(t, 0.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues(ns, "set", "set", reapReasonGatewayDeleted)))
}

// TestGatewayTerminating_Trigger pins the trigger to a terminating gateway rather than
// a missing one. A gateway that is gone is both the resting state after teardown and
// the gap between a delete and a re-apply; reaping there would destroy live workers on
// a recreate, and a restarted AGC cannot tell that gap from a real teardown.
func TestGatewayTerminating_Trigger(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	ctx := context.Background()
	rs := rsObj("set", ns, nil)

	for _, tc := range []struct {
		name    string
		gateway *v2alpha1.ActionsGateway
		want    bool
	}{
		{"terminating", terminatingGateway("gw", ns), true},
		{"live", gwObj("gw", ns, ""), false},
		{"missing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs)
			if tc.gateway != nil {
				b = b.WithObjects(tc.gateway)
			}
			got, err := gatewayTerminating(ctx, b.Build(), rs)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
