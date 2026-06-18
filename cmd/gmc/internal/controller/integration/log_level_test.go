//go:build integration

package integration_test

import (
	"testing"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
)

// envValue returns the value of the named env var on the first container of the
// Deployment, or "" if absent.
func envValue(dep *appsv1.Deployment, name string) string {
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// TestGMC_LogLevelChange_RollsAGCAndProxy verifies the per-tenant verbosity knob
// (Q89): spec.logLevel is threaded to both the AGC and proxy Deployments as the
// LOG_LEVEL env var, and flipping it from info to debug updates both pod
// templates — the rolling-restart path that makes a new level take effect. It
// mirrors the securityProfile threading: an env on the pod template, so a change
// rolls the Deployment rather than hot-reloading.
func TestGMC_LogLevelChange_RollsAGCAndProxy(t *testing.T) {
	const nsName = "team-loglevel-roll"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	// Default (no spec.logLevel) — must land on LOG_LEVEL=info on both workloads.
	ag := newActionsGateway("loglevel-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Both Deployments come up with LOG_LEVEL=info.
	g.Eventually(func() bool {
		var agc, proxy appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &agc); err != nil {
			return false
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &proxy); err != nil {
			return false
		}
		return envValue(&agc, "LOG_LEVEL") == "info" && envValue(&proxy, "LOG_LEVEL") == "info"
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"AGC and proxy Deployments must start with LOG_LEVEL=info by default")

	// Flip spec.logLevel to debug (retry on conflict — the reconciler may still
	// be writing the finalizer/status on first reconcile).
	require.Eventually(t, func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "loglevel-gateway"}, &fetched); err != nil {
			return false
		}
		fetched.Spec.LogLevel = "debug"
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "update ActionsGateway spec.logLevel to debug")

	// Both Deployments roll to LOG_LEVEL=debug — the change reaches the pod
	// template, which is what triggers the rolling restart.
	g.Eventually(func() bool {
		var agc, proxy appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &agc); err != nil {
			return false
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &proxy); err != nil {
			return false
		}
		return envValue(&agc, "LOG_LEVEL") == "debug" && envValue(&proxy, "LOG_LEVEL") == "debug"
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"AGC and proxy Deployments must roll to LOG_LEVEL=debug after spec.logLevel=debug")
}
