package controller

import (
	"context"
	"errors"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// v2StatusUpdateClient wraps a fake client, forcing every status subresource
// update on the given v2 ActionsGateway to return updErr.
func v2StatusUpdateClient(t *testing.T, ag *gmcv2alpha1.ActionsGateway, updErr error) client.Client {
	t.Helper()
	s := actionsGatewayV2TestScheme(t)
	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ag).
		WithStatusSubresource(ag).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return updErr
			},
		}).
		Build()
}

// TestV2SetDegraded_WritesConditionAndReturnsCause verifies the happy path:
// setDegraded records Degraded=True/Ready=False naming the cause and returns
// the original cause so the reconcile requeues with backoff.
func TestV2SetDegraded_WritesConditionAndReturnsCause(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "gh-app-creds", "")
	cause := errors.New("AGC Deployment: create rejected")

	r := &ActionsGatewayV2Reconciler{Client: v2StatusUpdateClient(t, ag, nil)}
	_, err := r.setDegraded(context.Background(), ag, cause)
	require.ErrorIs(t, err, cause, "setDegraded must return the original cause so the reconcile requeues")

	deg := meta.FindStatusCondition(ag.Status.Conditions, gmcv2alpha1.ConditionDegraded)
	require.NotNil(t, deg, "Degraded condition must be set")
	assert.Equal(t, metav1.ConditionTrue, deg.Status)
	assert.Equal(t, gmcv2alpha1.ReasonProvisioningFailed, deg.Reason)
	assert.Contains(t, deg.Message, "AGC Deployment")

	ready := meta.FindStatusCondition(ag.Status.Conditions, gmcv2alpha1.ConditionReady)
	require.NotNil(t, ready, "Ready=False must accompany Degraded")
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, gmcv2alpha1.ReasonProvisioningFailed, ready.Reason)
}

// TestV2SetDegraded_ConflictRequeuesWithoutError verifies the v2-specific
// conflict behaviour: unlike v1 (which always returns the cause), a status
// write conflict on the v2 reconciler swallows the cause and instead signals
// a plain requeue — the next reconcile will recompute from the fresher object.
func TestV2SetDegraded_ConflictRequeuesWithoutError(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "gh-app-creds", "")
	cause := errors.New("boom")
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: gmcv2alpha1.GroupVersion.Group, Resource: "actionsgateways"},
		ag.Name, errors.New("object was modified"))

	r := &ActionsGatewayV2Reconciler{Client: v2StatusUpdateClient(t, ag, conflict)}
	res, err := r.setDegraded(context.Background(), ag, cause)
	require.NoError(t, err, "a status-write conflict must not surface as an error")
	assert.Equal(t, ctrl.Result{Requeue: true}, res)
}

// TestV2SetDegraded_OtherStatusErrorPropagates verifies a non-conflict status
// write failure is returned (dropping the original cause), so the controller
// surfaces the more actionable underlying I/O error.
func TestV2SetDegraded_OtherStatusErrorPropagates(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "gh-app-creds", "")
	cause := errors.New("boom")
	writeErr := errors.New("etcd unavailable")

	r := &ActionsGatewayV2Reconciler{Client: v2StatusUpdateClient(t, ag, writeErr)}
	_, err := r.setDegraded(context.Background(), ag, cause)
	require.ErrorIs(t, err, writeErr)
}

// TestV2EnsureMetricsCerts_IssuesThenNoOps verifies the v2 metrics cert bundle
// is issued into both the server and client Secrets on first call, and a
// second call with a far-from-expiry cert leaves it untouched.
func TestV2EnsureMetricsCerts_IssuesThenNoOps(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ag := v2Gateway("gw", "team-a", "gh-app-creds", "")
	ag.UID = "gw-uid-1"
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.ensureMetricsCerts(context.Background(), ag))

	var server, clientSec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ag.Namespace, Name: metricsTLSSecretNameV2(ag)}, &server))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ag.Namespace, Name: metricsClientSecretNameV2(ag)}, &clientSec))
	assert.NotEmpty(t, server.Data[corev1.TLSCertKey], "server Secret must hold a cert")
	assert.NotEmpty(t, clientSec.Data[corev1.TLSCertKey], "client Secret must hold a cert")

	before := server.Data[corev1.TLSCertKey]
	require.NoError(t, r.ensureMetricsCerts(context.Background(), ag))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ag.Namespace, Name: metricsTLSSecretNameV2(ag)}, &server))
	assert.Equal(t, before, server.Data[corev1.TLSCertKey], "a valid bundle must not be re-issued")
}

// TestV2EnsureMetricsCerts_RegeneratesWhenClientSecretMissing verifies that if
// only the server Secret exists (e.g. a prior partial reconcile), the bundle is
// regenerated so the client Secret is created rather than left permanently
// absent.
func TestV2EnsureMetricsCerts_RegeneratesWhenClientSecretMissing(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := v2Gateway("gw", "team-a", "gh-app-creds", "")
	ag.UID = "gw-uid-1"

	bundle, err := generateMetricsCertsV2(ag.Namespace, agcNameV2(ag))
	require.NoError(t, err)
	serverSecret := buildMetricsTLSSecretV2(ag, bundle)
	// Only the server Secret pre-exists; the client Secret is missing.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(serverSecret).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.ensureMetricsCerts(context.Background(), ag))

	var clientSec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ag.Namespace, Name: metricsClientSecretNameV2(ag)}, &clientSec))
	assert.NotEmpty(t, clientSec.Data[corev1.TLSCertKey], "the missing client Secret must be created")
}
