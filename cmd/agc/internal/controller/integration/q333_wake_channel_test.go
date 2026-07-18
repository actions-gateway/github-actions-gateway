//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// setHasCondition reports whether the RunnerSet carries a status condition of the
// given type (v2 counterpart of hasCondition).
func setHasCondition(rs *v2alpha1.RunnerSet, condType string) bool {
	for _, c := range rs.Status.Conditions {
		if c.Type == condType {
			return true
		}
	}
	return false
}

// TestAGC_Reconciler_PushedConditionWakesRunnerGroup proves the Q333 wake channel is
// wired for the v1 RunnerGroup reconciler: a listener-pushed condition wakes the
// reconciler immediately — draining onto conditionCh triggers a reconcile through the
// source.Channel — rather than sitting in the channel until the next worker-Pod event
// or the resync period.
//
// MaxListeners=0 keeps the test deterministic: no listener goroutines run and no
// worker pods are created, so once the controller quiesces the ONLY thing that can wake
// it inside the assertion window is the pushed condition itself. Crucially, unlike the
// worker-Pod-watch test, this test creates no Pod and issues no RunnerGroup write — the
// push is the sole trigger, so a passing assertion isolates the wake channel.
func TestAGC_Reconciler_PushedConditionWakesRunnerGroup(t *testing.T) {
	const nsName = "agc-wake-rg-test"
	const rgName = "wake-rg"
	const sentinelType = "WakeProbe"

	createNSForAGC(t, nsName)

	rg := newRunnerGroup(nsName, rgName, 0)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	r, _, _ := startAGCReconcilerOpts(t, provisionerOptions{})
	key := types.NamespacedName{Namespace: nsName, Name: rgName}

	// Let the controller reach steady state before we probe it.
	waitForQuiescence(t, r)
	before := r.ReconcileCountForTest()

	// Push a sentinel condition as a listener goroutine would. This is the ONLY event
	// after quiescence — no Pod, no RunnerGroup write — so it alone must wake the
	// reconciler and be drained into status.
	r.SetConditionForTest(nsName, rgName, metav1.Condition{
		Type:    sentinelType,
		Status:  metav1.ConditionTrue,
		Reason:  "Probe",
		Message: "injected by Q333 wake-channel integration test",
	})

	require.Eventually(t, func() bool {
		var fetched v1alpha1.RunnerGroup
		if err := k8sClient.Get(ctx, key, &fetched); err != nil {
			return false
		}
		return r.ReconcileCountForTest() > before && hasCondition(&fetched, sentinelType)
	}, 15*time.Second, 50*time.Millisecond,
		"pushed condition did not wake the reconciler and flush into status (Q333 wake channel not wired?)")
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
	waitForSetReadyReason(t, ns, setName, metav1.ConditionFalse, v2alpha1.ReasonGatewayNotFound)
	waitForQuiescence(t, r)
	before := r.ReconcileCountForTest()

	r.SetConditionForTest(ns, setName, metav1.Condition{
		Type:    sentinelType,
		Status:  metav1.ConditionTrue,
		Reason:  "Probe",
		Message: "injected by Q333 wake-channel integration test",
	})

	require.Eventually(t, func() bool {
		var fetched v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, key, &fetched); err != nil {
			return false
		}
		return r.ReconcileCountForTest() > before && setHasCondition(&fetched, sentinelType)
	}, 15*time.Second, 50*time.Millisecond,
		"pushed condition did not wake the RunnerSet reconciler and flush into status (Q333 wake channel not wired?)")
}
