package controller

import (
	"context"
	"testing"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The apply* helpers collapsed onto the shared applyManagedChild path (Q366). The
// owner-reference decision is load-bearing for garbage collection: an un-owned
// child is reclaimed only by the reconcileDelete finalizer, so if that finalizer
// is force-removed the un-owned children leak. Q394 settled the policy uniformly
// across both reconcilers — every namespaced child is owner-referenced, and the
// only un-owned child is one a namespaced ActionsGateway cannot legally own (the
// cluster-scoped ClusterRoleBinding). These tables pin that contract per helper so
// a dropped owner shows up as a failure rather than a silent GC regression.

type ownerRefCase struct {
	name        string
	apply       func() error
	into        client.Object
	key         types.NamespacedName
	expectOwner bool
}

func assertOwnerRefContract(t *testing.T, c client.Client, cases []ownerRefCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, tc.apply())
			require.NoError(t, c.Get(context.Background(), tc.key, tc.into))
			if tc.expectOwner {
				require.Len(t, tc.into.GetOwnerReferences(), 1, "%s must stamp a controller owner reference", tc.name)
				assert.True(t, *tc.into.GetOwnerReferences()[0].Controller)
			} else {
				assert.Empty(t, tc.into.GetOwnerReferences(),
					"%s must stay un-owned: a namespaced ActionsGateway cannot own a cluster-scoped child, so reconcileDelete reclaims it (Q394)", tc.name)
			}
		})
	}
}

// TestV2ApplyHelpers_OwnerReferenceContract pins the v2 ActionsGateway reconciler's
// policy: every namespaced child is owner-referenced; the sole un-owned child is
// the cluster-scoped ClusterRoleBinding, which a namespaced ActionsGateway cannot
// own.
func TestV2ApplyHelpers_OwnerReferenceContract(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}
	ag := v2Gateway("tenant", "tenant-ns", "gh-creds", "")
	ctx := context.Background()

	ownedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "owned-tls", Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("x")},
	}
	saDesired := buildAGCServiceAccountV2(ag)
	rbDesired := buildAGCRoleBindingV2(ag)
	svcDesired := buildAGCServiceV2(ag)
	npDesired := buildAGCNetworkPolicyV2(ag, nil, nil, false, nil)
	depDesired := buildAGCDeploymentV2(ag, "agc:test", nil, gmcv2.SecurityProfileBaseline, nil)
	crbName := clusterRunnerTemplateReaderBindingName(ag)

	cases := []ownerRefCase{
		{"applyServiceAccount", func() error { return r.applyServiceAccount(ctx, ag, saDesired) },
			&corev1.ServiceAccount{}, types.NamespacedName{Namespace: ag.Namespace, Name: saDesired.Name}, true},
		{"applyRoleBinding", func() error { return r.applyRoleBinding(ctx, ag, rbDesired) },
			&rbacv1.RoleBinding{}, types.NamespacedName{Namespace: ag.Namespace, Name: rbDesired.Name}, true},
		{"applyService", func() error { return r.applyService(ctx, ag, svcDesired) },
			&corev1.Service{}, types.NamespacedName{Namespace: ag.Namespace, Name: svcDesired.Name}, true},
		{"applyNetworkPolicy", func() error { return r.applyNetworkPolicy(ctx, ag, npDesired) },
			&networkingv1.NetworkPolicy{}, types.NamespacedName{Namespace: ag.Namespace, Name: npDesired.Name}, true},
		{"applyDeployment", func() error { return r.applyDeployment(ctx, ag, depDesired) },
			&appsv1.Deployment{}, types.NamespacedName{Namespace: ag.Namespace, Name: depDesired.Name}, true},
		{"applyOwnedSecret", func() error { return r.applyOwnedSecret(ctx, ag, ownedSecret) },
			&corev1.Secret{}, types.NamespacedName{Namespace: ag.Namespace, Name: "owned-tls"}, true},
		{"applyClusterRunnerTemplateReaderBinding", func() error { return r.applyClusterRunnerTemplateReaderBinding(ctx, ag) },
			&rbacv1.ClusterRoleBinding{}, types.NamespacedName{Name: crbName}, false},
	}

	assertOwnerRefContract(t, c, cases)
}
