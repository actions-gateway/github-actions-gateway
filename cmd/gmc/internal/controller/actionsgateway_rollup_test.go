package controller

import (
	"context"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// ownedRunnerGroup builds a RunnerGroup carrying the owner labels the GMC stamps,
// with the given impairing condition types set True.
func ownedRunnerGroup(name, ownerName, ownerNS string, trueConditions ...string) *agcv1alpha1.RunnerGroup {
	rg := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ownerNS,
			Labels: map[string]string{
				"actions-gateway/owner-name": ownerName,
				"actions-gateway/owner-ns":   ownerNS,
			},
		},
	}
	for _, t := range trueConditions {
		rg.Status.Conditions = append(rg.Status.Conditions, metav1.Condition{
			Type: t, Status: metav1.ConditionTrue, Reason: "Test",
		})
	}
	return rg
}

func rollupTestReconciler(t *testing.T, objs ...client.Object) *ActionsGatewayReconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(applyTestScheme(t)).WithObjects(objs...).Build()
	return &ActionsGatewayReconciler{Client: c}
}

// TestEvalRunnerGroupHealth_NoGroups: a gateway owning no RunnerGroups is healthy.
func TestEvalRunnerGroupHealth_NoGroups(t *testing.T) {
	ag := applyTestAG()
	r := rollupTestReconciler(t)
	h := r.evalRunnerGroupHealth(context.Background(), ag)
	assert.False(t, h.degraded)
	assert.Equal(t, gmcv1alpha1.ReasonAllRunnerGroupsHealthy, h.reason)
}

// TestEvalRunnerGroupHealth_AllHealthy: owned groups with no impairing condition
// (Ready=True only) leave the rollup False.
func TestEvalRunnerGroupHealth_AllHealthy(t *testing.T) {
	ag := applyTestAG()
	healthy := ownedRunnerGroup("rg-a", ag.Name, ag.Namespace)
	healthy.Status.Conditions = []metav1.Condition{{Type: agcv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Up"}}
	r := rollupTestReconciler(t, healthy)
	h := r.evalRunnerGroupHealth(context.Background(), ag)
	assert.False(t, h.degraded)
	assert.Equal(t, gmcv1alpha1.ReasonAllRunnerGroupsHealthy, h.reason)
}

// TestEvalRunnerGroupHealth_Impaired: a group with an impairing condition trips the
// rollup, and the message names the group and the tripped condition.
func TestEvalRunnerGroupHealth_Impaired(t *testing.T) {
	ag := applyTestAG()
	bad := ownedRunnerGroup("rg-bad", ag.Name, ag.Namespace, agcv1alpha1.ConditionCredentialUnavailable)
	good := ownedRunnerGroup("rg-good", ag.Name, ag.Namespace)
	r := rollupTestReconciler(t, bad, good)

	h := r.evalRunnerGroupHealth(context.Background(), ag)
	assert.True(t, h.degraded)
	assert.Equal(t, gmcv1alpha1.ReasonRunnerGroupsImpaired, h.reason)
	assert.Contains(t, h.message, "rg-bad")
	assert.Contains(t, h.message, agcv1alpha1.ConditionCredentialUnavailable)
	assert.NotContains(t, h.message, "rg-good", "healthy groups must not be named")
}

// TestEvalRunnerGroupHealth_IgnoresOtherOwnersAndAdvisory verifies the rollup only
// counts this gateway's groups and ignores advisory/transient conditions (quota,
// rate-limit) that are deliberately excluded from the impairing set.
func TestEvalRunnerGroupHealth_IgnoresOtherOwnersAndAdvisory(t *testing.T) {
	ag := applyTestAG()
	// Another gateway's impaired group in the same namespace — must be ignored.
	otherOwner := ownedRunnerGroup("rg-other", "different-gateway", ag.Namespace, agcv1alpha1.ConditionDegraded)
	// This gateway's group tripping only an advisory condition — must not count.
	advisory := ownedRunnerGroup("rg-adv", ag.Name, ag.Namespace, agcv1alpha1.ConditionWorkerQuotaPressure, agcv1alpha1.ConditionRateLimited)
	r := rollupTestReconciler(t, otherOwner, advisory)

	h := r.evalRunnerGroupHealth(context.Background(), ag)
	assert.False(t, h.degraded, "advisory conditions and other owners' groups must not trip the rollup")
}

// TestRunnerGroupPredicate_OnlyImpairingChanges verifies the watch predicate fires
// on create/delete and on an impairing-condition flip, but not on unrelated status
// churn (e.g. activeSessions / a Ready transition).
func TestRunnerGroupPredicate_OnlyImpairingChanges(t *testing.T) {
	p := runnerGroupImpairingConditionsChanged()

	base := ownedRunnerGroup("rg", "g", "ns")
	assert.True(t, p.Create(event.CreateEvent{Object: base}), "create must enqueue")
	assert.True(t, p.Delete(event.DeleteEvent{Object: base}), "delete must enqueue")

	// Unrelated churn: only a Ready transition (not in the impairing set).
	withReady := base.DeepCopy()
	withReady.Status.Conditions = []metav1.Condition{{Type: agcv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Up"}}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: withReady}),
		"a non-impairing condition change must not enqueue")

	// Impairing flip: CredentialUnavailable goes True.
	impaired := base.DeepCopy()
	impaired.Status.Conditions = []metav1.Condition{{Type: agcv1alpha1.ConditionCredentialUnavailable, Status: metav1.ConditionTrue, Reason: "TokenUnavailable"}}
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: impaired}),
		"an impairing-condition flip must enqueue")
}
