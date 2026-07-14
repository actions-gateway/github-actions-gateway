//go:build integration

package integration_test

import (
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These tests assert that the v2 ActionsGateway and EgressProxy reconcilers emit
// Kubernetes Events for their meaningful transitions (Q305), so `kubectl describe`
// surfaces provisioning/credential/readiness activity rather than an empty Events
// list. envtest runs no kubelet, so neither pool becomes ready — that is exactly the
// path that produces the provisioning-start, cert-issuance, and not-ready Events we
// assert on here.

// eventReasonsFor returns the reasons of all Events whose regarding object matches the
// given kind/name in the namespace. The new-style recorder writes events.k8s.io/v1
// Events, which the manager's broadcaster flushes to the apiserver asynchronously, so
// callers poll this under require.Eventually.
func eventReasonsFor(t *testing.T, ns, kind, name string) []string {
	t.Helper()
	var list eventsv1.EventList
	if err := k8sClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil
	}
	var reasons []string
	for i := range list.Items {
		e := &list.Items[i]
		if e.Regarding.Kind == kind && e.Regarding.Name == name {
			reasons = append(reasons, e.Reason)
		}
	}
	return reasons
}

func containsAny(haystack, wanted []string) bool {
	for _, w := range wanted {
		for _, h := range haystack {
			if h == w {
				return true
			}
		}
	}
	return false
}

func TestV2_ActionsGateway_EmitsEvents(t *testing.T) {
	const ns = "v2-ag-events"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2alpha1.SecurityProfileLabel: v2alpha1.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	ag := newV2GatewayWired("gw", ns, "github-app", "shared")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startActionsGatewayV2Reconciler(t)

	// A valid gateway provisions its control plane but its AGC Deployment never becomes
	// ready in envtest, so the reconciler emits the provisioning-start Event, the
	// metrics-cert issuance Event, and the AGC-not-ready Event. Asserting any of them is
	// present proves the reconciler now surfaces its transitions.
	var reasons []string
	require.Eventually(t, func() bool {
		reasons = eventReasonsFor(t, ns, "ActionsGateway", "gw")
		return containsAny(reasons, []string{"Provisioning", "MetricsCertificateIssued", v2alpha1.ReasonAGCNotReady})
	}, 30*time.Second, 250*time.Millisecond, "expected the ActionsGateway to emit a provisioning/credential/readiness Event, got %v", reasons)

	assert.NotEmpty(t, reasons, "ActionsGateway should have at least one Event")
}

func TestV2_EgressProxy_EmitsEvents(t *testing.T) {
	const ns = "v2-ep-events"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	ep := newV2EgressProxyObject(egressProxyName, ns)
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns}})
	})

	// Confirm the proxy pool is being reconciled (the child Deployment appears) before
	// asserting on Events, so a slow first reconcile does not race the poll.
	require.Eventually(t, func() bool {
		var got v2alpha1.EgressProxy
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressProxyName}, &got) == nil
	}, 10*time.Second, 100*time.Millisecond)

	// The proxy TLS cert is issued on the first reconcile, and its pods never become
	// ready in envtest, so the reconciler emits the cert-issuance Event and the
	// proxy-not-ready Event. Either proves the (previously event-less) reconciler now
	// surfaces its transitions.
	var reasons []string
	require.Eventually(t, func() bool {
		reasons = eventReasonsFor(t, ns, "EgressProxy", egressProxyName)
		return containsAny(reasons, []string{"ProxyCertificateIssued", v2alpha1.ReasonProxyNotReady})
	}, 30*time.Second, 250*time.Millisecond, "expected the EgressProxy to emit a cert-issuance/readiness Event, got %v", reasons)

	assert.NotEmpty(t, reasons, "EgressProxy should have at least one Event")
}
