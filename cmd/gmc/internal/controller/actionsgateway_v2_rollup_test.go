package controller

import (
	"context"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// boundRunnerSet builds a RunnerSet targeting gatewayName via spec.gatewayRef, with the
// given status conditions.
func boundRunnerSet(name, ns, gatewayName string, conds ...metav1.Condition) *gmcv2alpha1.RunnerSet {
	return &gmcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       gmcv2alpha1.RunnerSetSpec{GatewayRef: gmcv2alpha1.ObjectRef{Name: gatewayName}},
		Status:     gmcv2alpha1.RunnerSetStatus{Conditions: conds},
	}
}

func v2RollupReconciler(t *testing.T, objs ...client.Object) *ActionsGatewayV2Reconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(actionsGatewayV2TestScheme(t)).WithObjects(objs...).Build()
	return &ActionsGatewayV2Reconciler{Client: c}
}

// TestEvalRunnerSetHealth_NoBoundSets: a gateway with no bound RunnerSets is healthy.
func TestEvalRunnerSetHealth_NoBoundSets(t *testing.T) {
	ag := v2Gateway("gw", "ns", "github-app", "shared")
	r := v2RollupReconciler(t)
	h := r.evalRunnerSetHealth(context.Background(), ag)
	assert.False(t, h.degraded)
	assert.Equal(t, gmcv2alpha1.ReasonAllRunnerSetsHealthy, h.reason)
	assert.Contains(t, h.message, "all 0 bound RunnerSet(s) healthy")
}

// TestEvalRunnerSetHealth_AllHealthy: a bound set that is Ready=True (and a bound set
// with no conditions yet) leaves the rollup False.
func TestEvalRunnerSetHealth_AllHealthy(t *testing.T) {
	ag := v2Gateway("gw", "ns", "github-app", "shared")
	ready := boundRunnerSet("rs-ready", "ns", "gw", metav1.Condition{
		Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonListenerActive,
	})
	// A set awaiting its first listener (transient NoActiveSessions) is not impaired.
	starting := boundRunnerSet("rs-start", "ns", "gw", metav1.Condition{
		Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: gmcv2alpha1.ReasonNoActiveSessions,
	})
	r := v2RollupReconciler(t, ready, starting)
	h := r.evalRunnerSetHealth(context.Background(), ag)
	assert.False(t, h.degraded, "Ready=True and transient NoActiveSessions must not trip the rollup")
	assert.Equal(t, gmcv2alpha1.ReasonAllRunnerSetsHealthy, h.reason)
	assert.Contains(t, h.message, "all 2 bound RunnerSet(s) healthy")
}

// TestEvalRunnerSetHealth_Impaired: a non-transient Ready=False and a WorkersUnschedulable
// set both trip the rollup, which names them and their tripped signals; a healthy set and
// a set bound to a different gateway are excluded.
func TestEvalRunnerSetHealth_Impaired(t *testing.T) {
	ag := v2Gateway("gw", "ns", "github-app", "shared")
	tokenBad := boundRunnerSet("rs-token", "ns", "gw", metav1.Condition{
		Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: gmcv2alpha1.ReasonTokenUnavailable,
	})
	unsched := boundRunnerSet("rs-unsched", "ns", "gw",
		metav1.Condition{Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonListenerActive},
		metav1.Condition{Type: gmcv2alpha1.ConditionWorkersUnschedulable, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonPodsUnschedulable},
	)
	good := boundRunnerSet("rs-good", "ns", "gw", metav1.Condition{
		Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonListenerActive,
	})
	otherOwner := boundRunnerSet("rs-other", "ns", "other-gw", metav1.Condition{
		Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: gmcv2alpha1.ReasonGatewayNotFound,
	})
	r := v2RollupReconciler(t, tokenBad, unsched, good, otherOwner)

	h := r.evalRunnerSetHealth(context.Background(), ag)
	assert.True(t, h.degraded)
	assert.Equal(t, gmcv2alpha1.ReasonRunnerSetsImpaired, h.reason)
	assert.Contains(t, h.message, "2 of 3 RunnerSet(s) impaired")
	assert.Contains(t, h.message, "rs-token")
	assert.Contains(t, h.message, gmcv2alpha1.ConditionReady+"="+gmcv2alpha1.ReasonTokenUnavailable)
	assert.Contains(t, h.message, "rs-unsched")
	assert.Contains(t, h.message, gmcv2alpha1.ConditionWorkersUnschedulable)
	assert.NotContains(t, h.message, "rs-good", "healthy sets must not be named")
	assert.NotContains(t, h.message, "rs-other", "a set bound to a different gateway must not be counted")
}

// TestEvalRunnerSetHealth_AdvisoryExcluded: advisory conditions (WorkerQuota ladder,
// EgressUnattributed, PossibleReapBlockingSidecar) on a Ready=True set do not trip the
// rollup — they are trade-off/throughput signals, not "the set is broken".
func TestEvalRunnerSetHealth_AdvisoryExcluded(t *testing.T) {
	ag := v2Gateway("gw", "ns", "github-app", "shared")
	advisory := boundRunnerSet("rs-adv", "ns", "gw",
		metav1.Condition{Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonListenerActive},
		metav1.Condition{Type: gmcv2alpha1.ConditionWorkerQuotaExceeded, Status: metav1.ConditionTrue, Reason: "QuotaExceeded"},
		metav1.Condition{Type: gmcv2alpha1.ConditionEgressUnattributed, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonDirectEgress},
		metav1.Condition{Type: gmcv2alpha1.ConditionPossibleReapBlockingSidecar, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonReapBlockingSidecar},
	)
	r := v2RollupReconciler(t, advisory)
	h := r.evalRunnerSetHealth(context.Background(), ag)
	assert.False(t, h.degraded, "advisory conditions must not trip the rollup")
}

// TestEvalRunnerSetHealth_ListenerPushedImpairment: a listener-pushed abnormal-is-True
// condition (Q330) trips the rollup even when Ready has converged to the benign
// NoActiveSessions — the exact classic-set-with-revoked-credentials shape, where the
// listener pushes Degraded=Unauthorized while no session is active. RunnerVersionTooOld
// trips it the same way; the advisory RateLimited does not.
func TestEvalRunnerSetHealth_ListenerPushedImpairment(t *testing.T) {
	ag := v2Gateway("gw", "ns", "github-app", "shared")
	// Revoked credentials: the shared listener pushes Degraded=Unauthorized while Ready
	// sits at the benign NoActiveSessions (no session could be created). Axis (1) alone
	// would read this set as healthy — the bug Q330 fixes.
	revoked := boundRunnerSet("rs-revoked", "ns", "gw",
		metav1.Condition{Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: gmcv2alpha1.ReasonNoActiveSessions},
		metav1.Condition{Type: gmcv2alpha1.ConditionDegraded, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonSessionUnauthorized},
	)
	// A too-old runner: RunnerVersionTooOld while Ready=True.
	tooOld := boundRunnerSet("rs-old", "ns", "gw",
		metav1.Condition{Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonListenerActive},
		metav1.Condition{Type: gmcv2alpha1.ConditionRunnerVersionTooOld, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonVersionTooOld},
	)
	// Rate-limited but otherwise healthy: advisory, excluded.
	limited := boundRunnerSet("rs-limited", "ns", "gw",
		metav1.Condition{Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonListenerActive},
		metav1.Condition{Type: gmcv2alpha1.ConditionRateLimited, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonSustainedRateLimit},
	)
	r := v2RollupReconciler(t, revoked, tooOld, limited)

	h := r.evalRunnerSetHealth(context.Background(), ag)
	assert.True(t, h.degraded)
	assert.Equal(t, gmcv2alpha1.ReasonRunnerSetsImpaired, h.reason)
	assert.Contains(t, h.message, "2 of 3 RunnerSet(s) impaired")
	assert.Contains(t, h.message, "rs-revoked")
	assert.Contains(t, h.message, gmcv2alpha1.ConditionDegraded)
	assert.Contains(t, h.message, "rs-old")
	assert.Contains(t, h.message, gmcv2alpha1.ConditionRunnerVersionTooOld)
	assert.NotContains(t, h.message, "rs-limited", "RateLimited is advisory and must not trip the rollup")
}

// TestRunnerSetPredicate_WakesOnListenerImpairment verifies the watch predicate fires when
// a listener-pushed impairing condition flips (Q330) so the rollup refreshes promptly, but
// still ignores the advisory RateLimited flip.
func TestRunnerSetPredicate_WakesOnListenerImpairment(t *testing.T) {
	p := runnerSetImpairmentChanged()

	// Ready stays benign; Degraded flips True — the signature changes, so it must enqueue.
	base := boundRunnerSet("rs", "ns", "gw", metav1.Condition{
		Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: gmcv2alpha1.ReasonNoActiveSessions,
	})
	degraded := base.DeepCopy()
	degraded.Status.Conditions = append(degraded.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionDegraded, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonSessionUnauthorized,
	})
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: degraded}),
		"a listener-pushed Degraded flip must enqueue")

	// RateLimited flips True: advisory, signature unchanged.
	limited := base.DeepCopy()
	limited.Status.Conditions = append(limited.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionRateLimited, Status: metav1.ConditionTrue, Reason: gmcv2alpha1.ReasonSustainedRateLimit,
	})
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: limited}),
		"an advisory RateLimited flip must not enqueue")
}

// TestRunnerSetPredicate_OnlyImpairmentChanges verifies the watch predicate fires on
// create/delete and when a set's impaired signature flips, but not on unrelated status
// churn (an advisory condition or a benign Ready reason).
func TestRunnerSetPredicate_OnlyImpairmentChanges(t *testing.T) {
	p := runnerSetImpairmentChanged()

	base := boundRunnerSet("rs", "ns", "gw")
	assert.True(t, p.Create(event.CreateEvent{Object: base}), "create must enqueue")
	assert.True(t, p.Delete(event.DeleteEvent{Object: base}), "delete must enqueue")

	// Unrelated churn: an advisory condition flips True — the impaired signature is unchanged.
	advisory := base.DeepCopy()
	advisory.Status.Conditions = []metav1.Condition{
		{Type: gmcv2alpha1.ConditionWorkerQuotaPressure, Status: metav1.ConditionTrue, Reason: "Pressure"},
	}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: advisory}),
		"an advisory-condition change must not enqueue")

	// Impairment flip: Ready goes False for a non-transient reason.
	impaired := base.DeepCopy()
	impaired.Status.Conditions = []metav1.Condition{
		{Type: gmcv2alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: gmcv2alpha1.ReasonTokenUnavailable},
	}
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: impaired}),
		"an impairment flip must enqueue")
}

// TestRunnerSetToActionsGateway maps a RunnerSet to its bound gateway request, and maps
// a set with no gatewayRef.name to nothing.
func TestRunnerSetToActionsGateway(t *testing.T) {
	r := &ActionsGatewayV2Reconciler{}
	reqs := r.runnerSetToActionsGateway(context.Background(), boundRunnerSet("rs", "ns", "gw"))
	assert.Len(t, reqs, 1)
	assert.Equal(t, "gw", reqs[0].Name)
	assert.Equal(t, "ns", reqs[0].Namespace)

	assert.Empty(t, r.runnerSetToActionsGateway(context.Background(), boundRunnerSet("rs", "ns", "")),
		"a set with no gatewayRef.name maps to no request")
}
