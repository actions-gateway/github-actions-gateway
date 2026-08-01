//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
)

// These tests pin the Q552 contract: `kubectl rollout restart` on a GMC-managed
// Deployment must actually restart it.
//
// kubectl implements the restart by patching a
// `kubectl.kubernetes.io/restartedAt` annotation onto the pod template. No GMC
// builder sets that key, so the reconciler's template replace reverted it on the
// next pass — often before the Deployment controller had computed a new template
// hash — and the restart was a silent no-op. kubectl still printed "successfully
// rolled out", because the *old* ReplicaSet was trivially complete.
//
// The unit tier pins the assignment helpers directly. This tier is what shows the
// operator-visible behavior: a running reconciler, its Owns(&Deployment{}) watch
// firing on the annotation write, and a periodic resync on top.

const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// rolloutRestart patches the pod-template annotation `kubectl rollout restart`
// writes, and returns the value it stamped.
func rolloutRestart(t *testing.T, ns, name string) string {
	t.Helper()
	const at = "2026-07-31T12:00:00Z"

	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: ns, Name: name}
	require.NoError(t, k8sClient.Get(ctx, key, &dep))
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[restartedAtAnnotation] = at
	require.NoError(t, k8sClient.Update(ctx, &dep))
	return at
}

// assertRestartedAtSurvives holds the assertion across a window several times the
// reconcilers' 2s SyncPeriod, so a periodic resync — not just the Owns() requeue
// the annotation write already triggered — has to leave the annotation alone.
func assertRestartedAtSurvives(t *testing.T, ns, name, want string) {
	t.Helper()
	gomega.NewWithT(t).Consistently(func(g gomega.Gomega) string {
		var dep appsv1.Deployment
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep)).To(gomega.Succeed())
		return dep.Spec.Template.Annotations[restartedAtAnnotation]
	}, 6*time.Second, 100*time.Millisecond).Should(gomega.Equal(want),
		"GMC reverted a kubectl rollout restart of a managed Deployment (Q552)")
}

func waitForDeployment(t *testing.T, ns, name string) {
	t.Helper()
	gomega.NewWithT(t).Eventually(func() error {
		var dep appsv1.Deployment
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep)
	}, 20*time.Second, 25*time.Millisecond).Should(gomega.Succeed())
}

func TestGMC_V1AGC_RolloutRestartSurvivesReconcile(t *testing.T) {
	const nsName = "team-agc-restart"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("restart-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startGMCReconciler(t, nil)
	waitForDeployment(t, nsName, agcName)

	at := rolloutRestart(t, nsName, agcName)
	assertRestartedAtSurvives(t, nsName, agcName, at)

	// The reconciler still owns the rest of the template: the annotation is
	// tolerated, not a licence for arbitrary drift.
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &dep))
	require.NotEmpty(t, dep.Spec.Template.Spec.Containers)
	require.Equal(t, "agc:test", dep.Spec.Template.Spec.Containers[0].Image)
}

func TestV2_AGC_RolloutRestartSurvivesReconcile(t *testing.T) {
	const nsName = "v2-agc-restart"
	const gw = "restart-gw"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newV2GatewayWired(gw, nsName, "github-app", "")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startActionsGatewayV2Reconciler(t)
	waitForDeployment(t, nsName, gw+"-agc")

	at := rolloutRestart(t, nsName, gw+"-agc")
	assertRestartedAtSurvives(t, nsName, gw+"-agc", at)
}

// TestGMC_V1AGC_UnrelatedTemplateDriftIsReverted pins the negative half: only the
// listed restart annotation is tolerated. Any other hand-edited pod-template
// annotation is still reconciled away.
func TestGMC_V1AGC_UnrelatedTemplateDriftIsReverted(t *testing.T) {
	const nsName = "team-agc-drift"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("drift-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startGMCReconciler(t, nil)
	waitForDeployment(t, nsName, agcName)

	key := types.NamespacedName{Namespace: nsName, Name: agcName}
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, key, &dep))
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["sidecar.istio.io/inject"] = "false"
	require.NoError(t, k8sClient.Update(ctx, &dep))

	gomega.NewWithT(t).Eventually(func(g gomega.Gomega) map[string]string {
		var got appsv1.Deployment
		g.Expect(k8sClient.Get(ctx, key, &got)).To(gomega.Succeed())
		return got.Spec.Template.Annotations
	}, 20*time.Second, 25*time.Millisecond).ShouldNot(gomega.HaveKey("sidecar.istio.io/inject"),
		"an unlisted hand-edited pod-template annotation must still be reverted")
}
