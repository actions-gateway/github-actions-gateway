//go:build integration

package integration_test

import (
	"net"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// These tests exercise the Q208 CNI-native FQDN egress opt-in on the v2 EgressProxy
// reconciler end-to-end against the real apiserver: the FQDN intent under a cilium or
// calico operator backend emits the matching CNI-native policy (asserted as an
// unstructured object — the suite installs stub Cilium/Calico CRDs), drops the GitHub
// CIDR rule from the standard NetworkPolicy (fail-closed), and removes the other-mode
// policy. The CIDR default is covered by the existing v2_egressproxy_test.go emission
// test, and the gke backend by v2_egressproxy_gke_fqdn_test.go.

var (
	ciliumGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}
	calicoGVK = schema.GroupVersionKind{Group: "projectcalico.org", Version: "v3", Kind: "NetworkPolicy"}
)

func getCNIPolicy(t *testing.T, ns, name string, gvk schema.GroupVersionKind) (*unstructured.Unstructured, error) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u)
	return u, err
}

// stdNPHasGitHubCIDR reports whether the standard NetworkPolicy carries a port-443
// ipBlock egress rule — the CIDR-mode GitHub allowlist that FQDN mode must drop. It is
// scoped to port 443 so the DNS rule's NodeLocal link-local ipBlock (on port 53) does
// not count as a GitHub CIDR rule.
func stdNPHasGitHubCIDR(np networkingv1.NetworkPolicy) bool {
	for _, rule := range np.Spec.Egress {
		on443 := false
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				on443 = true
			}
		}
		if !on443 {
			continue
		}
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				return true
			}
		}
	}
	return false
}

func TestV2_EgressProxy_FQDNModeCilium(t *testing.T) {
	const ns = "v2-ep-cilium-fqdn"
	createNamespace(t, ns)

	// Seed CIDRs so that, in CIDR mode, the standard NP *would* carry a GitHub rule —
	// proving FQDN mode drops it rather than it merely being absent for lack of data.
	ipCache := &controller.IPRangeCache{}
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	ipCache.Set([]net.IPNet{*cidr})
	startEgressProxyReconcilerWithBackend(t, ipCache, controller.FQDNBackendCilium)

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2.EgressProxySpec{EgressPolicyMode: gmcv2.EgressPolicyModeFQDN},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"

	// The CiliumNetworkPolicy is emitted, owned, scoped to 443 toFQDNs incl. api.github.com.
	var cnp *unstructured.Unstructured
	require.Eventually(t, func() bool {
		var gerr error
		cnp, gerr = getCNIPolicy(t, ns, fqdnName, ciliumGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "CiliumNetworkPolicy should be emitted for FQDN intent + cilium backend")

	assert.True(t, hasControllerOwnerRef(cnp.GetOwnerReferences(), egressProxyName), "Cilium policy must be owned by the EgressProxy")
	egress, found, err := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 2)
	githubRule := egress[1].(map[string]interface{})
	fqdns, found, err := unstructured.NestedSlice(githubRule, "toFQDNs")
	require.NoError(t, err)
	require.True(t, found)
	sawAPI := false
	for _, f := range fqdns {
		if f.(map[string]interface{})["matchName"] == "api.github.com" {
			sawAPI = true
		}
	}
	assert.True(t, sawAPI, "toFQDNs must include api.github.com")

	// The standard NetworkPolicy drops the GitHub CIDR rule (fail-closed posture).
	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &np))
	assert.False(t, stdNPHasGitHubCIDR(np), "FQDN mode must drop the GitHub CIDR egress rule from the standard NetworkPolicy")

	// The other-mode (Calico) policy must not exist.
	_, err = getCNIPolicy(t, ns, fqdnName, calicoGVK)
	assert.True(t, apierrors.IsNotFound(err), "Calico policy must be absent under the cilium backend, got %v", err)
}

// TestV2_EgressProxy_FQDNExtraDestinations proves the Q242 G.1 deliverable-3
// plumbing in FQDN mode under the cilium backend: an operator destinationFQDNs entry is appended to the
// CiliumNetworkPolicy toFQDNs set and to the proxy CONNECT host-suffix env, and a
// destinationCIDRs entry lands as an ipBlock peer on the standard NetworkPolicy
// (which is applied in FQDN mode too) and in PROXY_ALLOWED_CIDRS.
func TestV2_EgressProxy_FQDNExtraDestinations(t *testing.T) {
	const ns = "v2-ep-cilium-extra"
	createNamespace(t, ns)
	startEgressProxyReconcilerWithBackend(t, nil, controller.FQDNBackendCilium)

	const destFQDN = "proxy.golang.org"
	const destCIDR = "199.36.153.8/30"
	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			EgressPolicyMode: gmcv2.EgressPolicyModeFQDN,
			DestinationFQDNs: []string{destFQDN},
			DestinationCIDRs: []string{destCIDR},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"
	var cnp *unstructured.Unstructured
	require.Eventually(t, func() bool {
		var gerr error
		cnp, gerr = getCNIPolicy(t, ns, fqdnName, ciliumGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "CiliumNetworkPolicy should be emitted")

	// The extra FQDN is appended to the GitHub toFQDNs set (as a matchName entry).
	egress, found, err := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	githubRule := egress[len(egress)-1].(map[string]interface{})
	fqdns, found, err := unstructured.NestedSlice(githubRule, "toFQDNs")
	require.NoError(t, err)
	require.True(t, found)
	sawExtra, sawGitHub := false, false
	for _, f := range fqdns {
		switch f.(map[string]interface{})["matchName"] {
		case destFQDN:
			sawExtra = true
		case "api.github.com":
			sawGitHub = true
		}
	}
	assert.True(t, sawExtra, "toFQDNs must include the operator destinationFQDNs entry")
	assert.True(t, sawGitHub, "toFQDNs must still include the implicit GitHub hostnames")

	// Deployment env: opted in, GitHub + extra FQDN in host suffixes, CIDR set.
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &dep))
	env := containerEnv(t, dep)
	assert.Contains(t, env["PROXY_ALLOWED_HOST_SUFFIXES"], destFQDN)
	assert.Contains(t, env["PROXY_ALLOWED_HOST_SUFFIXES"], "api.github.com")
	assert.Equal(t, destCIDR, env["PROXY_ALLOWED_CIDRS"])

	// Standard NetworkPolicy (applied in FQDN mode too) carries the CIDR ipBlock.
	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &np))
	assert.True(t, npHasCIDRPeer(np, destCIDR), "destinationCIDRs must become an ipBlock peer even in FQDN mode")
}

// TestV2_EgressProxy_FQDNCarriesReferrerGHESHost covers Q506 #2. The GitHub host
// a tenant uses lives on the referring ActionsGateway, not on the EgressProxy, so an
// FQDN-mode GHES tenant used to get a policy naming six public hosts and nothing its
// traffic touches. The gateway is applied AFTER the pool is Ready and the reconciles
// its Ready status write queued have drained, so the assertion also proves the
// referrer watch edge fires: with the suite's 2s resync disabled and no not-ready
// requeue left running, nothing else can re-enqueue the proxy (the Q326 setup, for
// the same reason).
func TestV2_EgressProxy_FQDNCarriesReferrerGHESHost(t *testing.T) {
	const ns = "v2-ep-cilium-ghes"
	const ghesHost = "ghes.example.com"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	// A freshly-refreshed cache keeps EgressRulesStale quiet (vacuously here — FQDN
	// mode is not CIDR-refreshed). The periodic egress recheck is not affected by
	// cache state: it requeues every threshold/8 (~6h), far past this test either way.
	ipCache := &controller.IPRangeCache{}
	ipCache.MarkRefreshed(time.Now())
	reconciles := startEgressProxyReconcilerOpts(t, ipCache, controller.FQDNBackendCilium, nil)

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			EgressPolicyMode: gmcv2.EgressPolicyModeFQDN,
			MinReplicas:      ptr32(1),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"
	require.Eventually(t, func() bool {
		_, gerr := getCNIPolicy(t, ns, fqdnName, ciliumGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "CiliumNetworkPolicy should be emitted")

	// envtest runs no kubelet, so drive the pool Ready by hand — a not-ready pool
	// short-requeues, and that requeue alone would carry the gateway in.
	g := gomega.NewWithT(t)
	depKey := types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}
	var dep appsv1.Deployment
	g.Eventually(func() error {
		return k8sClient.Get(ctx, depKey, &dep)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
	dep.Status.Replicas = 1
	dep.Status.ReadyReplicas = 1
	require.NoError(t, k8sClient.Status().Update(ctx, &dep))

	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, egressProxyName, gmcv2.ConditionReady)
	}, 20*time.Second, 100*time.Millisecond).Should(
		gomega.HaveField("Status", metav1.ConditionTrue),
		"the pool must be Ready before the gateway lands")
	reconciles.waitIdle(t, ns, time.Second)

	// No referrer yet: the appliance is absent, which is the defect being fixed.
	cnp, err := getCNIPolicy(t, ns, fqdnName, ciliumGVK)
	require.NoError(t, err)
	require.False(t, ciliumPolicyAllowsFQDN(t, cnp, ghesHost),
		"no referrer binds a GHES host yet")

	gw := newV2GatewayWired("ghesgw", ns, "github-app", egressProxyName)
	gw.Spec.GitHubURL = "https://" + ghesHost + "/example-org"
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, gw) })

	require.Eventually(t, func() bool {
		u, gerr := getCNIPolicy(t, ns, fqdnName, ciliumGVK)
		if gerr != nil {
			return false
		}
		return ciliumPolicyAllowsFQDN(t, u, ghesHost)
	}, 15*time.Second, 100*time.Millisecond,
		"the referring gateway's GHES host must reach the emitted toFQDNs set")

	// The built-in public set is additive, never replaced.
	cnp, err = getCNIPolicy(t, ns, fqdnName, ciliumGVK)
	require.NoError(t, err)
	assert.True(t, ciliumPolicyAllowsFQDN(t, cnp, "api.github.com"),
		"the implicit GitHub hostnames must survive")
}

// TestV2_EgressProxy_FQDNOmitsUnallowlistedReferrerHost covers Q1322. githubURL is
// tenant-authored, so before this an FQDN-mode pool's policy allowed TCP/443 to any
// hostname a tenant put there, although the same host in spec.destinationFQDNs is
// refused at admission. Three gateways bind one pool: the suite's allowlisted
// appliance, a host on no allowlist, and a wildcard whose parent domain is
// allowlisted. Only the first may reach the emitted policy or the CONNECT allowlist,
// and the condition names the other two.
func TestV2_EgressProxy_FQDNOmitsUnallowlistedReferrerHost(t *testing.T) {
	const (
		ns          = "v2-ep-cilium-refused"
		allowedHost = "ghes.example.com"
		refusedHost = "exfil.example.net"
		// CoversFQDN reads this as golang.org, which the suite allowlists; emitted, it
		// would be a Cilium matchPattern admitting every host under golang.org.
		wildcardHost = "*.golang.org"
	)
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	startEgressProxyReconcilerWithBackend(t, nil, controller.FQDNBackendCilium)

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			EgressPolicyMode: gmcv2.EgressPolicyModeFQDN,
			MinReplicas:      ptr32(1),
			// Opts the proxy in to its CONNECT allowlist, the second emitted surface.
			DestinationFQDNs: []string{"proxy.golang.org"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	for _, bound := range []struct{ name, host string }{
		{"refusedgw", refusedHost}, {"wildgw", wildcardHost}, {"allowedgw", allowedHost},
	} {
		gw := newV2GatewayWired(bound.name, ns, "github-app", egressProxyName)
		gw.Spec.GitHubURL = "https://" + bound.host + "/example-org"
		require.NoError(t, k8sClient.Create(ctx, gw), "gateway bound to %s", bound.host)
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, gw) })
	}

	g := gomega.NewWithT(t)
	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, egressProxyName, gmcv2.ConditionGitHubEgressIncomplete)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", gmcv2.ReasonGitHubHostNotAllowlisted),
		gomega.HaveField("Message", gomega.ContainSubstring(refusedHost)),
		gomega.HaveField("Message", gomega.ContainSubstring(wildcardHost)),
		gomega.HaveField("Message", gomega.Not(gomega.ContainSubstring(allowedHost))),
		gomega.HaveField("Message", gomega.ContainSubstring("--allowed-egress-fqdns")),
	), "the condition must name both refused hosts and the remedy")

	// The status write follows the policy and Deployment applies in one reconcile, so
	// the condition above means both children have been built with the two refused
	// referrers in view, and stay so. The allowlisted gateway is created last and may
	// not have been in that reconcile, so wait for its host before reading the rest.
	fqdnName := egressProxyName + "-proxy-fqdn"
	var entries []string
	g.Eventually(func() []string {
		cnp, err := getCNIPolicy(t, ns, fqdnName, ciliumGVK)
		if err != nil {
			return nil
		}
		entries = ciliumPolicyFQDNEntries(t, cnp)
		return entries
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.ContainElement(allowedHost),
		"an allowlisted appliance is still carried")
	assert.Contains(t, entries, "api.github.com")
	assert.Contains(t, entries, "proxy.golang.org", "destinationFQDNs are unaffected")
	assert.NotContains(t, entries, refusedHost, "a host on no allowlist must not reach the policy")
	assert.NotContains(t, entries, wildcardHost, "a wildcard host must not reach the policy as a pattern")

	var suffixes string
	g.Eventually(func() string {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &dep); err != nil {
			return ""
		}
		suffixes = containerEnv(t, dep)["PROXY_ALLOWED_HOST_SUFFIXES"]
		return suffixes
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.ContainSubstring(allowedHost))
	assert.NotContains(t, suffixes, refusedHost)
	assert.NotContains(t, suffixes, wildcardHost)
}

// TestV2_EgressProxy_AllowlistChangeReachesPolicy covers the other half of gating a
// referrer host on a list that changes under a running GMC (Q1322): a platform admin
// adding the host must reach the pool's policy, and withdrawing it must leave. The
// pool is Ready and idle with the suite's resync off, so the only wake left is the
// one under test, the allowlist's change channel.
func TestV2_EgressProxy_AllowlistChangeReachesPolicy(t *testing.T) {
	const ns = "v2-ep-cilium-allowlist-change"
	const lateHost = "late.example.org"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	ipCache := &controller.IPRangeCache{}
	ipCache.MarkRefreshed(time.Now())
	al := allowlist.NewEgressDestination(nil, nil)
	changed := make(chan event.TypedGenericEvent[struct{}], 1)
	reconciles := startEgressProxyReconcilerAllowlisted(t, ipCache, controller.FQDNBackendCilium, nil, false, al, changed)

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			EgressPolicyMode: gmcv2.EgressPolicyModeFQDN,
			MinReplicas:      ptr32(1),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	gw := newV2GatewayWired("lategw", ns, "github-app", egressProxyName)
	gw.Spec.GitHubURL = "https://" + lateHost + "/example-org"
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, gw) })

	// envtest runs no kubelet, so drive the pool Ready by hand: a not-ready pool
	// short-requeues, and that requeue alone would carry the allowlist change in.
	g := gomega.NewWithT(t)
	depKey := types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}
	var dep appsv1.Deployment
	g.Eventually(func() error {
		return k8sClient.Get(ctx, depKey, &dep)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
	dep.Status.Replicas = 1
	dep.Status.ReadyReplicas = 1
	require.NoError(t, k8sClient.Status().Update(ctx, &dep))
	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, egressProxyName, gmcv2.ConditionReady)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.HaveField("Status", metav1.ConditionTrue))

	gap := func() *metav1.Condition {
		return egressProxyCondition(t, ns, egressProxyName, gmcv2.ConditionGitHubEgressIncomplete)
	}
	carried := func() bool {
		cnp, err := getCNIPolicy(t, ns, egressProxyName+"-proxy-fqdn", ciliumGVK)
		return err == nil && ciliumPolicyAllowsFQDN(t, cnp, lateHost)
	}
	g.Eventually(gap, 20*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", gmcv2.ReasonGitHubHostNotAllowlisted),
	), "with an empty allowlist the host is refused")
	require.False(t, carried())
	reconciles.waitIdle(t, ns, time.Second)

	// Replacing the set alone wakes nothing, which is what makes the send the cause.
	al.SetDynamic([]string{lateHost}, nil)
	g.Consistently(carried, 2*time.Second, 200*time.Millisecond).Should(gomega.BeFalse(),
		"nothing but the change channel may carry the new entry in")

	changed <- event.TypedGenericEvent[struct{}]{}
	g.Eventually(carried, 15*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"an allowlisted host must reach the policy on the change, not at the 6h re-check")
	g.Eventually(gap, 15*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", gmcv2.ReasonGitHubEgressAllowed),
	))

	reconciles.waitIdle(t, ns, time.Second)
	al.SetDynamic(nil, nil)
	changed <- event.TypedGenericEvent[struct{}]{}
	g.Eventually(carried, 15*time.Second, 100*time.Millisecond).Should(gomega.BeFalse(),
		"a withdrawn entry must leave the policy")
}

// TestV2_EgressProxy_AllowlistConfigMapAbsentThenWritten runs the EgressProxy and
// allowlist ConfigMap reconcilers on one manager, as the GMC does (Q1322). A named
// ConfigMap that does not exist is a supported install, and it produces no watch
// event, so nothing but the ConfigMap reconciler's read at start can tell a waiting
// pool that there is nothing to wait for. The wait is given an hour here so that the
// read is the only thing that can end it. The ConfigMap then arrives and leaves, and
// the pool's policy follows it through the change channel both ways.
func TestV2_EgressProxy_AllowlistConfigMapAbsentThenWritten(t *testing.T) {
	const (
		ns     = "v2-ep-cilium-allowlist-cm"
		cmName = "egress-allowlist"
		host   = "ghes.corp.example"
	)
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	ipCache := &controller.IPRangeCache{}
	ipCache.MarkRefreshed(time.Now())
	al := allowlist.NewEgressDestination(nil, nil)
	al.AwaitDynamic(time.Now().Add(time.Hour))
	changed := make(chan event.TypedGenericEvent[struct{}], 1)
	startEgressProxyReconcilerAllowlisted(t, ipCache, controller.FQDNBackendCilium, nil, false, al, changed,
		func(mgr ctrl.Manager) error {
			return (&controller.EgressDestinationAllowlistReconciler{
				Client:        mgr.GetClient(),
				ConfigMapName: cmName,
				Namespace:     ns,
				Allowlist:     al,
				Changed:       changed,
			}).SetupWithManager(mgr)
		})

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			EgressPolicyMode: gmcv2.EgressPolicyModeFQDN,
			MinReplicas:      ptr32(1),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })
	gw := newV2GatewayWired("ghesgw", ns, "github-app", egressProxyName)
	gw.Spec.GitHubURL = "https://" + host + "/example-org"
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, gw) })

	g := gomega.NewWithT(t)
	refused := gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", gmcv2.ReasonGitHubHostNotAllowlisted),
		gomega.HaveField("Message", gomega.ContainSubstring(host)),
	)
	gap := func() *metav1.Condition {
		return egressProxyCondition(t, ns, egressProxyName, gmcv2.ConditionGitHubEgressIncomplete)
	}
	carried := func() bool {
		cnp, err := getCNIPolicy(t, ns, egressProxyName+"-proxy-fqdn", ciliumGVK)
		return err == nil && ciliumPolicyAllowsFQDN(t, cnp, host)
	}

	g.Eventually(gap, 30*time.Second, 100*time.Millisecond).Should(refused,
		"with no ConfigMap to read, the pool must reconcile on the static list and say what it left out")
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &appsv1.Deployment{}),
		"a pool that reconciled has its children")
	require.False(t, carried())

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns},
		Data:       map[string]string{controller.EgressDestinationFQDNsConfigMapKey: "corp.example"},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cm) })
	g.Eventually(carried, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"writing the ConfigMap must carry the host into the policy")
	g.Eventually(gap, 20*time.Second, 100*time.Millisecond).Should(gomega.HaveField("Status", metav1.ConditionFalse))

	require.NoError(t, k8sClient.Delete(ctx, cm))
	g.Eventually(carried, 20*time.Second, 100*time.Millisecond).Should(gomega.BeFalse(),
		"deleting the ConfigMap falls back to the static list, which does not cover the host")
	g.Eventually(gap, 20*time.Second, 100*time.Millisecond).Should(refused)
}

// ciliumPolicyFQDNEntries returns every matchName and matchPattern value in the
// policy's GitHub egress rule.
func ciliumPolicyFQDNEntries(t *testing.T, cnp *unstructured.Unstructured) []string {
	t.Helper()
	egress, found, err := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	fqdns, found, err := unstructured.NestedSlice(egress[len(egress)-1].(map[string]interface{}), "toFQDNs")
	require.NoError(t, err)
	require.True(t, found)
	var out []string
	for _, f := range fqdns {
		for _, v := range f.(map[string]interface{}) {
			out = append(out, v.(string))
		}
	}
	return out
}

// ciliumPolicyAllowsFQDN reports whether the policy's GitHub egress rule names the
// given host as an exact matchName entry.
func ciliumPolicyAllowsFQDN(t *testing.T, cnp *unstructured.Unstructured, host string) bool {
	t.Helper()
	egress, found, err := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	fqdns, found, err := unstructured.NestedSlice(egress[len(egress)-1].(map[string]interface{}), "toFQDNs")
	require.NoError(t, err)
	require.True(t, found)
	for _, f := range fqdns {
		if f.(map[string]interface{})["matchName"] == host {
			return true
		}
	}
	return false
}

func TestV2_EgressProxy_FQDNModeCalico(t *testing.T) {
	const ns = "v2-ep-calico-fqdn"
	createNamespace(t, ns)

	ipCache := &controller.IPRangeCache{}
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	ipCache.Set([]net.IPNet{*cidr})
	startEgressProxyReconcilerWithBackend(t, ipCache, controller.FQDNBackendCalico)

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2.EgressProxySpec{EgressPolicyMode: gmcv2.EgressPolicyModeFQDN},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"

	var calico *unstructured.Unstructured
	require.Eventually(t, func() bool {
		var gerr error
		calico, gerr = getCNIPolicy(t, ns, fqdnName, calicoGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "Calico NetworkPolicy should be emitted for FQDN intent + calico backend")

	assert.True(t, hasControllerOwnerRef(calico.GetOwnerReferences(), egressProxyName), "Calico policy must be owned by the EgressProxy")
	egress, found, err := unstructured.NestedSlice(calico.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	githubRule := egress[len(egress)-1].(map[string]interface{})
	domains, found, err := unstructured.NestedStringSlice(githubRule, "destination", "domains")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, domains, "api.github.com")

	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &np))
	assert.False(t, stdNPHasGitHubCIDR(np), "FQDN mode must drop the GitHub CIDR egress rule")

	_, err = getCNIPolicy(t, ns, fqdnName, ciliumGVK)
	assert.True(t, apierrors.IsNotFound(err), "Cilium policy must be absent under the calico backend, got %v", err)
}
