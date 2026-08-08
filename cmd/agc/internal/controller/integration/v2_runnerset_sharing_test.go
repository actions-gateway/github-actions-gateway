//go:build integration

package integration_test

import (
	"context"
	"testing"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The consumer half of cross-namespace EgressProxy sharing (Q166, M4, §H.9).
//
// The AGC never evaluates allowedNamespaces and never reads the remote EgressProxy:
// its cache and Role are scoped to its own namespace, so it cannot. The GMC decides
// consent and projects a ConfigMap into the consumer namespace, and the AGC treats
// that projection's presence as the grant. These tests drive the AGC against the
// projection directly, standing in for the GMC, which is what lets them assert the
// AGC's own fail-closed behaviour rather than the GMC's.

const sharedProxyNS = "v2-rs-share-provider"

// projectedShareName mirrors the name both modules derive independently. Spelled out
// here rather than imported so a one-sided change to either derivation fails.
func projectedShareName(proxyNS, proxyName string) string {
	return "proxy-share-" + proxyNS + "-" + proxyName
}

// newProjectedShare builds what the GMC writes into a granted consumer namespace.
func newProjectedShare(consumerNS, proxyNS, proxyName string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projectedShareName(proxyNS, proxyName),
			Namespace: consumerNS,
			Labels:    map[string]string{"actions-gateway/proxy-share": "true"},
		},
		Data: map[string]string{
			"ca.crt":     "-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n",
			"proxy-host": proxyName + "-proxy." + proxyNS + ".svc.cluster.local",
			"proxy-port": "8080",
			"no-proxy":   "",
		},
	}
}

// A cross-namespace proxyRef with no projection must fail closed. This is the
// consumer-side expression of the security property: the AGC has no way to reach the
// remote proxy's spec, so absent a grant it must refuse rather than assume.
func TestV2_RunnerSet_UngrantedCrossNamespaceProxy_FailsClosed(t *testing.T) {
	const ns = "v2-rs-share-denied"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("set", ns, "gw")
	rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "shared", Namespace: sharedProxyNS}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })

	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonProxyShareNotGranted)
}

// With the projection in place the same reference resolves, and withdrawing it puts
// the set back to fail-closed. The revocation leg is the one that matters: a grant
// that cannot be taken back is not a grant.
func TestV2_RunnerSet_GrantedCrossNamespaceProxy_ResolvesAndRevokes(t *testing.T) {
	const ns = "v2-rs-share-granted"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))

	share := newProjectedShare(ns, sharedProxyNS, "shared")
	require.NoError(t, k8sClient.Create(ctx, share))

	rs := newRunnerSet("set", ns, "gw")
	rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "shared", Namespace: sharedProxyNS}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })

	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// Revoke: the GMC deletes the projection when the grant is withdrawn.
	require.NoError(t, k8sClient.Delete(ctx, share))
	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonProxyShareNotGranted)
}
