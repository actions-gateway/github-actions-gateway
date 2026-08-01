//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestV2_GMCRestart_LeavesTheAGCDeploymentUntouched pins the property Q587 asked
// about: a GMC process replaced by another one holding the same configuration
// must not re-render a tenant's AGC Deployment, because a changed pod template
// means a new pod-template-hash, a new ReplicaSet, and a rolled tenant control
// plane.
//
// Only this tier can observe it. The builder is a pure function of its inputs, so
// a unit test comparing two renders (TestBuildAGCDeploymentV2_RenderIsDeterministic)
// says nothing about the apply path: apply* replaces the whole Spec with a
// builder object that omits every server-defaulted field, so the round trip
// through the apiserver's defaulting is part of the property. envtest runs no
// kube-controller-manager, so no ReplicaSet exists to count — but the
// pod-template-hash is a pure function of the template, so an unchanged template
// is exactly the assertion that no roll could occur.
func TestV2_GMCRestart_LeavesTheAGCDeploymentUntouched(t *testing.T) {
	const (
		ns    = "v2-gmc-restart"
		gw    = "gw"
		gwAGC = "gw-agc"
	)
	createNamespaceWithLabels(t, ns, map[string]string{
		v2alpha1.SecurityProfileLabel: v2alpha1.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	ag := newV2GatewayWired(gw, ns, "github-app", "shared")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	// Both processes get the same AGC_EXTRA_* passthrough. It is the one render
	// input assembled from the GMC's own environment rather than from an API
	// object, so a restart is where an ordering difference in it would surface.
	extraEnv := []corev1.EnvVar{
		{Name: "GITHUB_API_BASE_URL", Value: "http://fakegithub.e2e-infra.svc.cluster.local:8080"},
		{Name: "GITHUB_BROKER_URL", Value: "http://fakegithub.e2e-infra.svc.cluster.local:8080"},
		{Name: "WRAPPER_IMAGE", Value: "wrapper:test"},
	}

	firstCtx, stopFirst := context.WithCancel(ctx)
	t.Cleanup(stopFirst)
	firstStopped := startActionsGatewayV2ReconcilerIn(t, firstCtx, extraEnv)

	depKey := types.NamespacedName{Namespace: ns, Name: gwAGC}
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, depKey, &dep) == nil
	}, 15*time.Second, 100*time.Millisecond, "AGC Deployment should be created")

	// Let the first process settle (finalizer add, metrics-cert issue, first status
	// write) so the baseline is a steady state and not a mid-provision snapshot.
	time.Sleep(4 * time.Second)
	require.NoError(t, k8sClient.Get(ctx, depKey, &dep))
	baselineTemplate := *dep.Spec.Template.DeepCopy()
	baselineRV := dep.ResourceVersion

	stopFirst()
	<-firstStopped

	secondCtx, stopSecond := context.WithCancel(ctx)
	t.Cleanup(stopSecond)
	startActionsGatewayV2ReconcilerIn(t, secondCtx, extraEnv)

	// Positive control. "The Deployment did not change" is worth nothing until the
	// replacement process is proven to have reconciled this gateway: delete a child
	// it recreates, and wait for it back. The AGC Service is applied in the same
	// reconcileResources pass as the Deployment, two steps ahead of it.
	require.NoError(t, k8sClient.Delete(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: gwAGC, Namespace: ns},
	}))
	require.Eventually(t, func() bool {
		var svc corev1.Service
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwAGC}, &svc) == nil
	}, 30*time.Second, 100*time.Millisecond, "the replacement GMC must reconcile the gateway, or this test measures nothing")

	// The reconciler runs with SyncPeriod=2s, so this window spans several full
	// periodic reconciles by the new process.
	g := gomega.NewWithT(t)
	g.Consistently(func(g gomega.Gomega) {
		var got appsv1.Deployment
		g.Expect(k8sClient.Get(ctx, depKey, &got)).To(gomega.Succeed())
		g.Expect(got.ResourceVersion).To(gomega.Equal(baselineRV))
	}, 6*time.Second, 500*time.Millisecond).Should(gomega.Succeed(),
		"a GMC restart must not write the AGC Deployment at all")

	require.NoError(t, k8sClient.Get(ctx, depKey, &dep))
	assert.Equal(t, baselineTemplate, dep.Spec.Template,
		"a GMC restart re-rendered the AGC pod template; the tenant's control plane would roll")
}
