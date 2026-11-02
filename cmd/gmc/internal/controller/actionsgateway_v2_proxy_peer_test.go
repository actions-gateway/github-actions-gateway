package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// selects reports whether sel matches the label set l.
func selects(t *testing.T, sel *metav1.LabelSelector, l map[string]string) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(sel)
	require.NoError(t, err)
	return s.Matches(labels.Set(l))
}

// TestWorkloadNetworkPolicyReachesEveryPool pins the workload policy's proxy peer: it
// must select the gateway's own pool and any other EgressProxy pool in the namespace,
// since a RunnerSet may name its own proxyRef and the peer cannot key on one pool's
// name. A peer that misses them is a silent, total egress break.
func TestWorkloadNetworkPolicyReachesEveryPool(t *testing.T) {
	np := buildWorkloadNetworkPolicyV2(v2Gateway("gateway", "team-a", "github-app", "gateway-egress"), nil, false, nil)
	peer := proxyPortEgressPeer(t, np.Spec.Egress)

	for _, pool := range []string{"gateway-egress", "other"} {
		pod := buildEgressProxyDeployment(newEP(pool, "team-a", nil), "proxy:test", nil).Spec.Template.Labels
		assert.True(t, selects(t, peer.PodSelector, pod), "workload pods must reach EgressProxy %q", pool)
	}
}

// proxyPortEgressPeer returns the single peer of the egress rule that opens the proxy
// port, failing the test if the rule or its peer is absent.
func proxyPortEgressPeer(t *testing.T, rules []networkingv1.NetworkPolicyEgressRule) networkingv1.NetworkPolicyPeer {
	t.Helper()
	for _, r := range rules {
		for _, p := range r.Ports {
			if p.Port != nil && p.Port.IntVal == proxyPort {
				require.Len(t, r.To, 1, "the proxy egress rule must have exactly one peer")
				return r.To[0]
			}
		}
	}
	t.Fatalf("no egress rule on the proxy port %d", proxyPort)
	return networkingv1.NetworkPolicyPeer{}
}
