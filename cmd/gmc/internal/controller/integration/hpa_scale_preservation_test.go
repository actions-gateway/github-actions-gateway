//go:build integration

package integration_test

import (
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
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
