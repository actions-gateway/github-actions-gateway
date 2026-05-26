//go:build integration

package integration_test

import (
	"testing"
	"time"

	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func ptr32(v int32) *int32 { return &v }

func TestGMC_HPABoundsUpdate(t *testing.T) {
	const nsName = "team-hpa-update"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("hpa-gateway", nsName, "github-app")
	ag.Spec.Proxy.MinReplicas = ptr32(2)
	ag.Spec.Proxy.MaxReplicas = ptr32(8)
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ag)
	})

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for HPA to be created with initial values.
	g.Eventually(func() error {
		var hpa autoscalingv2.HorizontalPodAutoscaler
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &hpa); err != nil {
			return err
		}
		return nil
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &hpa))
	require.NotNil(t, hpa.Spec.MinReplicas)
	require.Equal(t, int32(2), *hpa.Spec.MinReplicas)
	require.Equal(t, int32(8), hpa.Spec.MaxReplicas)

	// Update the ActionsGateway with new HPA bounds (retry on conflict — the
	// reconciler may still be writing the finalizer/status on first reconcile).
	require.Eventually(t, func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "hpa-gateway"}, &fetched); err != nil {
			return false
		}
		fetched.Spec.Proxy.MinReplicas = ptr32(3)
		fetched.Spec.Proxy.MaxReplicas = ptr32(15)
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "update ActionsGateway HPA bounds")

	// Wait for HPA to reflect the updated values.
	g.Eventually(func() bool {
		var updatedHPA autoscalingv2.HorizontalPodAutoscaler
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &updatedHPA); err != nil {
			return false
		}
		if updatedHPA.Spec.MinReplicas == nil {
			return false
		}
		return *updatedHPA.Spec.MinReplicas == 3 && updatedHPA.Spec.MaxReplicas == 15
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "HPA should reflect updated min=3, max=15")
}

func TestGMC_HPABoundsUpdate_MinReplicasClamped(t *testing.T) {
	const nsName = "team-hpa-clamped"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	// Start with minReplicas=5, maxReplicas=5 (equal boundary).
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "hpa-clamped-gateway", Namespace: nsName},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "github-app"},
			Proxy: gmcv1alpha1.ProxyConfig{
				MinReplicas: ptr32(5),
				MaxReplicas: ptr32(5),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ag)
	})

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	g.Eventually(func() bool {
		var hpa autoscalingv2.HorizontalPodAutoscaler
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &hpa); err != nil {
			return false
		}
		if hpa.Spec.MinReplicas == nil {
			return false
		}
		return *hpa.Spec.MinReplicas == 5 && hpa.Spec.MaxReplicas == 5
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "HPA should have min=5, max=5")
}
