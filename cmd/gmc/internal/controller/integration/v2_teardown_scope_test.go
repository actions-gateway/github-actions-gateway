//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// v2Child is one object reconcileDelete deletes by name.
type v2Child struct {
	label string
	key   types.NamespacedName
	obj   func() client.Object
}

// v2TeardownChildren lists what a direct-egress gateway's teardown deletes. The
// metrics Secrets are left to owner-reference garbage collection, which envtest does
// not run, so they are not listed.
func v2TeardownChildren(ns, gw string) []v2Child {
	nn := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }
	return []v2Child{
		{"AGC Deployment", nn(gw + "-agc"), func() client.Object { return &appsv1.Deployment{} }},
		{"AGC Service", nn(gw + "-agc"), func() client.Object { return &corev1.Service{} }},
		{"AGC NetworkPolicy", nn(gw + "-agc"), func() client.Object { return &networkingv1.NetworkPolicy{} }},
		{"workload NetworkPolicy", nn(gw + "-workload"), func() client.Object { return &networkingv1.NetworkPolicy{} }},
		{"AGC RoleBinding", nn(gw + "-agc"), func() client.Object { return &rbacv1.RoleBinding{} }},
		{"AGC ServiceAccount", nn(gw + "-agc"), func() client.Object { return &corev1.ServiceAccount{} }},
		{"worker ServiceAccount", nn(gw + "-worker"), func() client.Object { return &corev1.ServiceAccount{} }},
		{"ClusterRoleBinding", types.NamespacedName{Name: "agc-clusterrunnertemplate-reader." + ns + "." + gw}, func() client.Object { return &rbacv1.ClusterRoleBinding{} }},
	}
}

// requireV2Children waits for every teardown-owned child of gw in ns to exist.
func requireV2Children(t *testing.T, ns, gw string) {
	t.Helper()
	for _, c := range v2TeardownChildren(ns, gw) {
		require.Eventually(t, func() bool { return k8sClient.Get(ctx, c.key, c.obj()) == nil },
			20*time.Second, 100*time.Millisecond, "%s of %s/%s should be created", c.label, ns, gw)
	}
}

// deleteV2GatewayAndWait deletes gw in ns and waits for it to finalize away, which
// teardown allows only once it has confirmed every child gone.
func deleteV2GatewayAndWait(t *testing.T, ns, gw string) {
	t.Helper()
	require.NoError(t, k8sClient.Delete(ctx, &v2.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: gw}}))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gw}, &v2.ActionsGateway{}))
	}, 30*time.Second, 250*time.Millisecond, "%s/%s should finalize away", ns, gw)
}

// TestV2_Teardown_RemovesOnlyItsOwnChildren deletes one of three gateways, a
// neighbour in the same namespace and a same-named one in another, and asserts
// teardown removed that gateway's children and none of theirs.
// TestV2_Teardown_FailClosedUntilChildrenGone covers a child that lingers.
func TestV2_Teardown_RemovesOnlyItsOwnChildren(t *testing.T) {
	const nsA, nsB = "v2-teardown-own-a", "v2-teardown-own-b"
	gateways := []struct{ ns, name string }{{nsA, "gw"}, {nsA, "gw2"}, {nsB, "gw"}}
	for _, ns := range []string{nsA, nsB} {
		createNamespace(t, ns)
		createGitHubAppSecret(t, ns, "github-app")
	}
	for _, g := range gateways {
		ag := newV2GatewayWired(g.name, g.ns, "github-app", "")
		require.NoError(t, k8sClient.Create(ctx, ag))
		t.Cleanup(func() {
			_ = k8sClient.Delete(context.Background(), ag)
			_ = k8sClient.Delete(context.Background(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agc-clusterrunnertemplate-reader." + g.ns + "." + g.name}})
		})
	}
	startActionsGatewayV2Reconciler(t)
	for _, g := range gateways {
		requireV2Children(t, g.ns, g.name)
	}
	// A neighbour's reconcile recreates a child teardown deleted by mistake, so the
	// survivors are compared by UID rather than by existence.
	uids := map[string]types.UID{}
	for _, g := range gateways[1:] {
		for _, c := range v2TeardownChildren(g.ns, g.name) {
			obj := c.obj()
			require.NoError(t, k8sClient.Get(ctx, c.key, obj))
			uids[g.ns+"/"+g.name+"/"+c.label] = obj.GetUID()
		}
	}

	deleteV2GatewayAndWait(t, nsA, "gw")

	for _, c := range v2TeardownChildren(nsA, "gw") {
		err := k8sClient.Get(ctx, c.key, c.obj())
		assert.True(t, apierrors.IsNotFound(err), "%s of the deleted gateway should be gone; got: %v", c.label, err)
	}
	for _, g := range gateways[1:] {
		for _, c := range v2TeardownChildren(g.ns, g.name) {
			obj := c.obj()
			if assert.NoError(t, k8sClient.Get(ctx, c.key, obj), "%s of %s/%s must survive", c.label, g.ns, g.name) {
				assert.Equal(t, uids[g.ns+"/"+g.name+"/"+c.label], obj.GetUID(), "%s of %s/%s must be the original, not a recreation", c.label, g.ns, g.name)
				assert.Nil(t, obj.GetDeletionTimestamp(), "%s of %s/%s must not be deleted", c.label, g.ns, g.name)
			}
		}
	}
}

// TestV2_Teardown_ReapplyAfterDeleteProvisionsAgain recreates a gateway under its
// old name once teardown has finished and asserts it provisions from scratch: fresh
// children controlled by the new object, and no Degraded condition. Nothing from
// the first incarnation may survive into the second.
func TestV2_Teardown_ReapplyAfterDeleteProvisionsAgain(t *testing.T) {
	const ns = "v2-teardown-reapply"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	createDirectV2Gateway(t, ns, "github-app", nil)
	startActionsGatewayV2Reconciler(t)
	requireV2Children(t, ns, "gw")
	first, ok := agcDeployment(ns)
	require.True(t, ok)

	deleteV2GatewayAndWait(t, ns, "gw")

	createDirectV2Gateway(t, ns, "github-app", nil)
	var second v2.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &second))
	requireV2Children(t, ns, "gw")
	require.Eventually(t, func() bool {
		dep, ok := agcDeployment(ns)
		owner := metav1.GetControllerOf(&dep)
		return ok && dep.UID != first.UID && owner != nil && owner.UID == second.UID
	}, 20*time.Second, 100*time.Millisecond, "the AGC Deployment should be a fresh one controlled by the reapplied gateway")
	require.Eventually(t, func() bool {
		var ag v2.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &ag); err != nil {
			return false
		}
		c := meta.FindStatusCondition(ag.Status.Conditions, v2.ConditionDegraded)
		return c != nil && c.Status == metav1.ConditionFalse
	}, 20*time.Second, 100*time.Millisecond, "the reapplied gateway should not be Degraded")
}
