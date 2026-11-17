//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestV2_EgressProxy_PolicyPrecedesDeployment has the apiserver refuse the proxy
// Deployment, through a ResourceQuota admitting none, and asserts the proxy
// NetworkPolicy already exists: a first provision that stops at the Deployment
// leaves no proxy pod without a policy selecting it (Q1264).
func TestV2_EgressProxy_PolicyPrecedesDeployment(t *testing.T) {
	const ns = "v2-ep-policy-order"
	createNamespace(t, ns)

	deployments := corev1.ResourceName("count/deployments.apps")
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "no-deployments", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{deployments: resource.MustParse("0")}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	// envtest runs no quota controller, so the usage it would record is set by hand.
	quota.Status = corev1.ResourceQuotaStatus{
		Hard: corev1.ResourceList{deployments: resource.MustParse("0")},
		Used: corev1.ResourceList{deployments: resource.MustParse("0")},
	}
	require.NoError(t, k8sClient.Status().Update(ctx, quota))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), quota) })

	startEgressProxyReconciler(t, nil)
	ep := &gmcv2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "ordered", Namespace: ns}}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })

	require.Eventually(t, func() bool {
		c := egressProxyCondition(t, ns, "ordered", gmcv2.ConditionDegraded)
		return c != nil && c.Status == metav1.ConditionTrue && strings.Contains(c.Message, "apply proxy Deployment")
	}, 20*time.Second, 100*time.Millisecond, "the quota must stop the reconcile at the Deployment")

	key := types.NamespacedName{Namespace: ns, Name: proxyChildName("ordered")}
	require.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{})), "the quota admitted the Deployment")
	require.NoError(t, k8sClient.Get(ctx, key, &networkingv1.NetworkPolicy{}),
		"the proxy NetworkPolicy must exist before the Deployment is applied")

	// Lifting the quota lets the retry provision the pool.
	require.NoError(t, k8sClient.Delete(ctx, quota))
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, key, &appsv1.Deployment{}) == nil
	}, 30*time.Second, 200*time.Millisecond, "the Deployment is applied once the quota is gone")
}
