package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// shareTestScheme registers the groups the sharing path reads and writes.
func shareTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, gmcv2.AddToScheme(s))
	return s
}

func proxyWithSharing(namespace, name string, allowed ...string) *gmcv2.EgressProxy {
	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
	if allowed != nil {
		ep.Spec.Sharing = &gmcv2.ProxySharing{AllowedNamespaces: allowed}
	}
	return ep
}

// The secure default is the whole point of the field: anything short of an explicit
// listing must deny, because a namespace must not gain access it did not previously
// have by leaving a field unset.
func TestProxyShareGranted_DeniesWithoutExplicitConsent(t *testing.T) {
	tests := []struct {
		name       string
		proxy      *gmcv2.EgressProxy
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
			issuedProxyTLSSecret(t, ep),
			&gmcv2.ActionsGateway{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gw"},
				Spec: gmcv2.ActionsGatewaySpec{
					DefaultProxyRef: &gmcv2.ProxyObjectRef{Name: "pool", Namespace: "provider"},
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

// gmc-tenant-resource-guard denies a projection into a namespace that is not a
// marked tenant. That denial is reported, and must not stop the same reconcile
// withdrawing a share another namespace has lost.
func TestReconcileProxyShares_DeniedProjectionDoesNotBlockRevocation(t *testing.T) {
	ctx := context.Background()
	ep := proxyWithSharing("provider", "pool", "team-a", "not-a-tenant")
	referrer := func(ns string) *gmcv2.ActionsGateway {
		return &gmcv2.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gw"},
			Spec: gmcv2.ActionsGatewaySpec{
				DefaultProxyRef: &gmcv2.ProxyObjectRef{Name: "pool", Namespace: "provider"},
			},
		}
	}

	c := fake.NewClientBuilder().
		WithScheme(shareTestScheme(t)).
		WithObjects(
			issuedProxyTLSSecret(t, ep),
			referrer("team-a"),
			referrer("not-a-tenant"),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && obj.GetNamespace() == "not-a-tenant" {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), nil)
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &EgressProxyReconciler{Client: c}
	key := types.NamespacedName{Namespace: "team-a", Name: proxyShareConfigMapName("provider", "pool")}

	err := r.reconcileProxyShares(ctx, ep)
	require.ErrorContains(t, err, `"not-a-tenant"`, "the denied projection is reported")
	require.NoError(t, c.Get(ctx, key, &corev1.ConfigMap{}), "the admitted namespace is still projected")

	ep.Spec.Sharing.AllowedNamespaces = []string{"not-a-tenant"}
	err = r.reconcileProxyShares(ctx, ep)
	require.ErrorContains(t, err, `"not-a-tenant"`)
	require.True(t, apierrors.IsNotFound(c.Get(ctx, key, &corev1.ConfigMap{})),
		"the revoked share is withdrawn although another projection was denied")
}

// A referrer alone must not pull the CA into its namespace: consent is provider-side,
// and projecting on the consumer's say-so would be the bypass the handshake exists to
// prevent.
func TestReconcileProxyShares_UnconsentedReferrerGetsNothing(t *testing.T) {
	ctx := context.Background()
	ep := proxyWithSharing("provider", "pool") // no sharing at all

	c := fake.NewClientBuilder().
		WithScheme(shareTestScheme(t)).
		WithObjects(&gmcv2.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gw"},
			Spec: gmcv2.ActionsGatewaySpec{
				DefaultProxyRef: &gmcv2.ProxyObjectRef{Name: "pool", Namespace: "provider"},
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

// A consumer's AGC cannot read a shared pool's EgressProxy, so the projection is the
// only way its per-set EgressAuditUnattributed learns the source half (Q1069). The key
// must follow the provider's spec, or a pool turned on later reads Off to every
// consumer forever.
func TestReconcileProxyShares_ProjectsAuditLogging(t *testing.T) {
	ctx := context.Background()
	ep := proxyWithSharing("provider", "pool", "team-a")

	c := fake.NewClientBuilder().
		WithScheme(shareTestScheme(t)).
		WithObjects(
			issuedProxyTLSSecret(t, ep),
			&gmcv2.ActionsGateway{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gw"},
				Spec: gmcv2.ActionsGatewaySpec{
					DefaultProxyRef: &gmcv2.ProxyObjectRef{Name: "pool", Namespace: "provider"},
				},
			},
		).Build()
	r := &EgressProxyReconciler{Client: c}
	key := types.NamespacedName{Namespace: "team-a", Name: proxyShareConfigMapName("provider", "pool")}

	for _, want := range []string{auditLoggingOff, auditLoggingConnectionsWithSource} {
		ep.Spec.AuditLogging = want
		if err := r.reconcileProxyShares(ctx, ep); err != nil {
			t.Fatalf("project with auditLogging %q: %v", want, err)
		}
		var cm corev1.ConfigMap
		if err := c.Get(ctx, key, &cm); err != nil {
			t.Fatalf("granted namespace holds no projection: %v", err)
		}
		if got := cm.Data[shareAuditLoggingKey]; got != want {
			t.Errorf("projection carries %s %q, want the provider's %q", shareAuditLoggingKey, got, want)
		}
	}
}

// issuedProxyTLSSecret returns the proxy TLS Secret ensureProxyCert would write.
func issuedProxyTLSSecret(t *testing.T, ep *gmcv2.EgressProxy) *corev1.Secret {
	t.Helper()
	certPEM, keyPEM, err := generateEgressProxyCert(ep.Namespace, proxyResourceName(ep))
	require.NoError(t, err)
	return buildEgressProxyCertSecret(ep, certPEM, keyPEM)
}

// Consumers install the projected ca.crt as a trust root (Q1324). A provider-side
// write that appends a CA to the Secret must be replaced before projection, so the
// consumer namespace never receives a certificate able to sign for a GitHub host.
func TestReconcileProxyShares_PlantedCAIsNeverProjected(t *testing.T) {
	ctx := context.Background()
	ep := proxyWithSharing("provider", "pool", "team-a")

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "planted"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	ghKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ghDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), DNSNames: []string{"api.github.com"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &ghKey.PublicKey, caKey)
	require.NoError(t, err)
	ghLeaf, err := x509.ParseCertificate(ghDER)
	require.NoError(t, err)

	verifiesGitHub := func(rootsPEM []byte) bool {
		pool := x509.NewCertPool()
		require.True(t, pool.AppendCertsFromPEM(rootsPEM))
		_, err := ghLeaf.Verify(x509.VerifyOptions{DNSName: "api.github.com", Roots: pool})
		return err == nil
	}

	secret := issuedProxyTLSSecret(t, ep)
	planted := append(slices.Clone(secret.Data[corev1.TLSCertKey]), encodeCertPEM(caDER)...)
	secret.Data[corev1.TLSCertKey] = planted
	require.True(t, verifiesGitHub(planted), "control: the planted bytes must sign for api.github.com")

	c := fake.NewClientBuilder().
		WithScheme(shareTestScheme(t)).
		WithObjects(ep, secret, &gmcv2.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gw"},
			Spec: gmcv2.ActionsGatewaySpec{
				DefaultProxyRef: &gmcv2.ProxyObjectRef{Name: "pool", Namespace: "provider"},
			},
		}).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: shareTestScheme(t)}
	key := types.NamespacedName{Namespace: "team-a", Name: proxyShareConfigMapName("provider", "pool")}

	// Projecting straight from the planted Secret refuses rather than copying it.
	require.Error(t, r.reconcileProxyShares(ctx, ep))
	var cm corev1.ConfigMap
	require.True(t, apierrors.IsNotFound(c.Get(ctx, key, &cm)), "projected from a planted Secret")

	// The reconcile order re-issues first, then projects the re-issued leaf alone.
	require.NoError(t, r.ensureProxyCert(ctx, ep))
	require.NoError(t, r.reconcileProxyShares(ctx, ep))
	require.NoError(t, c.Get(ctx, key, &cm))
	projected := []byte(cm.Data[shareCACertKey])
	assert.NotEqual(t, planted, projected)
	block, rest := pem.Decode(projected)
	require.NotNil(t, block)
	assert.Empty(t, bytes.TrimSpace(rest), "projection carries more than one certificate")
	assert.False(t, verifiesGitHub(projected), "the projected trust root signs for api.github.com")
}

// A Secret the share step refuses must not hold off revocation: projection stops,
// and a withdrawn grant is still pruned (Q1324).
func TestReconcileProxyShares_RefusedCertStillPrunesARevokedGrant(t *testing.T) {
	ctx := context.Background()
	ep := proxyWithSharing("provider", "pool", "team-a", "team-b")
	secret := issuedProxyTLSSecret(t, ep)
	referrer := func(ns string) *gmcv2.ActionsGateway {
		return &gmcv2.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gw"},
			Spec: gmcv2.ActionsGatewaySpec{
				DefaultProxyRef: &gmcv2.ProxyObjectRef{Name: "pool", Namespace: "provider"},
			},
		}
	}
	c := fake.NewClientBuilder().
		WithScheme(shareTestScheme(t)).
		WithObjects(secret, referrer("team-a"), referrer("team-b")).Build()
	r := &EgressProxyReconciler{Client: c}
	keyB := types.NamespacedName{Namespace: "team-b", Name: proxyShareConfigMapName("provider", "pool")}
	require.NoError(t, r.reconcileProxyShares(ctx, ep))
	require.NoError(t, c.Get(ctx, keyB, &corev1.ConfigMap{}))

	// Append a second certificate, then revoke team-b while the Secret is refused.
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	secret.Data[corev1.TLSCertKey] = append(slices.Clone(secret.Data[corev1.TLSCertKey]), secret.Data[corev1.TLSCertKey]...)
	require.NoError(t, c.Update(ctx, secret))
	ep.Spec.Sharing.AllowedNamespaces = []string{"team-a"}

	require.ErrorContains(t, r.reconcileProxyShares(ctx, ep), "is not the GMC-issued certificate")
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, keyB, &corev1.ConfigMap{})),
		"the revoked share survived because the Secret was refused")
}
