//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

func TestGMC_TenantTeardown_RemovesOnlyOwnedResources(t *testing.T) {
	const nsA = "team-teardown-a"
	const nsB = "team-teardown-b"

	createNamespace(t, nsA)
	createNamespace(t, nsB)
	createGitHubAppSecret(t, nsA, "github-app")
	createGitHubAppSecret(t, nsB, "github-app")

	agA := newActionsGateway("gateway-a", nsA, "github-app")
	agB := newActionsGateway("gateway-b", nsB, "github-app")

	require.NoError(t, k8sClient.Create(ctx, agA))
	require.NoError(t, k8sClient.Create(ctx, agB))

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), agA)
		_ = k8sClient.Delete(context.Background(), agB)
	})

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for both gateways to be provisioned.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: proxyName},
			&appsv1.Deployment{})
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsB, Name: proxyName},
			&appsv1.Deployment{})
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Delete team-a gateway.
	require.NoError(t, k8sClient.Delete(ctx, agA))

	// Assert resources are removed from team-a.
	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: proxyName},
			&appsv1.Deployment{})
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "proxy Deployment in team-a should be deleted")

	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: agcName},
			&appsv1.Deployment{})
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "agc Deployment in team-a should be deleted")

	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: agcName},
			&corev1.ServiceAccount{})
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "AGC ServiceAccount in team-a should be deleted")

	g.Eventually(func() bool {
		proxyGone := apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: nsA, Name: proxyName}, &networkingv1.NetworkPolicy{}))
		workloadGone := apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: nsA, Name: workloadName}, &networkingv1.NetworkPolicy{}))
		return proxyGone && workloadGone
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "NetworkPolicies in team-a should be deleted")

	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsA, Name: agcName},
			&rbacv1.Role{})
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "Role in team-a should be deleted")

	// Assert team-b resources are untouched.
	var depB appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsB, Name: proxyName}, &depB),
		"proxy Deployment in team-b must still exist after team-a deletion")

	var saB corev1.ServiceAccount
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsB, Name: agcName}, &saB),
		"AGC ServiceAccount in team-b must still exist")

	var npB networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsB, Name: proxyName}, &npB),
		"proxy NetworkPolicy in team-b must still exist after team-a deletion")
}

func TestGMC_TenantTeardown_ReapplyAfterDelete(t *testing.T) {
	const nsName = "team-reapply"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("reapply-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for initial reconcile.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&appsv1.Deployment{})
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Succeed(), "proxy Deployment should be created")

	// Delete the CR and wait for full teardown.
	require.NoError(t, k8sClient.Delete(ctx, ag))
	g.Eventually(func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "reapply-gateway"}, &fetched)
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "CR should be gone after teardown")

	// Wait for owned resources to be cleaned up.
	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&appsv1.Deployment{})
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "proxy Deployment should be deleted")

	// Re-apply the same CR.
	ag2 := newActionsGateway("reapply-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag2))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag2) })

	// Assert all resources are re-created cleanly.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&appsv1.Deployment{})
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Succeed(), "proxy Deployment should be re-created after re-apply")

	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName},
			&appsv1.Deployment{})
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Succeed(), "AGC Deployment should be re-created after re-apply")

	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&networkingv1.NetworkPolicy{})
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Succeed(), "proxy NetworkPolicy should be re-created after re-apply")
}

func TestGMC_Finalizer_BlocksImmediateDeletion(t *testing.T) {
	const nsName = "team-finalizer"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("finalizer-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for the finalizer to be added.
	g.Eventually(func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "finalizer-gateway"}, &fetched); err != nil {
			return false
		}
		for _, f := range fetched.Finalizers {
			if f == "actions-gateway.github.com/gmc-cleanup" {
				return true
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "finalizer should be added")

	// Delete — object won't disappear until reconciler removes finalizer.
	require.NoError(t, k8sClient.Delete(ctx, ag))

	// Eventually the gateway CR itself is gone (finalizer removed by reconciler).
	g.Eventually(func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "finalizer-gateway"}, &fetched)
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "ActionsGateway CR should be gone after teardown")
}
