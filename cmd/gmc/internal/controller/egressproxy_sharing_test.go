package controller

import (
	"context"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// shareTestScheme registers the groups the sharing path reads and writes.
func shareTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, gmcv2alpha1.AddToScheme(s))
	return s
}

func proxyWithSharing(namespace, name string, allowed ...string) *gmcv2alpha1.EgressProxy {
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
	if allowed != nil {
		ep.Spec.Sharing = &gmcv2alpha1.ProxySharing{AllowedNamespaces: allowed}
	}
	return ep
}

// The secure default is the whole point of the field: anything short of an explicit
// listing must deny, because a namespace must not gain access it did not previously
// have by leaving a field unset.
func TestProxyShareGranted_DeniesWithoutExplicitConsent(t *testing.T) {
	tests := []struct {
		name       string
		proxy      *gmcv2alpha1.EgressProxy
		consumerNS string
		want       bool
	}{
		{"nil sharing denies", proxyWithSharing("provider", "pool"), "team-a", false},
		{"empty allowedNamespaces denies", proxyWithSharing("provider", "pool", []string{}...), "team-a", false},
		{"unlisted namespace denies", proxyWithSharing("provider", "pool", "team-b"), "team-a", false},
		{"empty consumer namespace denies", proxyWithSharing("provider", "pool", "team-a", ""), "", false},
		{"listed namespace grants", proxyWithSharing("provider", "pool", "team-a"), "team-a", true},
		{"one of several grants", proxyWithSharing("provider", "pool", "team-b", "team-a"), "team-a", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyShareGranted(tc.proxy, tc.consumerNS); got != tc.want {
				t.Errorf("proxyShareGranted(%q) = %v, want %v", tc.consumerNS, got, tc.want)
			}
		})
	}
}

// A prefix match would grant "team-a-staging" from a listing of "team-a".
func TestProxyShareGranted_MatchesWholeNamespaceOnly(t *testing.T) {
	ep := proxyWithSharing("provider", "pool", "team-a")
	for _, ns := range []string{"team-a-staging", "team", "TEAM-A"} {
		if proxyShareGranted(ep, ns) {
			t.Errorf("namespace %q was granted by a listing of only %q", ns, "team-a")
		}
	}
}

// The projection name has to distinguish providers, or a consumer holding grants
// from same-named proxies in two namespaces gets one CA silently overwriting the
// other — and then trusts the wrong proxy.
func TestProxyShareConfigMapName_DistinguishesProviderNamespace(t *testing.T) {
	a := proxyShareConfigMapName("platform-a", "shared")
	b := proxyShareConfigMapName("platform-b", "shared")
	if a == b {
		t.Fatalf("same name %q derived for two provider namespaces", a)
	}
	if len(a) > 63 {
		t.Errorf("derived name %q exceeds the 63-char label budget (%d)", a, len(a))
	}
}

// The GMC and AGC derive this name independently in two modules; if they disagree,
// the consumer looks for a ConfigMap that is never written and every cross-namespace
// reference fails closed with no explanation. Pinning the exact string is what makes
// a one-sided edit fail here rather than in a cluster.
func TestProxyShareConfigMapName_IsStable(t *testing.T) {
	if got, want := proxyShareConfigMapName("platform", "shared"), "proxy-share-platform-shared"; got != want {
		t.Errorf("proxyShareConfigMapName = %q, want %q", got, want)
	}
}

// Two selectors in ONE peer AND; two peers OR. Getting this wrong admits every pod
// in the granted namespace and every workload pod in every namespace, which reads
// identically in a diff and voids the grant entirely.
func TestEgressProxyNetworkPolicy_GrantedIngressPeerANDsBothSelectors(t *testing.T) {
	ep := proxyWithSharing("provider", "pool", "team-a")
	np := buildEgressProxyNetworkPolicy(ep, nil)

	// Index rather than a pointer: staticcheck's SA5011 does not treat t.Fatal as
	// terminal in every environment, so a *rule guarded by a nil check reads to it
	// as a possible nil dereference below. Selecting by index keeps the guard
	// explicit and leaves no pointer for that check to reason about.
	grantedIdx := -1
	for i := range np.Spec.Ingress {
		for _, peer := range np.Spec.Ingress[i].From {
			if peer.NamespaceSelector != nil &&
				peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] == "team-a" {
				grantedIdx = i
			}
		}
	}
	if grantedIdx < 0 {
		t.Fatal("no ingress rule admits the granted namespace team-a")
	}
	granted := np.Spec.Ingress[grantedIdx]
	if len(granted.From) != 1 {
		t.Fatalf("granted rule has %d peers; both selectors must sit in ONE peer or they OR", len(granted.From))
	}
	if granted.From[0].PodSelector == nil {
		t.Error("granted peer has no PodSelector, so it admits every pod in the namespace")
	} else if got := granted.From[0].PodSelector.MatchLabels[labelComponent]; got != componentWorkload {
		t.Errorf("granted peer PodSelector matches %q, want %q", got, componentWorkload)
	}
}

// Absent sharing must leave the policy byte-identical to the pre-M4 shape.
func TestEgressProxyNetworkPolicy_NoSharingAddsNoIngress(t *testing.T) {
	base := buildEgressProxyNetworkPolicy(proxyWithSharing("provider", "pool"), nil)
	empty := buildEgressProxyNetworkPolicy(proxyWithSharing("provider", "pool", []string{}...), nil)
	if len(base.Spec.Ingress) != len(empty.Spec.Ingress) {
		t.Fatalf("empty allowedNamespaces changed the ingress rule count: %d vs %d",
			len(base.Spec.Ingress), len(empty.Spec.Ingress))
	}
	// Only the grant selects a namespace by name; the metrics-scrape rule's own
	// namespace selector is pre-existing and matches on a monitoring label instead.
	for _, rule := range base.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil {
				continue
			}
			if ns, ok := peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName]; ok {
				t.Errorf("an unshared proxy admits namespace %q by name", ns)
			}
		}
	}
}

// The consumer's egress peer names the granted pool, not "any proxy in that
// namespace": one provider namespace can hold two pools granting different consumers,
// and a namespace-wide peer would reach the one that granted nothing.
func TestRemoteProxyEgressRules_SelectTheGrantedPoolOnly(t *testing.T) {
	rules := remoteProxyEgressRules([]remoteProxy{{Namespace: "platform", Name: "pool-a"}})
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if len(rules[0].To) != 1 {
		t.Fatalf("rule has %d peers; both selectors must sit in ONE peer or they OR", len(rules[0].To))
	}
	peer := rules[0].To[0]
	if peer.NamespaceSelector == nil ||
		peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != "platform" {
		t.Errorf("peer does not pin the provider namespace: %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil {
		t.Fatal("peer has no PodSelector, so it reaches every pod in the provider namespace")
	}
	if got := peer.PodSelector.MatchLabels[egressProxyComponentLabel]; got != "pool-a" {
		t.Errorf("peer selects %q, want the granted pool %q", got, "pool-a")
	}
}

// Nothing granted means no extra egress at all: an unshared gateway keeps exactly the
// same-namespace policy it had before M4.
func TestRemoteProxyEgressRules_NoneWhenNothingGranted(t *testing.T) {
	if got := remoteProxyEgressRules(nil); len(got) != 0 {
		t.Errorf("emitted %d egress rule(s) with no granted proxies", len(got))
	}
}

// Revoking a grant must delete the projection, because the consumer treats its
// presence as the grant. A prune keyed on the *current* spec would never reach a
// namespace the spec no longer names.
func TestReconcileProxyShares_RevokedGrantIsPruned(t *testing.T) {
	ctx := context.Background()
	ep := proxyWithSharing("provider", "pool", "team-a")

	c := fake.NewClientBuilder().
		WithScheme(shareTestScheme(t)).
		WithObjects(
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "provider", Name: egressProxyTLSSecretName(ep)},
				Data:       map[string][]byte{corev1.TLSCertKey: []byte("-----BEGIN CERTIFICATE-----")},
			},
			&gmcv2alpha1.ActionsGateway{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gw"},
				Spec: gmcv2alpha1.ActionsGatewaySpec{
					DefaultProxyRef: &gmcv2alpha1.ProxyObjectRef{Name: "pool", Namespace: "provider"},
				},
			},
		).Build()
	r := &EgressProxyReconciler{Client: c}

	if err := r.reconcileProxyShares(ctx, ep); err != nil {
		t.Fatalf("project: %v", err)
	}
	key := types.NamespacedName{Namespace: "team-a", Name: proxyShareConfigMapName("provider", "pool")}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, key, &cm); err != nil {
		t.Fatalf("granted namespace holds no projection: %v", err)
	}
	if cm.Data[shareHostKey] != "pool-proxy.provider.svc.cluster.local" {
		t.Errorf("projection carries host %q", cm.Data[shareHostKey])
	}

	// Withdraw consent and reconcile again.
	ep.Spec.Sharing.AllowedNamespaces = nil
	if err := r.reconcileProxyShares(ctx, ep); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := c.Get(ctx, key, &cm); !apierrors.IsNotFound(err) {
		t.Errorf("projection survived the revoked grant (err=%v)", err)
	}
}

// A referrer alone must not pull the CA into its namespace: consent is provider-side,
// and projecting on the consumer's say-so would be the bypass the handshake exists to
// prevent.
func TestReconcileProxyShares_UnconsentedReferrerGetsNothing(t *testing.T) {
	ctx := context.Background()
	ep := proxyWithSharing("provider", "pool") // no sharing at all

	c := fake.NewClientBuilder().
		WithScheme(shareTestScheme(t)).
		WithObjects(&gmcv2alpha1.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gw"},
			Spec: gmcv2alpha1.ActionsGatewaySpec{
				DefaultProxyRef: &gmcv2alpha1.ProxyObjectRef{Name: "pool", Namespace: "provider"},
			},
		}).Build()
	r := &EgressProxyReconciler{Client: c}

	if err := r.reconcileProxyShares(ctx, ep); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var list corev1.ConfigMapList
	if err := c.List(ctx, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("projected %d ConfigMap(s) for an unconsented referrer", len(list.Items))
	}
}
