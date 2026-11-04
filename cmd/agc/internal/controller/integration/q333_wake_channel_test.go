//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// setHasCondition reports whether the RunnerSet carries a status condition of the
// given type (v2 counterpart of hasCondition).
func setHasCondition(rs *v2.RunnerSet, condType string) bool {
	for _, c := range rs.Status.Conditions {
		if c.Type == condType {
			return true
		}
	}
	return false
}

// TestV2_Reconciler_PushedConditionWakesRunnerSet is the v2 counterpart: a pushed
// condition wakes the RunnerSet reconciler promptly via the Q333 wake channel.
//
// The RunnerSet references a gateway that does not exist, so it settles at
// Ready=False/GatewayNotFound with no listeners running and no periodic requeue — a
// stable idle state. After quiescence the pushed sentinel condition is the sole trigger
// that can wake the reconciler within the assertion window; the unresolved-refs path
// still writes status, so the drained sentinel is persisted.
func TestV2_Reconciler_PushedConditionWakesRunnerSet(t *testing.T) {
	const ns = "v2-wake-rs-test"
	const setName = "wake-set"
	const sentinelType = "WakeProbe"

	createNSForAGC(t, ns)
	r := startRunnerSetReconciler(t)

	rs := newRunnerSet(setName, ns, "gw-does-not-exist")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	key := types.NamespacedName{Namespace: ns, Name: setName}

	// Wait until the set has settled at GatewayNotFound and the controller quiesces, so
	// no in-flight reconcile can be mistaken for the wake.
	waitForSetReadyReason(t, ns, setName, metav1.ConditionFalse, v2.ReasonGatewayNotFound)
	waitForQuiescence(t, r)
	before := r.ReconcileCountForTest()

	r.SetConditionForTest(ns, setName, metav1.Condition{
		Type:    sentinelType,
		Status:  metav1.ConditionTrue,
		Reason:  "Probe",
		Message: "injected by Q333 wake-channel integration test",
	})

	require.Eventually(t, func() bool {
		var fetched v2.RunnerSet
		if err := k8sClient.Get(ctx, key, &fetched); err != nil {
			return false
		}
		return r.ReconcileCountForTest() > before && setHasCondition(&fetched, sentinelType)
	}, 15*time.Second, 50*time.Millisecond,
		"pushed condition did not wake the RunnerSet reconciler and flush into status (Q333 wake channel not wired?)")
}
