package controller

import (
	"context"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestV2ApplyRoleBinding_RecreatesOnRoleRefChange mirrors
// TestApplyRoleBinding_RecreatesOnRoleRefChange for the v2 reconciler: an
// existing binding with a different (immutable) roleRef must be deleted and
// recreated rather than patched, while a steady-state re-apply is a no-op.
func TestV2ApplyRoleBinding_RecreatesOnRoleRefChange(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := v2Gateway("tenant", "tenant-ns", "gh-app-creds", "")

	legacy := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ag.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "legacy-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agcNameV2(ag), Namespace: ag.Namespace}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.applyRoleBinding(context.Background(), ag, buildAGCRoleBindingV2(ag)))

	var rb rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcNameV2(ag)}, &rb))
	assert.Equal(t, "ClusterRole", rb.RoleRef.Kind)
	assert.Equal(t, agcTenantRoleName, rb.RoleRef.Name)
	require.Len(t, rb.OwnerReferences, 1)
	assert.Equal(t, ag.Name, rb.OwnerReferences[0].Name)

	// Steady-state re-apply must not error and must leave the roleRef untouched.
	require.NoError(t, r.applyRoleBinding(context.Background(), ag, buildAGCRoleBindingV2(ag)))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcNameV2(ag)}, &rb))
	assert.Equal(t, agcTenantRoleName, rb.RoleRef.Name)
}

// TestV2ApplyClusterRunnerTemplateReaderBinding_CreateThenPatch verifies the
// cluster-scoped binding is created with the expected roleRef/subject (and no
// owner reference, since a cluster-scoped object cannot be owned by a namespaced
// ActionsGateway), and that a roleRef change triggers delete+recreate.
func TestV2ApplyClusterRunnerTemplateReaderBinding_CreateThenPatch(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := v2Gateway("tenant", "tenant-ns", "gh-app-creds", "")
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.applyClusterRunnerTemplateReaderBinding(context.Background(), ag))

	var crb rbacv1.ClusterRoleBinding
	name := clusterRunnerTemplateReaderBindingName(ag)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, &crb))
	assert.Equal(t, clusterRunnerTemplateReaderRole, crb.RoleRef.Name)
	assert.Empty(t, crb.OwnerReferences, "a cluster-scoped binding cannot carry an owner ref to a namespaced ActionsGateway")
	require.Len(t, crb.Subjects, 1)
	assert.Equal(t, agcNameV2(ag), crb.Subjects[0].Name)

	// Steady-state re-apply is a no-op patch and must not error.
	require.NoError(t, r.applyClusterRunnerTemplateReaderBinding(context.Background(), ag))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, &crb))
	assert.Equal(t, clusterRunnerTemplateReaderRole, crb.RoleRef.Name)
}

// TestV2ApplyClusterRunnerTemplateReaderBinding_RecreatesOnRoleRefChange
// verifies that an existing cluster-scoped binding with a stale (immutable)
// roleRef is deleted and recreated with the current roleRef, mirroring the
// namespaced RoleBinding recreate behaviour.
func TestV2ApplyClusterRunnerTemplateReaderBinding_RecreatesOnRoleRefChange(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := v2Gateway("tenant", "tenant-ns", "gh-app-creds", "")
	name := clusterRunnerTemplateReaderBindingName(ag)

	legacy := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "legacy-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agcNameV2(ag), Namespace: ag.Namespace}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.applyClusterRunnerTemplateReaderBinding(context.Background(), ag))

	var crb rbacv1.ClusterRoleBinding
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, &crb))
	assert.Equal(t, clusterRunnerTemplateReaderRole, crb.RoleRef.Name, "the stale roleRef must be replaced via delete+recreate")
}

// TestSecretToActionsGateways_MatchesGitHubAppSecretInNamespace verifies the v2
// Secret mapper enqueues only ActionsGateways in the Secret's namespace whose
// credentials.githubApp names it.
func TestSecretToActionsGateways_MatchesGitHubAppSecretInNamespace(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	matching := v2Gateway("tenant-a", "ns1", "gh-app-creds", "")
	other := v2Gateway("tenant-b", "ns1", "other-creds", "")
	otherNS := v2Gateway("tenant-c", "ns2", "gh-app-creds", "")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(matching, other, otherNS).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	secret := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "gh-app-creds", Namespace: "ns1"}}
	reqs := r.secretToActionsGateways(context.Background(), secret)

	require.Len(t, reqs, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "ns1", Name: "tenant-a"}, reqs[0].NamespacedName)
}

// TestProxyToActionsGateways_MatchesDefaultProxyRef verifies the v2 EgressProxy
// mapper enqueues only ActionsGateways whose defaultProxyRef names the proxy.
func TestProxyToActionsGateways_MatchesDefaultProxyRef(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	matching := v2Gateway("tenant-a", "ns1", "gh-app-creds", "shared-proxy")
	noProxy := v2Gateway("tenant-b", "ns1", "gh-app-creds", "")
	otherProxy := v2Gateway("tenant-c", "ns1", "gh-app-creds", "other-proxy")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(matching, noProxy, otherProxy).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	proxy := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "shared-proxy", Namespace: "ns1"}}
	reqs := r.proxyToActionsGateways(context.Background(), proxy)

	require.Len(t, reqs, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "ns1", Name: "tenant-a"}, reqs[0].NamespacedName)
}

// TestGatewaysMatching_ListErrorReturnsNil verifies a List failure (e.g. an
// unlisted namespace with no informer/cache entry never errors here, but a
// genuinely broken client) degrades to nil requests rather than panicking.
func TestGatewaysMatching_ListErrorReturnsNil(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	reqs := r.gatewaysMatching(context.Background(), "empty-ns", func(*gmcv2alpha1.ActionsGateway) bool { return true })
	assert.Empty(t, reqs)
}
