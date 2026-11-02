package controller

import (
	"context"
	"errors"
	"testing"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestSetDegraded_WritesConditionNamingStepAndReturnsCause verifies the Q156
// behaviour: on a provisioning error setDegraded writes Degraded=True (naming the
// failing step in the message) and Ready=False, then returns the original cause so
// controller-runtime still requeues.
func TestSetDegraded_WritesConditionNamingStepAndReturnsCause(t *testing.T) {
	ag := newActionsGateway()
	cause := &provisioningError{step: "proxy Deployment + Service", err: errors.New("boom")}

	r := &ActionsGatewayReconciler{Client: statusUpdateClient(t, ag, nil)}
	_, err := r.setDegraded(context.Background(), ag, cause)
	require.ErrorIs(t, err, cause, "setDegraded must return the original cause so the reconcile requeues")

	deg := meta.FindStatusCondition(ag.Status.Conditions, gmcv1alpha1.ConditionDegraded)
	require.NotNil(t, deg, "Degraded condition must be set")
	assert.Equal(t, metav1.ConditionTrue, deg.Status)
	assert.Equal(t, gmcv1alpha1.ReasonProvisioningFailed, deg.Reason)
	assert.Contains(t, deg.Message, "proxy Deployment + Service",
		"Degraded message must name the failing step")

	ready := meta.FindStatusCondition(ag.Status.Conditions, gmcv1alpha1.ConditionReady)
	require.NotNil(t, ready, "Ready=False must accompany Degraded")
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, gmcv1alpha1.ReasonDegraded, ready.Reason)
}

// TestSetDegraded_FallsBackWhenUnwrapped verifies setDegraded still records a
// Degraded condition (with a generic step) when the cause is not a
// *provisioningError.
func TestSetDegraded_FallsBackWhenUnwrapped(t *testing.T) {
	ag := newActionsGateway()
	cause := errors.New("plain error")

	r := &ActionsGatewayReconciler{Client: statusUpdateClient(t, ag, nil)}
	_, err := r.setDegraded(context.Background(), ag, cause)
	require.ErrorIs(t, err, cause)

	deg := meta.FindStatusCondition(ag.Status.Conditions, gmcv1alpha1.ConditionDegraded)
	require.NotNil(t, deg)
	assert.Equal(t, metav1.ConditionTrue, deg.Status)
	assert.Contains(t, deg.Message, "reconcile", "falls back to a generic step label")
}

// TestSetDegraded_ReturnsCauseOnStatusConflict verifies a status-write conflict
// is swallowed (best-effort observability) while the original cause is still
// returned so the reconcile retries.
func TestSetDegraded_ReturnsCauseOnStatusConflict(t *testing.T) {
	ag := newActionsGateway()
	cause := &provisioningError{step: "AGC Deployment", err: errors.New("boom")}
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: gmcv1alpha1.GroupVersion.Group, Resource: "actionsgateways"},
		ag.Name, errors.New("object was modified"))

	r := &ActionsGatewayReconciler{Client: statusUpdateClient(t, ag, conflict)}
	_, err := r.setDegraded(context.Background(), ag, cause)
	require.ErrorIs(t, err, cause, "the provisioning cause must be returned even when the status write conflicts")
}

// TestReconcileResources_WrapsFailingStepAsProvisioningError verifies that a
// failure inside reconcileResources surfaces as a *provisioningError naming the
// in-progress step, so Reconcile can attribute the Degraded condition without
// parsing error strings.
func TestReconcileResources_WrapsFailingStepAsProvisioningError(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	boom := errors.New("create rejected")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return boom
			},
		}).
		Build()
	r := applyTestReconciler(t, c, scheme)

	err := r.reconcileResources(context.Background(), ag, nil, "proxy:8080")
	require.Error(t, err)
	var pe *provisioningError
	require.ErrorAs(t, err, &pe, "reconcileResources must wrap failures as *provisioningError")
	assert.NotEmpty(t, pe.step, "the failing step must be recorded")
}

// TestReconcileResources_PolicyPrecedesProxyDeployment fails the proxy Deployment
// create and asserts the proxy NetworkPolicy already exists, so a first provision
// that stops there leaves no proxy pod without a policy (Q1264).
func TestReconcileResources_PolicyPrecedesProxyDeployment(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return errors.New("deployment create rejected")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := applyTestReconciler(t, c, scheme)
	ctx := context.Background()

	err := r.reconcileResources(ctx, ag, nil, "proxy:8080")
	var pe *provisioningError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "proxy Deployment + Service", pe.step)

	key := types.NamespacedName{Namespace: ag.Namespace, Name: npProxyName}
	require.NoError(t, c.Get(ctx, key, &networkingv1.NetworkPolicy{}), "the policy must exist before the proxy Deployment is applied")
}
