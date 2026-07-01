package controller

import (
	"context"
	"errors"
	"testing"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestApplyNamespacePSA_StampsDefaultProfile verifies the happy path: with no
// spec.securityProfile override, the six PSA labels are stamped at the
// "restricted" default level onto the tenant namespace via Server-Side Apply.
func TestApplyNamespacePSA_StampsDefaultProfile(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ag.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	r := applyTestReconciler(t, c, scheme)

	require.NoError(t, r.applyNamespacePSA(context.Background(), ag))

	var got corev1.Namespace
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ag.Namespace}, &got))
	assert.Equal(t, defaultSecurityProfile, got.Labels["pod-security.kubernetes.io/enforce"])
	assert.Equal(t, defaultSecurityProfile, got.Labels["pod-security.kubernetes.io/warn"])
	assert.Equal(t, defaultSecurityProfile, got.Labels["pod-security.kubernetes.io/audit"])
	assert.Equal(t, "latest", got.Labels["pod-security.kubernetes.io/enforce-version"])
}

// TestApplyNamespacePSA_UsesConfiguredProfile verifies a non-default
// spec.securityProfile is stamped verbatim rather than the platform default.
func TestApplyNamespacePSA_UsesConfiguredProfile(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	ag.Spec.SecurityProfile = "privileged"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ag.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	r := applyTestReconciler(t, c, scheme)

	require.NoError(t, r.applyNamespacePSA(context.Background(), ag))

	var got corev1.Namespace
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ag.Namespace}, &got))
	assert.Equal(t, "privileged", got.Labels["pod-security.kubernetes.io/enforce"])
}

// TestApplyNamespacePSA_ForbiddenReturnsError verifies that when the
// namespace-psa-guard ValidatingAdmissionPolicy denies the patch (surfaced as
// IsForbidden), applyNamespacePSA propagates the error rather than retrying
// with ForceOwnership — a retry cannot help until an operator labels the
// namespace as managed — and emits an actionable Warning Event naming the
// marker label the operator must apply.
func TestApplyNamespacePSA_ForbiddenReturnsError(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "namespaces"}, ag.Namespace, errors.New("denied by admission policy"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return forbidden
		},
	}).Build()
	rec := events.NewFakeRecorder(1)
	r := applyTestReconciler(t, c, scheme)
	r.Recorder = rec

	err := r.applyNamespacePSA(context.Background(), ag)
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err))

	select {
	case msg := <-rec.Events:
		assert.Contains(t, msg, TenantNamespaceMarkerLabel, "the event must name the marker label the operator must apply")
	default:
		t.Fatal("expected a Warning Event on the ActionsGateway naming the missing marker label")
	}
}

// TestApplyNamespacePSA_ConflictRetriesWithForceOwnership verifies that when
// the first apply hits a field-manager conflict (another manager owns a PSA
// key), applyNamespacePSA retries with ForceOwnership and succeeds, restamping
// the controller's labels and emitting a Warning Event describing the
// out-of-band modification.
func TestApplyNamespacePSA_ConflictRetriesWithForceOwnership(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ag.Namespace}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()

	conflict := apierrors.NewConflict(
		schema.GroupResource{Resource: "namespaces"}, ag.Namespace, errors.New("field manager conflict"))
	var applyCalls int
	c := interceptor.NewClient(base, interceptor.Funcs{
		Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			applyCalls++
			if applyCalls == 1 {
				return conflict
			}
			return cl.Apply(ctx, obj, opts...)
		},
	})
	rec := events.NewFakeRecorder(1)
	r := applyTestReconciler(t, c, scheme)
	r.Recorder = rec

	require.NoError(t, r.applyNamespacePSA(context.Background(), ag))
	assert.Equal(t, 2, applyCalls, "a field-manager conflict must trigger exactly one ForceOwnership retry")

	var got corev1.Namespace
	require.NoError(t, base.Get(context.Background(), types.NamespacedName{Name: ag.Namespace}, &got))
	assert.Equal(t, defaultSecurityProfile, got.Labels["pod-security.kubernetes.io/enforce"])

	select {
	case msg := <-rec.Events:
		assert.Contains(t, msg, "PSALabelsOverridden")
	default:
		t.Fatal("expected a Warning Event describing the out-of-band PSA label modification")
	}
}

// TestApplyNamespacePSA_OtherErrorPropagates verifies an unrelated apply error
// (neither Forbidden nor Conflict) is returned as-is without a retry.
func TestApplyNamespacePSA_OtherErrorPropagates(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	boom := errors.New("etcd unavailable")
	var applyCalls int
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			applyCalls++
			return boom
		},
	}).Build()
	r := applyTestReconciler(t, c, scheme)

	err := r.applyNamespacePSA(context.Background(), ag)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1, applyCalls, "an unrelated error must not trigger the ForceOwnership retry")
}

// TestProxyMaxReplicas_DefaultsWhenUnset verifies the platform default of 10
// applies when spec.proxy.maxReplicas is nil.
func TestProxyMaxReplicas_DefaultsWhenUnset(t *testing.T) {
	ag := &gmcv1alpha1.ActionsGateway{}
	assert.Equal(t, int32(10), proxyMaxReplicas(ag))
}

// TestProxyMaxReplicas_UsesOverride verifies an explicit spec.proxy.maxReplicas
// override takes precedence over the default.
func TestProxyMaxReplicas_UsesOverride(t *testing.T) {
	override := int32(42)
	ag := &gmcv1alpha1.ActionsGateway{
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			Proxy: gmcv1alpha1.ProxyConfig{MaxReplicas: &override},
		},
	}
	assert.Equal(t, int32(42), proxyMaxReplicas(ag))
}
