package controller

import (
	"context"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestApplyService_PreservesClusterIP is the key behavioural guarantee of the
// CreateOrPatch migration: patching the Service updates the managed Spec fields
// (selector/ports/type) while leaving the server-assigned ClusterIP intact. A
// whole-object replace would have wiped it.
func TestApplyService_PreservesClusterIP(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := v2Gateway("tenant", "tenant-ns", "gh-creds", "")
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
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.applyService(context.Background(), ag, buildAGCServiceV2(ag)))

	var svc corev1.Service
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(desired), &svc))
	assert.Equal(t, "10.0.0.42", svc.Spec.ClusterIP, "server-assigned ClusterIP must be preserved")
	assert.Equal(t, desired.Spec.Ports, svc.Spec.Ports, "managed ports must be updated")
	assert.Equal(t, desired.Spec.Selector, svc.Spec.Selector)
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
	scheme := actionsGatewayV2TestScheme(t)
	ag := v2Gateway("tenant", "tenant-ns", "gh-creds", "")
	desired := buildAGCServiceAccountV2(ag)
	live := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: desired.Name, Namespace: desired.Namespace, Labels: seededLabels(desired.Labels),
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.applyServiceAccount(context.Background(), ag, buildAGCServiceAccountV2(ag)))

	var got corev1.ServiceAccount
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(desired), &got))
	assertLabelsMerged(t, desired.Labels, got.Labels)
}

// TestApplyManagedSecret_PreservesForeignLabels is the Secret path's twin: it
// updates rather than patches, so it needs its own coverage.
func TestApplyManagedSecret_PreservesForeignLabels(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := v2Gateway("tenant", "tenant-ns", "gh-creds", "")
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
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

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
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(tt.live).Build()

			require.NoError(t, tt.apply(&EgressProxyReconciler{Client: c}))

			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(tt.live), tt.fetched))
			assertLabelsMerged(t, want, tt.fetched.GetLabels())
		})
	}
}
