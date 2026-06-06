package controller

import (
	"context"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// applyTestScheme registers every API group the apply* helpers operate on.
func applyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, rbacv1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, policyv1.AddToScheme(s))
	require.NoError(t, autoscalingv2.AddToScheme(s))
	require.NoError(t, gmcv1alpha1.AddToScheme(s))
	require.NoError(t, agcv1alpha1.AddToScheme(s))
	return s
}

func applyTestReconciler(t *testing.T, c client.Client, scheme *runtime.Scheme) *ActionsGatewayReconciler {
	t.Helper()
	return &ActionsGatewayReconciler{Client: c, Scheme: scheme}
}

// applyTestAG returns an ActionsGateway with a UID so SetControllerReference
// stamps a usable owner reference.
func applyTestAG() *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "tenant-ns", UID: "ag-uid-1"},
	}
}

// TestApplyServiceAccount_CreateThenPatch verifies the create path then an
// idempotent patch path that updates only the managed labels.
func TestApplyServiceAccount_CreateThenPatch(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	require.NoError(t, r.applyServiceAccount(context.Background(), buildAGCServiceAccount(ag)))

	var sa corev1.ServiceAccount
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcSAName}, &sa))
	assert.Equal(t, managedLabels(ag), sa.Labels)

	// Second apply is a no-op patch and must not error.
	require.NoError(t, r.applyServiceAccount(context.Background(), buildAGCServiceAccount(ag)))
}

// TestApplyService_PreservesClusterIP is the key behavioural guarantee of the
// CreateOrPatch migration: patching the Service updates the managed Spec fields
// (selector/ports/type) while leaving the server-assigned ClusterIP intact. A
// whole-object replace would have wiped it.
func TestApplyService_PreservesClusterIP(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()

	// Pre-existing Service with a server-assigned ClusterIP and a stale port.
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: ag.Namespace},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.0.0.42",
			Selector:  map[string]string{"app": "stale"},
			Ports:     []corev1.ServicePort{{Name: "stale", Port: 1, TargetPort: intstr.FromInt32(1)}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := applyTestReconciler(t, c, scheme)

	require.NoError(t, r.applyService(context.Background(), buildProxyService(ag)))

	var svc corev1.Service
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: proxyServiceName}, &svc))
	assert.Equal(t, "10.0.0.42", svc.Spec.ClusterIP, "server-assigned ClusterIP must be preserved")
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, int32(proxyPort), svc.Spec.Ports[0].Port, "managed ports must be updated")
	assert.Equal(t, map[string]string{"app": proxyAppName}, svc.Spec.Selector)
}

// TestApplyDeployment_SetsOwnerReference verifies the create path stamps a
// controller owner reference (so the Owns(Deployment) watch fires) and that a
// subsequent apply patches the spec without dropping the owner reference.
func TestApplyDeployment_SetsOwnerReference(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	require.NoError(t, r.applyDeployment(context.Background(), ag, buildProxyDeployment(ag, "proxy:test")))

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: proxyServiceName}, &dep))
	require.Len(t, dep.OwnerReferences, 1)
	assert.Equal(t, ag.Name, dep.OwnerReferences[0].Name)
	require.NotNil(t, dep.OwnerReferences[0].Controller)
	assert.True(t, *dep.OwnerReferences[0].Controller)

	// Re-apply with a changed image; owner reference must survive the patch.
	require.NoError(t, r.applyDeployment(context.Background(), ag, buildProxyDeployment(ag, "proxy:v2")))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: proxyServiceName}, &dep))
	require.Len(t, dep.OwnerReferences, 1)
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "proxy:v2", dep.Spec.Template.Spec.Containers[0].Image)
}

// TestApplyRoleBinding_RecreatesOnRoleRefChange verifies the immutable-roleRef
// upgrade path deletes and recreates the binding rather than attempting a
// (server-rejected) patch, while the steady-state path performs no delete.
func TestApplyRoleBinding_RecreatesOnRoleRefChange(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()

	// Legacy binding referencing a per-tenant Role (immutable roleRef differs).
	legacy := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: agcSAName, Namespace: ag.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "legacy-agc-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agcSAName, Namespace: ag.Namespace}},
	}

	var deletes int
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes++
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := applyTestReconciler(t, c, scheme)

	require.NoError(t, r.applyRoleBinding(context.Background(), buildAGCRoleBinding(ag)))
	assert.Equal(t, 1, deletes, "roleRef change must delete+recreate")

	var rb rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcSAName}, &rb))
	assert.Equal(t, "ClusterRole", rb.RoleRef.Kind)
	assert.Equal(t, agcTenantRoleName, rb.RoleRef.Name)

	// Steady-state re-apply: roleRef unchanged, so no further delete.
	require.NoError(t, r.applyRoleBinding(context.Background(), buildAGCRoleBinding(ag)))
	assert.Equal(t, 1, deletes, "unchanged roleRef must not trigger a delete")
}

// TestApplyOwnedSecret_CreateThenUpdate verifies the owned-Secret helper stamps
// an owner reference on create and patches the data on update.
func TestApplyOwnedSecret_CreateThenUpdate(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("v1")},
	}
	require.NoError(t, r.applyOwnedSecret(context.Background(), ag, sec))

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: "owned"}, &got))
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, ag.Name, got.OwnerReferences[0].Name)
	assert.Equal(t, []byte("v1"), got.Data["tls.crt"])

	sec.Data["tls.crt"] = []byte("v2")
	require.NoError(t, r.applyOwnedSecret(context.Background(), ag, sec))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: "owned"}, &got))
	assert.Equal(t, []byte("v2"), got.Data["tls.crt"])
	require.Len(t, got.OwnerReferences, 1)
}
