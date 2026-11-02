package controller

import (
	"context"
	"maps"
	"testing"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
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
	require.NoError(t, gmcv2.AddToScheme(s))
	return s
}

func applyTestReconciler(t *testing.T, c client.Client, scheme *runtime.Scheme) *ActionsGatewayV2Reconciler {
	t.Helper()
	return &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}
}

// applyTestAG returns an ActionsGateway with a UID so SetControllerReference
// stamps a usable owner reference.
func applyTestAG() *gmcv2.ActionsGateway {
	return &gmcv2.ActionsGateway{
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

	desired := buildAGCServiceAccountV2(ag)
	require.NoError(t, r.applyServiceAccount(context.Background(), ag, desired))

	var sa corev1.ServiceAccount
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(desired), &sa))
	assert.Equal(t, desired.Labels, sa.Labels)

	// Second apply is a no-op patch and must not error.
	require.NoError(t, r.applyServiceAccount(context.Background(), ag, buildAGCServiceAccountV2(ag)))
}

// TestApplyService_PreservesClusterIP is the key behavioural guarantee of the
// CreateOrPatch path: patching the Service updates the managed Spec fields
// (selector/ports) while leaving the server-assigned ClusterIP intact. A
// whole-object replace would have wiped it.
func TestApplyService_PreservesClusterIP(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	desired := buildAGCServiceV2(ag)

	// Pre-existing Service with a server-assigned ClusterIP and a stale port.
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: ag.Namespace},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.0.0.42",
			Selector:  map[string]string{"app": "stale"},
			Ports:     []corev1.ServicePort{{Name: "stale", Port: 1, TargetPort: intstr.FromInt32(1)}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := applyTestReconciler(t, c, scheme)

	require.NoError(t, r.applyService(context.Background(), ag, buildAGCServiceV2(ag)))

	var svc corev1.Service
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(desired), &svc))
	assert.Equal(t, "10.0.0.42", svc.Spec.ClusterIP, "server-assigned ClusterIP must be preserved")
	assert.Equal(t, desired.Spec.Ports, svc.Spec.Ports, "managed ports must be updated")
	assert.Equal(t, desired.Spec.Selector, svc.Spec.Selector)
}

// TestApplyDeployment_SetsOwnerReference verifies the create path stamps a
// controller owner reference (so the Owns(Deployment) watch fires) and that a
// subsequent apply patches the spec without dropping the owner reference.
func TestApplyDeployment_SetsOwnerReference(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()
	key := types.NamespacedName{Namespace: ag.Namespace, Name: agcNameV2(ag)}

	require.NoError(t, r.applyDeployment(context.Background(), ag, buildAGCDeploymentV2(ag, "agc:test", nil, "restricted", nil)))

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), key, &dep))
	require.Len(t, dep.OwnerReferences, 1)
	assert.Equal(t, ag.Name, dep.OwnerReferences[0].Name)
	require.NotNil(t, dep.OwnerReferences[0].Controller)
	assert.True(t, *dep.OwnerReferences[0].Controller)

	// Re-apply with a changed image; owner reference must survive the patch.
	require.NoError(t, r.applyDeployment(context.Background(), ag, buildAGCDeploymentV2(ag, "agc:v2", nil, "restricted", nil)))
	require.NoError(t, c.Get(context.Background(), key, &dep))
	require.Len(t, dep.OwnerReferences, 1)
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "agc:v2", dep.Spec.Template.Spec.Containers[0].Image)
}

// TestApplyOwnedSecret_CreateThenUpdate verifies the owned-Secret path creates
// with an owner reference and then updates the data in place.
func TestApplyOwnedSecret_CreateThenUpdate(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
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

// foreignLabelKey/Value is a label another controller or an operator set on a GMC child.
const foreignLabelKey, foreignLabelValue = "example.com/owned-elsewhere", "kept"

// seededLabels returns want's labels with one GMC-owned key drifted and a foreign
// key added, as a live object would carry them after someone else edited it.
func seededLabels(want map[string]string) map[string]string {
	l := map[string]string{foreignLabelKey: foreignLabelValue}
	for k := range want {
		l[k] = "drifted"
	}
	return l
}

// assertLabelsMerged checks that every desired label is written and the foreign
// one is left alone (Q1215).
func assertLabelsMerged(t *testing.T, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		assert.Equal(t, v, got[k], "GMC-owned label %s", k)
	}
	assert.Equal(t, foreignLabelValue, got[foreignLabelKey], "foreign label must survive the apply")
	assert.Len(t, got, len(want)+1)
}

// TestApplyManagedChild_PreservesForeignLabels seeds a live child carrying a label
// the GMC never wrote, re-applies it, and asserts the label survives while the
// GMC's own keys are corrected.
func TestApplyManagedChild_PreservesForeignLabels(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	desired := buildAGCServiceAccountV2(ag)
	live := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: desired.Name, Namespace: desired.Namespace, Labels: seededLabels(desired.Labels),
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
	r := applyTestReconciler(t, c, scheme)

	require.NoError(t, r.applyServiceAccount(context.Background(), ag, buildAGCServiceAccountV2(ag)))

	var got corev1.ServiceAccount
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(desired), &got))
	assertLabelsMerged(t, desired.Labels, got.Labels)
}

// TestApplyManagedSecret_PreservesForeignLabels is the Secret path's twin: it
// updates rather than patches, so it needs its own coverage.
func TestApplyManagedSecret_PreservesForeignLabels(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	desired := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"k": []byte("v")},
		}
	}
	live := desired()
	live.Labels = seededLabels(live.Labels)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
	r := applyTestReconciler(t, c, scheme)

	require.NoError(t, r.applyOwnedSecret(context.Background(), ag, desired()))

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(live), &got))
	assertLabelsMerged(t, v2GatewayLabels(ag), got.Labels)
}

// TestApplyProjections_PreserveForeignLabels covers the cross-namespace projection
// writers, which read and update directly rather than through applyManagedChild.
func TestApplyProjections_PreserveForeignLabels(t *testing.T) {
	want := map[string]string{labelShareManaged: "true", labelShareProxyNS: "proxy-ns", labelShareProxyNam: "pool"}
	objMeta := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: "share", Namespace: "consumer-ns", Labels: maps.Clone(want)}
	}
	tests := []struct {
		name    string
		live    client.Object
		apply   func(*EgressProxyReconciler) error
		fetched client.Object
	}{
		{
			name: "share ConfigMap",
			live: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "share", Namespace: "consumer-ns", Labels: seededLabels(want)}},
			apply: func(r *EgressProxyReconciler) error {
				return r.applyProxyShare(context.Background(), &corev1.ConfigMap{ObjectMeta: objMeta(), Data: map[string]string{"k": "v"}})
			},
			fetched: &corev1.ConfigMap{},
		},
		{
			name: "projected Secret",
			live: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "share", Namespace: "consumer-ns", Labels: seededLabels(want)}},
			apply: func(r *EgressProxyReconciler) error {
				return r.applyProjectedSecret(context.Background(), &corev1.Secret{ObjectMeta: objMeta(), Data: map[string][]byte{"k": []byte("v")}})
			},
			fetched: &corev1.Secret{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := applyTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.live).Build()

			require.NoError(t, tt.apply(&EgressProxyReconciler{Client: c}))

			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(tt.live), tt.fetched))
			assertLabelsMerged(t, want, tt.fetched.GetLabels())
		})
	}
}
