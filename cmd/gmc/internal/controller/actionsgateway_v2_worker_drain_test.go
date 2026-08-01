package controller

import (
	"context"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// deletingV2GatewayFor returns a v2 ActionsGateway that has been deleting for `since`,
// which is what the worker-drain deadline is measured against.
func deletingV2GatewayFor(since time.Duration) *gmcv2alpha1.ActionsGateway {
	ag := deletingV2Gateway()
	deleted := metav1.NewTime(time.Now().Add(-since))
	ag.DeletionTimestamp = &deleted
	return ag
}

// workingRunnerSet is a RunnerSet bound to gw reporting live worker pods.
func workingRunnerSet(name, ns string, active, pending int32) *gmcv2alpha1.RunnerSet {
	rs := boundRunnerSet(name, ns, "gw")
	rs.Status.ActiveJobs = active
	rs.Status.PendingJobs = pending
	return rs
}

// TestReconcileDeleteV2_HoldsForWorkerDrain covers Q547's GMC half. The AGC reaps the
// tenant's worker pods when it sees the gateway's deletion timestamp, but the GMC
// deletes its Deployment — and, a few round trips later, its RoleBinding and
// ServiceAccount — far faster than the AGC can act. Teardown therefore has to hold
// before it deletes anything, or the pods are stranded with no reaper.
func TestReconcileDeleteV2_HoldsForWorkerDrain(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := "team-a"
	ag := deletingV2GatewayFor(10 * time.Second)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ns}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, dep, workingRunnerSet("busy", ns, 1, 0)).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	res, err := r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "teardown must requeue while workers are still up")

	assert.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: agcNameV2(ag)}, &appsv1.Deployment{}),
		"the AGC Deployment must survive the wait — it is the only thing that can reap the workers")
}

// TestReconcileDeleteV2_ProceedsOnceDrained: zero counts release the gate immediately.
// The counts fall as soon as the AGC ISSUES its deletes, because its reaper stops
// counting a pod that already carries a deletion timestamp — from that moment the
// kubelet finishes the pod with no controller involved.
func TestReconcileDeleteV2_ProceedsOnceDrained(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := "team-a"
	ag := deletingV2GatewayFor(10 * time.Second)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ns}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, dep, workingRunnerSet("drained", ns, 0, 0)).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	_, err := r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err)
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: agcNameV2(ag)}, &appsv1.Deployment{}),
		"a drained gateway must tear down on the first pass, with no added latency")
}

// TestReconcileDeleteV2_WorkerDrainTimesOut: an AGC that is crashed, scaled to zero or
// never healthy updates no counts, so the gate would hold the gateway forever. Past the
// deadline teardown proceeds and says what it is leaving behind.
func TestReconcileDeleteV2_WorkerDrainTimesOut(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := "team-a"
	ag := deletingV2GatewayFor(workerDrainTimeout + time.Minute)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ns}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, dep, workingRunnerSet("stuck", ns, 2, 1)).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	_, err := r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err)
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: agcNameV2(ag)}, &appsv1.Deployment{}),
		"teardown must not be held open forever by an AGC that will never reap")
}

// TestUndrainedRunnerSets_ScopeAndReporting: only sets bound to this gateway count, and
// the report names the counts so the operator can see what is holding teardown.
func TestUndrainedRunnerSets_ScopeAndReporting(t *testing.T) {
	ns := "team-a"
	ag := deletingV2GatewayFor(time.Second)

	neighbour := workingRunnerSet("neighbour", ns, 5, 5)
	neighbour.Spec.GatewayRef = gmcv2alpha1.ObjectRef{Name: "other-gw"}

	r := v2RollupReconciler(t,
		workingRunnerSet("busy", ns, 2, 3),
		workingRunnerSet("idle", ns, 0, 0),
		neighbour,
	)

	got := r.undrainedRunnerSets(context.Background(), ag)
	require.Len(t, got, 1, "an idle set and another gateway's set must not hold this teardown")
	assert.Equal(t, "busy (2 active, 3 pending)", got[0])
}
