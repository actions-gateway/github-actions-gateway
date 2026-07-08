//go:build integration

package integration_test

import (
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These tests pin the Q283 contract for an HPA-targeted proxy pool Deployment: the
// reconciler owns the replica *floor*, the HPA owns everything above it.
//
// Before the fix both proxy reconcilers blanket-copied the builder's DeploymentSpec
// (whose Replicas is minReplicas) on every pass, so an HPA scale-out was reverted
// within milliseconds — the Owns(&appsv1.Deployment{}) watch turns the HPA's own
// write to the scale subresource into a requeue that undoes it. The pool could never
// stay scaled out.
//
// envtest runs no kube-controller-manager, so there is no HPA controller to drive the
// scale-out for us; the tests write the scale subresource directly, which is exactly
// what the HPA controller does. Only a real apiserver serves that subresource, and
// only a real apiserver defaults `.spec.replicas` on an existing Deployment — so this
// behavior is unobservable with a fake client, hence the envtest tier.

// scaleDeployment sets .spec.replicas through the Deployment's scale subresource,
// the same write the HPA controller issues.
func scaleDeployment(t *testing.T, ns, name string, replicas int32) {
	t.Helper()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	}
	require.NoError(t, k8sClient.SubResource("scale").Update(ctx, dep, client.WithSubResourceBody(scale)))
}

// deploymentReplicas reads the live .spec.replicas, failing the assertion while the
// Deployment does not exist yet.
func deploymentReplicas(g gomega.Gomega, ns, name string) int32 {
	var dep appsv1.Deployment
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep)).To(gomega.Succeed())
	g.Expect(dep.Spec.Replicas).NotTo(gomega.BeNil())
	return *dep.Spec.Replicas
}

// waitForDeploymentReplicas blocks until the Deployment exists with the given
// .spec.replicas.
func waitForDeploymentReplicas(t *testing.T, ns, name string, want int32) {
	t.Helper()
	gomega.NewWithT(t).Eventually(func(g gomega.Gomega) int32 {
		return deploymentReplicas(g, ns, name)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Equal(want))
}

// assertReplicasSurviveReconciles holds the assertion across a window several times
// the reconciler's 2s SyncPeriod, so a periodic resync — not just the Owns() requeue
// the scale write already triggered — has to leave the count alone.
func assertReplicasSurviveReconciles(t *testing.T, ns, name string, want int32) {
	t.Helper()
	gomega.NewWithT(t).Consistently(func(g gomega.Gomega) int32 {
		return deploymentReplicas(g, ns, name)
	}, 6*time.Second, 100*time.Millisecond).Should(gomega.Equal(want),
		"reconciler reverted an HPA scale-out on the proxy Deployment (Q283)")
}

func TestV2_EgressProxy_ReconcilerPreservesHPAScaleOut(t *testing.T) {
	const ns = "v2-ep-hpa-scaleout"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	minR := int32(2)
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{MinReplicas: &minR},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	name := proxyChildName(egressProxyName)

	// The floor still applies at create: the pool starts at minReplicas.
	waitForDeploymentReplicas(t, ns, name, minR)

	// Stand in for the HPA controller scaling the pool out under load.
	scaleDeployment(t, ns, name, 5)
	assertReplicasSurviveReconciles(t, ns, name, 5)

	// The HPA's own bounds are still reconciled from the CR — the reconciler gives up
	// the replica count, not the floor that constrains it. A scale-down below the
	// floor is the HPA's job to refuse, via this minReplicas.
	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &hpa))
	require.NotNil(t, hpa.Spec.MinReplicas)
	require.Equal(t, minR, *hpa.Spec.MinReplicas)
}

func TestV2_EgressProxy_ReconcilerRestoresFloorFromZero(t *testing.T) {
	const ns = "v2-ep-hpa-zero"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	minR := int32(3)
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{MinReplicas: &minR},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	name := proxyChildName(egressProxyName)
	waitForDeploymentReplicas(t, ns, name, minR)

	// An HPA will not act on a target sitting at zero replicas, so the reconciler must
	// restore the floor itself — otherwise the tenant's only egress path stays down.
	scaleDeployment(t, ns, name, 0)
	waitForDeploymentReplicas(t, ns, name, minR)
}

func TestGMC_V1Proxy_ReconcilerPreservesHPAScaleOut(t *testing.T) {
	const nsName = "team-hpa-scaleout"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("scaleout-gateway", nsName, "github-app")
	ag.Spec.Proxy.MinReplicas = ptr32(2)
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startGMCReconciler(t, nil)

	// The floor applies at create, then the HPA takes over above it.
	waitForDeploymentReplicas(t, nsName, proxyName, 2)
	scaleDeployment(t, nsName, proxyName, 4)
	assertReplicasSurviveReconciles(t, nsName, proxyName, 4)

	// The AGC Deployment is not HPA-targeted: the reconciler still owns its replica
	// count outright and pins it back to 1.
	scaleDeployment(t, nsName, agcName, 3)
	waitForDeploymentReplicas(t, nsName, agcName, 1)
}

// TestGMC_V1Proxy_HPABoundsAreReconciledFromTheCR pins the *other* half of the Q176
// root cause: the GMC owns the HPA's whole spec, so an out-of-band edit to the HPA's
// own spec.minReplicas is reverted to the ActionsGateway's spec.proxy.minReplicas.
//
// This is correct — the CR is the source of truth for the pool's bounds — but it is
// what made the E2E_GMC_HPADrivesScaleUp e2e flaky: that test raised the *HPA's*
// minReplicas and waited for status.desiredReplicas to follow. The HPA's resulting
// scale-out wrote the proxy Deployment, the Owns(&Deployment{}) watch turned that
// write into a reconcile, and the reconcile put minReplicas back. desiredReplicas
// was 2 only inside that window, so a longer timeout could never help. The e2e now
// drives the floor through the CR instead.
func TestGMC_V1Proxy_HPABoundsAreReconciledFromTheCR(t *testing.T) {
	const nsName = "team-hpa-bounds-revert"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("bounds-gateway", nsName, "github-app")
	ag.Spec.Proxy.MinReplicas = ptr32(1)
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)
	hpaKey := types.NamespacedName{Namespace: nsName, Name: proxyName}

	g.Eventually(func(g gomega.Gomega) int32 {
		var hpa autoscalingv2.HorizontalPodAutoscaler
		g.Expect(k8sClient.Get(ctx, hpaKey, &hpa)).To(gomega.Succeed())
		g.Expect(hpa.Spec.MinReplicas).NotTo(gomega.BeNil())
		return *hpa.Spec.MinReplicas
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Equal(int32(1)))

	// Edit the HPA out of band, the way the old e2e did.
	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, k8sClient.Get(ctx, hpaKey, &hpa))
	hpa.Spec.MinReplicas = ptr32(2)
	require.NoError(t, k8sClient.Update(ctx, &hpa))

	// The next reconcile — which in a real cluster the HPA's own scale-out would
	// trigger via the Deployment watch — reverts it to the CR's value.
	g.Eventually(func(g gomega.Gomega) int32 {
		var got autoscalingv2.HorizontalPodAutoscaler
		g.Expect(k8sClient.Get(ctx, hpaKey, &got)).To(gomega.Succeed())
		g.Expect(got.Spec.MinReplicas).NotTo(gomega.BeNil())
		return *got.Spec.MinReplicas
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Equal(int32(1)),
		"GMC must reconcile the HPA's bounds back to the ActionsGateway spec")

	// Raising the floor on the CR is the supported path, and it sticks.
	require.Eventually(t, func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "bounds-gateway"}, &fetched); err != nil {
			return false
		}
		fetched.Spec.Proxy.MinReplicas = ptr32(2)
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "raise proxy.minReplicas on the CR")

	g.Eventually(func(g gomega.Gomega) int32 {
		var got autoscalingv2.HorizontalPodAutoscaler
		g.Expect(k8sClient.Get(ctx, hpaKey, &got)).To(gomega.Succeed())
		g.Expect(got.Spec.MinReplicas).NotTo(gomega.BeNil())
		return *got.Spec.MinReplicas
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Equal(int32(2)))
}
