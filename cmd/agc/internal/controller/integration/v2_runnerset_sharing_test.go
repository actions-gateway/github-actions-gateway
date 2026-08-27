//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
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

// shareRecheck pulls the production one-minute share re-check (§H.9) down to
// something a test can wait out. It is the RunnerSet's only event source for a
// projected share in either direction of the grant — the AGC may get that ConfigMap
// but not watch it — so a test that revokes or grants one has to set it.
const shareRecheck = 200 * time.Millisecond

func withShareRecheck(r *controller.RunnerSetReconciler) {
	r.ProxyShareRecheckInterval = shareRecheck
}

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
//
// The revoke wait is satisfied by the share re-check or by an incidental listener
// wake, and cannot tell them apart — which is why it flaked before the re-check
// existed (Q999). GrantArrivingLaterResolves below is the leg that proves the poll,
// having no incidental wake available to it.
func TestV2_RunnerSet_GrantedCrossNamespaceProxy_ResolvesAndRevokes(t *testing.T) {
	const ns = "v2-rs-share-granted"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t, withShareRecheck)

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

// A grant that arrives after the reference does must resolve it. This is the same
// missing-referent → referent-appears contract §H.7 gives every other reference, and
// the projection is the one referent the AGC cannot watch: its Role grants get on
// ConfigMaps and not list/watch, so no informer fires when the GMC writes it.
//
// The share re-check is the only thing that can satisfy this wait. A set sitting
// ProxyShareNotGranted has had both listeners stopped, so nothing pushes the wake
// channel, it owns no worker pods, and its own spec does not change — remove the
// requeue and this goes red at the full Eventually budget while
// FailsClosedUntilRefsResolve, which does the same thing with watched referents,
// still passes in under a second.
func TestV2_RunnerSet_CrossNamespaceProxy_GrantArrivingLaterResolves(t *testing.T) {
	const ns = "v2-rs-share-late"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t, withShareRecheck)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("set", ns, "gw")
	rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "shared", Namespace: sharedProxyNS}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })

	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonProxyShareNotGranted)

	// The GMC projects the share once the provider consents.
	require.NoError(t, k8sClient.Create(ctx, newProjectedShare(ns, sharedProxyNS, "shared")))
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}
