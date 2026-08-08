package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Q582: a migration's coexistence window runs a v1 inline proxy pool and a v2
// EgressProxy pool in one namespace. v1 has exactly one pool per namespace and keys
// its PodDisruptionBudget selector, its Deployment selector (which is what the HPA
// controller reads off the scale subresource), and its required hostname
// anti-affinity term on the bare `app: actions-gateway-proxy` label. A v2 pod wearing
// that label is therefore claimed by all three, which is what put each pool's pods
// under the other's PDB, wedged both HPAs on AmbiguousSelector, and made the two
// pools repel each other off every node.
//
// These assert the property that keeps that from happening: over the two pools' real
// pod-template label sets, neither pool's selectors match the other's pods.

// selects reports whether sel matches the label set l.
func selects(t *testing.T, sel *metav1.LabelSelector, l map[string]string) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(sel)
	require.NoError(t, err)
	return s.Matches(labels.Set(l))
}

// coexistingPoolPodLabels builds the pod-template label sets of a v1 inline pool and
// a v2 EgressProxy pool sharing one namespace, from the builders themselves — a
// hand-written literal would stop tracking the thing under test.
func coexistingPoolPodLabels(t *testing.T) (v1Pod, v2Pod map[string]string) {
	t.Helper()
	v1Pod = buildProxyDeployment(newTestAG("gateway", "team-a"), "proxy:test").Spec.Template.Labels
	v2Pod = buildEgressProxyDeployment(newEP("gateway-egress", "team-a", nil), "proxy:test", nil).Spec.Template.Labels
	return v1Pod, v2Pod
}

func TestCoexistence_PoolSelectorsDoNotCrossMatch(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ep := newEP("gateway-egress", "team-a", nil)
	v1Pod, v2Pod := coexistingPoolPodLabels(t)

	v1Dep := buildProxyDeployment(ag, "proxy:test")
	v2Dep := buildEgressProxyDeployment(ep, "proxy:test", nil)

	// Deployment selectors. Disjointness here is the precondition the HPA controller
	// checks: it lists the pods matching its scale target's selector and refuses to
	// act — ScalingActive=False/AmbiguousSelector — when any of them is also
	// controlled by another HPA. One pool's selector reaching the other's pods is
	// exactly that condition, and it wedges BOTH pools' autoscaling, not one.
	assert.True(t, selects(t, v1Dep.Spec.Selector, v1Pod), "the v1 selector must still own the v1 pool")
	assert.True(t, selects(t, v2Dep.Spec.Selector, v2Pod), "the v2 selector must still own the v2 pool")
	assert.False(t, selects(t, v1Dep.Spec.Selector, v2Pod), "the v1 Deployment selector must not reach v2 pool pods")
	assert.False(t, selects(t, v2Dep.Spec.Selector, v1Pod), "the v2 Deployment selector must not reach v1 pool pods")

	// PodDisruptionBudgets. A pod covered by two PDBs is one the eviction API refuses
	// to evict at all, so a cross-match does not merely mis-budget a drain — it blocks
	// it.
	v1PDB := buildPDB(ag)
	v2PDB := buildEgressProxyPDB(ep)
	assert.True(t, selects(t, v1PDB.Spec.Selector, v1Pod))
	assert.True(t, selects(t, v2PDB.Spec.Selector, v2Pod))
	assert.False(t, selects(t, v1PDB.Spec.Selector, v2Pod), "the v1 PDB must not cover v2 pool pods")
	assert.False(t, selects(t, v2PDB.Spec.Selector, v1Pod), "the v2 PDB must not cover v1 pool pods")

	// Hostname anti-affinity. The scheduler enforces a RUNNING pod's term against an
	// incoming one, so a cross-matching term costs the namespace one node per replica
	// of BOTH pools — and strands the pool that starts second.
	v1Term := requiredAntiAffinityTerm(t, v1Dep.Spec.Template.Spec)
	v2Term := requiredAntiAffinityTerm(t, v2Dep.Spec.Template.Spec)
	assert.True(t, selects(t, v1Term.LabelSelector, v1Pod), "the v1 pool must still spread across nodes")
	assert.True(t, selects(t, v2Term.LabelSelector, v2Pod), "the v2 pool must still spread across nodes")
	assert.False(t, selects(t, v1Term.LabelSelector, v2Pod), "v1 replicas must not repel v2 pool pods")
	assert.False(t, selects(t, v2Term.LabelSelector, v1Pod), "v2 replicas must not repel v1 pool pods")
}

// TestCoexistence_WorkloadNetworkPolicyReachesTheV2Pool pins the egress half of the
// same change. Narrowing the v2 pool's labels without moving the v2 workload policy's
// proxy peer off the v1 label would leave every v2 AGC and worker pod unable to reach
// its own proxy — a silent, total egress break rather than a scheduling annoyance.
func TestCoexistence_WorkloadNetworkPolicyReachesTheV2Pool(t *testing.T) {
	v1Pod, v2Pod := coexistingPoolPodLabels(t)

	np := buildWorkloadNetworkPolicyV2(v2Gateway("gateway", "team-a", "github-app", "gateway-egress"), nil, false, nil)
	peer := proxyPortEgressPeer(t, np.Spec.Egress)

	assert.True(t, selects(t, peer.PodSelector, v2Pod), "v2 workload pods must still reach their own proxy pool")
	assert.False(t, selects(t, peer.PodSelector, v1Pod), "v2 workload pods have no business reaching a coexisting v1 pool")

	// A second pool in the same namespace is reachable too: a RunnerSet may name its
	// own proxyRef, so the peer cannot key on one pool's name.
	other := buildEgressProxyDeployment(newEP("other", "team-a", nil), "proxy:test", nil).Spec.Template.Labels
	assert.True(t, selects(t, peer.PodSelector, other), "every EgressProxy pool in the namespace must be reachable")
}

// requiredAntiAffinityTerm returns the pod spec's single required pod-anti-affinity
// term, failing the test if the built-in cross-node spread is missing.
func requiredAntiAffinityTerm(t *testing.T, spec corev1.PodSpec) corev1.PodAffinityTerm {
	t.Helper()
	require.NotNil(t, spec.Affinity)
	require.NotNil(t, spec.Affinity.PodAntiAffinity)
	terms := spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	require.Len(t, terms, 1)
	require.Equal(t, "kubernetes.io/hostname", terms[0].TopologyKey)
	return terms[0]
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
