package controller

import (
	"context"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// deletingV2GatewayFor returns a v2 ActionsGateway that has been deleting for `since`,
// which is what the worker-drain deadline is measured against.
func deletingV2GatewayFor(since time.Duration) *gmcv2.ActionsGateway {
	ag := deletingV2Gateway()
	deleted := metav1.NewTime(time.Now().Add(-since))
	ag.DeletionTimestamp = &deleted
	return ag
}

// workingRunnerSet is a RunnerSet bound to gw reporting live worker pods.
func workingRunnerSet(name, ns string, active, pending int32) *gmcv2.RunnerSet {
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
	neighbour.Spec.GatewayRef = gmcv2.ObjectRef{Name: "other-gw"}

	r := v2RollupReconciler(t,
		workingRunnerSet("busy", ns, 2, 3),
		workingRunnerSet("idle", ns, 0, 0),
		neighbour,
	)

	got := r.undrainedRunnerSets(context.Background(), ag)
	require.Len(t, got, 1, "an idle set and another gateway's set must not hold this teardown")
	assert.Equal(t, "busy (2 active, 3 pending)", got[0])
}

// workerPod is a pod the AGC provisioned for RunnerSet rs: selected by the workload
// NetworkPolicy and carrying the RunnerSet owner label.
func workerPod(name, ns, rs string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns,
		Labels: map[string]string{labelComponent: componentWorkload, labelRunnerSetOwner: rs},
	}}
}

// workloadNP is the gateway's workload NetworkPolicy, seeded so teardown has it to delete.
func workloadNP(ag *gmcv2.ActionsGateway) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: workloadNPNameV2(ag), Namespace: ag.Namespace}}
}

// TestReconcileDeleteV2_WorkerDrainTimeoutKeepsWorkloadPolicy (Q1328): past the drain
// deadline teardown proceeds without the AGC, but a worker pod it never reaped is still
// running job code. Deleting the workload NetworkPolicy then would leave that pod with
// no egress restriction for up to maxWorkerLifetime, so the policy — and the gateway's
// finalizer — stay until the pod is gone. Everything else is still torn down.
func TestReconcileDeleteV2_WorkerDrainTimeoutKeepsWorkloadPolicy(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := "team-a"
	ag := deletingV2GatewayFor(workerDrainTimeout + time.Minute)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ns}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, dep, workloadNP(ag), workingRunnerSet("stuck", ns, 1, 0), workerPod("stuck-w1", ns, "stuck")).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	res, err := r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "teardown must poll until the worker pod is gone")
	assert.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(workloadNP(ag)), &networkingv1.NetworkPolicy{}),
		"the workload NetworkPolicy must outlive a worker pod still running job code")
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: agcNameV2(ag)}, &appsv1.Deployment{}),
		"the rest of the control plane is still torn down")
	var got gmcv2.ActionsGateway
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ag), &got))
	assert.Contains(t, got.Finalizers, gmcv2.ActionsGatewayFinalizer, "the finalizer holds the policy's teardown open")

	// The pod goes: the next pass deletes the policy and releases the gateway.
	require.NoError(t, c.Delete(context.Background(), workerPod("stuck-w1", ns, "stuck")))
	_, err = r.reconcileDelete(context.Background(), &got)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(workloadNP(ag)), &networkingv1.NetworkPolicy{})),
		"with no worker pod left the workload NetworkPolicy is deleted")
}

// TestReconcileDeleteV2_TerminatingWorkerKeepsWorkloadPolicy (Q1328): the healthy path.
// The AGC's counts reach zero as soon as it ISSUES its deletes, while the kubelet is
// still running the pod through its terminationGracePeriodSeconds, which the tenant
// template sets. A pod carrying a deletion timestamp is still running job code.
func TestReconcileDeleteV2_TerminatingWorkerKeepsWorkloadPolicy(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := "team-a"
	ag := deletingV2GatewayFor(10 * time.Second)
	pod := workerPod("drained-w1", ns, "drained")
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"test.actions-gateway.com/hold"}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, workloadNP(ag), workingRunnerSet("drained", ns, 0, 0), pod).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	_, err := r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err)
	assert.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(workloadNP(ag)), &networkingv1.NetworkPolicy{}),
		"a terminating worker pod is still running job code and must stay confined")
}

// TestReconcileDeleteV2_OtherGatewaysWorkersDoNotHold (Q1328): the hold is scoped to
// this gateway's RunnerSets, so a neighbour gateway's busy workers in the same
// namespace never pin this teardown.
func TestReconcileDeleteV2_OtherGatewaysWorkersDoNotHold(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := "team-a"
	ag := deletingV2GatewayFor(10 * time.Second)
	neighbour := boundRunnerSet("neighbour", ns, "other-gw")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, workloadNP(ag), neighbour, workerPod("neighbour-w1", ns, "neighbour")).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	_, err := r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(workloadNP(ag)), &networkingv1.NetworkPolicy{})),
		"another gateway's worker pods must not hold this gateway's teardown")
}
