//go:build integration

package integration_test

import (
	"context"
	"net"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestV2_EgressProxy_IPRangeRefresh pins the IP-range reconciler's EgressProxy pass:
// a managed pool's NetworkPolicy takes a rotated GitHub range from the refresh
// itself, read back as ReconcileNow returns and so before any reconcile could carry
// it, and a managedNetworkPolicy: false pool carries no GitHub range before or after
// and is never patched. TestV2_DirectEgress_IPRangeRefreshUpdatesDirectPolicies is
// the direct-egress half.
func TestV2_EgressProxy_IPRangeRefresh(t *testing.T) {
	const managedNS, unmanagedNS = "v2-ep-iprange", "v2-ep-iprange-unmanaged"
	createNamespace(t, managedNS)
	createNamespace(t, unmanagedNS)

	_, cidr1, _ := net.ParseCIDR("140.82.112.0/20")
	_, cidr2, _ := net.ParseCIDR("1.2.3.0/24")
	ipCache := &controller.IPRangeCache{}
	ipCache.Set([]net.IPNet{*cidr1})
	ipCache.MarkRefreshed(time.Now())

	managed := newV2EgressProxyObject(egressProxyName, managedNS)
	unmanaged := newV2EgressProxyObject(egressProxyName, unmanagedNS)
	off := false
	unmanaged.Spec.ManagedNetworkPolicy = &off
	for _, ep := range []*v2.EgressProxy{managed, unmanaged} {
		require.NoError(t, k8sClient.Create(ctx, ep))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })
	}
	startEgressProxyReconciler(t, ipCache)

	proxyNP := func(ns string) (*networkingv1.NetworkPolicy, error) {
		var np networkingv1.NetworkPolicy
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &np)
		return &np, err
	}
	require.Eventually(t, func() bool {
		m, merr := proxyNP(managedNS)
		_, uerr := proxyNP(unmanagedNS)
		return merr == nil && uerr == nil && npHasGitHubCIDR(m, cidr1.String())
	}, 20*time.Second, 100*time.Millisecond, "both pools' NetworkPolicies, the managed one carrying the first range")
	np, err := proxyNP(unmanagedNS)
	require.NoError(t, err)
	assert.False(t, npHasGitHubCIDR(np, cidr1.String()), "managedNetworkPolicy: false must carry no GitHub range")

	metrics := &controller.Metrics{IPRangeUpdates: prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "q1259_ip_range_updates_total"}, []string{"namespace"})}
	(&controller.IPRangeReconciler{
		Client:    k8sClient,
		Fetcher:   &stubIPFetcher{cidrs: []net.IPNet{*cidr1, *cidr2}},
		Cache:     ipCache,
		V2Enabled: true,
		Metrics:   metrics,
	}).ReconcileNow(ctx)

	np, err = proxyNP(managedNS)
	require.NoError(t, err)
	assert.True(t, npHasGitHubCIDR(np, cidr2.String()) && npHasGitHubCIDR(np, cidr1.String()),
		"the refresh must patch the managed pool's NetworkPolicy with the rotated range")
	np, err = proxyNP(unmanagedNS)
	require.NoError(t, err)
	assert.False(t, npHasGitHubCIDR(np, cidr1.String()) || npHasGitHubCIDR(np, cidr2.String()),
		"the refresh must not add a GitHub range to an unmanaged pool")
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.IPRangeUpdates.WithLabelValues(managedNS)), "the managed pool is patched once")
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.IPRangeUpdates.WithLabelValues(unmanagedNS)), "the unmanaged pool is skipped")
}
