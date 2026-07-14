//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestV2_ActionsGateway_RunnerSetsDegraded_Rollup verifies Q304: the health of the
// RunnerSets bound to a v2 ActionsGateway (spec.gatewayRef) rolls up to a
// RunnerSetsDegraded condition on the gateway — the operator's single pane, the v2
// counterpart of v1's RunnerGroupsDegraded. The rollup is scoped to bound sets (a set
// targeting a different gateway is ignored), names the impaired sets and their tripped
// signals, and clears when the children recover. The GMC's RunnerSet watch drives the
// parent reconcile, so the assertions rely only on eventual consistency (no manual
// gateway poke).
func TestV2_ActionsGateway_RunnerSetsDegraded_Rollup(t *testing.T) {
	const ns = "v2-ag-rs-rollup"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	ag := newV2GatewayWired("gw", ns, "github-app", "shared")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	// Two RunnerSets bound to gw, plus one bound to a different gateway (control): the
	// control must never count toward gw's rollup, even when impaired.
	boundA := newScaleSetRunnerSet("bound-a", ns, "gw", "linux")
	boundB := newScaleSetRunnerSet("bound-b", ns, "gw", "windows")
	other := newScaleSetRunnerSet("other-set", ns, "other-gw", "macos")
	for _, rs := range []*v2alpha1.RunnerSet{boundA, boundB, other} {
		require.NoError(t, k8sClient.Create(ctx, rs))
		rs := rs
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	}

	startActionsGatewayV2Reconciler(t)

	// The gateway provisions its AGC control plane — proving reconcileResources reached
	// updateStatus, where the rollup is computed.
	require.Eventually(t, func() bool {
		var dep appsv1.Deployment
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &dep) == nil
	}, 15*time.Second, 100*time.Millisecond, "AGC Deployment should be created")

	requireRollup := func(status metav1.ConditionStatus, reason string, contains ...string) {
		t.Helper()
		require.Eventually(t, func() bool {
			var got v2alpha1.ActionsGateway
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
				return false
			}
			c := findCondition(got.Status.Conditions, v2alpha1.ConditionRunnerSetsDegraded)
			if c == nil || c.Status != status || c.Reason != reason {
				return false
			}
			for _, s := range contains {
				if !strings.Contains(c.Message, s) {
					return false
				}
			}
			return true
		}, 20*time.Second, 100*time.Millisecond, "RunnerSetsDegraded=%s/%s message must contain %v", status, reason, contains)
	}

	// Bound sets carry no Ready condition yet (no AGC reconciles them in this suite), so
	// the rollup starts healthy — and the out-of-scope set is not in the count.
	requireRollup(metav1.ConditionFalse, v2alpha1.ReasonAllRunnerSetsHealthy, "all 2 bound RunnerSet(s) healthy")

	// Impair both bound sets: bound-a via a non-transient Ready=False (a reference or
	// GitHub-auth failure), bound-b via the abnormal WorkersUnschedulable condition (a
	// set can be serving yet have worker pods stuck Pending). The out-of-scope set is
	// impaired too, to prove scoping.
	setRunnerSetCondition(t, ns, "bound-a", metav1.Condition{
		Type: v2alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: v2alpha1.ReasonTokenUnavailable, Message: "test-induced",
	})
	setRunnerSetCondition(t, ns, "bound-b", metav1.Condition{
		Type: v2alpha1.ConditionWorkersUnschedulable, Status: metav1.ConditionTrue,
		Reason: v2alpha1.ReasonPodsUnschedulable, Message: "test-induced",
	})
	setRunnerSetCondition(t, ns, "other-set", metav1.Condition{
		Type: v2alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: v2alpha1.ReasonTokenUnavailable, Message: "test-induced",
	})

	// Both bound sets roll up, each naming its tripped signal; the control set does not.
	requireRollup(metav1.ConditionTrue, v2alpha1.ReasonRunnerSetsImpaired,
		"2 of 2 RunnerSet(s) impaired", "bound-a", "bound-b",
		v2alpha1.ConditionReady+"="+v2alpha1.ReasonTokenUnavailable,
		v2alpha1.ConditionWorkersUnschedulable)

	// Scoping: the set bound to a different gateway must never appear in gw's rollup.
	var got v2alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got))
	assert.NotContains(t, findCondition(got.Status.Conditions, v2alpha1.ConditionRunnerSetsDegraded).Message, "other-set",
		"a RunnerSet bound to a different gateway must not roll up")

	// Recover both bound sets; the rollup must clear back to healthy.
	setRunnerSetCondition(t, ns, "bound-a", metav1.Condition{
		Type: v2alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: v2alpha1.ReasonListenerActive, Message: "recovered",
	})
	setRunnerSetCondition(t, ns, "bound-b", metav1.Condition{
		Type: v2alpha1.ConditionWorkersUnschedulable, Status: metav1.ConditionFalse,
		Reason: v2alpha1.ReasonWorkersSchedulable, Message: "recovered",
	})
	requireRollup(metav1.ConditionFalse, v2alpha1.ReasonAllRunnerSetsHealthy, "all 2 bound RunnerSet(s) healthy")
}

// setRunnerSetCondition upserts a status condition on a RunnerSet, retrying on the
// resourceVersion conflict a concurrent reconcile can cause. It drives the child-health
// transitions the RunnerSetsDegraded rollup reacts to (Q304).
func setRunnerSetCondition(t *testing.T, ns, name string, cond metav1.Condition) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
			return false
		}
		meta.SetStatusCondition(&rs.Status.Conditions, cond)
		return k8sClient.Status().Update(ctx, &rs) == nil
	}, 5*time.Second, 25*time.Millisecond, "set %s=%s on RunnerSet %s", cond.Type, cond.Status, name)
}
