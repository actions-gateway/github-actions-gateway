//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestV2_Teardown_FailClosedUntilChildrenGone proves the Q125 fail-closed teardown
// ported to the v2 ActionsGateway reconciler (Q328) against the real apiserver:
// deleting a gateway whose children still exist retains the cleanup finalizer and
// emits a TeardownIncomplete event; the finalizer is removed — and the CR finalizes
// away — only once every child is verifiably gone. A foreign finalizer on the AGC
// Deployment stands in for "a child lingers through its delete" (in envtest no
// garbage collector runs, so explicit teardown is also what removes the namespaced
// children at all).
func TestV2_Teardown_FailClosedUntilChildrenGone(t *testing.T) {
	const ns = "v2-teardown"
	const gwName = "gw"
	agcName := gwName + "-agc"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	ag := newV2GatewayWired(gwName, ns, "github-app", "shared")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	// Wait for the control plane to be provisioned.
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: agcName}, &dep) == nil
	}, 20*time.Second, 100*time.Millisecond, "AGC Deployment should be created")

	// Hold the AGC Deployment with a foreign finalizer so it survives its delete
	// (deletionTimestamp set, object lingering) — the scenario teardown must not
	// walk away from. Retried under Eventually: the reconciler may be patching the
	// Deployment concurrently.
	const hold = "test.actions-gateway.github.com/teardown-hold"
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: agcName}, &dep); err != nil {
			return false
		}
		dep.Finalizers = append(dep.Finalizers, hold)
		return k8sClient.Update(ctx, &dep) == nil
	}, 10*time.Second, 100*time.Millisecond, "hold finalizer should be added to the AGC Deployment")
	// Never leak the hold — release it even if an assertion below fails first.
	t.Cleanup(func() {
		var d appsv1.Deployment
		if k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: agcName}, &d) == nil {
			d.Finalizers = nil
			_ = k8sClient.Update(context.Background(), &d)
		}
	})

	require.NoError(t, k8sClient.Delete(ctx, ag))

	// While the held child lingers: TeardownIncomplete is emitted and the gateway's
	// cleanup finalizer stays in place (the CR must not finalize away).
	var reasons []string
	require.Eventually(t, func() bool {
		reasons = eventReasonsFor(t, ns, "ActionsGateway", gwName)
		return containsAny(reasons, []string{"TeardownIncomplete"})
	}, 30*time.Second, 250*time.Millisecond, "expected a TeardownIncomplete event while the AGC Deployment lingers, got %v", reasons)

	var fetched v2alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwName}, &fetched),
		"the gateway CR must still exist while a child lingers")
	require.NotNil(t, fetched.DeletionTimestamp, "the gateway should be mid-deletion")
	assert.Contains(t, fetched.Finalizers, v2alpha1.ActionsGatewayFinalizer,
		"the cleanup finalizer must be retained while a child lingers")

	// The unheld children are deleted explicitly (envtest runs no GC, so this is
	// the teardown's own work): the AGC ServiceAccount is gone while the held
	// Deployment remains.
	require.Eventually(t, func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: agcName}, &corev1.ServiceAccount{})
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 100*time.Millisecond, "the AGC ServiceAccount should be deleted by teardown")
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: agcName}, &dep),
		"the held AGC Deployment must still linger")

	// Release the hold: the child drains, teardown confirms every child gone, and
	// the finalizer is removed — the CR finalizes away.
	require.Eventually(t, func() bool {
		var d appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: agcName}, &d); err != nil {
			return apierrors.IsNotFound(err)
		}
		d.Finalizers = nil
		return k8sClient.Update(ctx, &d) == nil
	}, 10*time.Second, 100*time.Millisecond, "hold finalizer should be released")

	require.Eventually(t, func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwName}, &v2alpha1.ActionsGateway{})
		return apierrors.IsNotFound(err)
	}, 30*time.Second, 250*time.Millisecond, "the gateway must finalize away once every child is gone")

	// The cluster-scoped ClusterRoleBinding (no owner ref possible) is gone too.
	crbName := "agc-clusterrunnertemplate-reader." + ns + "." + gwName
	assert.Eventually(t, func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: crbName}, &rbacv1.ClusterRoleBinding{})
		return apierrors.IsNotFound(err)
	}, 10*time.Second, 100*time.Millisecond, "the per-gateway ClusterRoleBinding must be deleted")
}
