//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// waitForQuiescence blocks until the reconciler's invocation count stops
// increasing for one poll interval, i.e. no reconcile is pending. Returning
// only after the count is stable guarantees that any straggler reconcile
// (e.g. the no-op confirming one that follows the first status write) has
// already run — so a condition injected afterwards cannot be drained until the
// next genuinely new event.
func waitForQuiescence(t *testing.T, r interface{ ReconcileCountForTest() int64 }) {
	t.Helper()
	var last int64 = -1
	require.Eventually(t, func() bool {
		c := r.ReconcileCountForTest()
		stable := c > 0 && c == last
		last = c
		return stable
	}, 15*time.Second, 200*time.Millisecond, "controller never quiesced")
}

// TestV2_RunnerSet_WorkerPodEventTriggersReconcile proves the RunnerSet controller's
// worker-Pod watch is wired: creating a worker Pod re-triggers a reconcile, so status
// and the reaper track pod transitions without waiting for the next RunnerSet write
// or the cache resync.
//
// The set's gateway does not exist, so it settles at GatewayNotFound with no listener
// and no provisioned pods; once the controller quiesces, the hand-made Pod is the only
// thing that can wake it.
func TestV2_RunnerSet_WorkerPodEventTriggersReconcile(t *testing.T) {
	const ns = "v2-podwatch-test"
	const setName = "podwatch-set"

	createNSForAGC(t, ns)
	r := startRunnerSetReconciler(t)

	rs := newRunnerSet(setName, ns, "gw-does-not-exist")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })

	waitForSetReadyReason(t, ns, setName, metav1.ConditionFalse, v2.ReasonGatewayNotFound)
	waitForQuiescence(t, r)
	before := r.ReconcileCountForTest()

	createV2WorkerPod(t, ns, setName, "runner-podwatch-probe")

	require.Eventually(t, func() bool {
		return r.ReconcileCountForTest() > before
	}, 15*time.Second, 50*time.Millisecond,
		"worker Pod creation did not trigger a reconcile (Pod watch not wired?)")
}
