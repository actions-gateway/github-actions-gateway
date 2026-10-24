package controller

import (
	"context"
	"crypto/x509"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func attributingPool(allowed ...string) *gmcv2alpha1.EgressProxy {
	return newEP("shared", "provider", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.FailureAttribution = failureAttributionPerConsumerPort
		ep.Spec.Sharing = &gmcv2alpha1.ProxySharing{AllowedNamespaces: allowed}
	})
}

// gatewayUsing returns a gateway in ns whose defaultProxyRef names provider/shared,
// opted in when perJob is set.
func gatewayUsing(ns string, perJob bool) *gmcv2alpha1.ActionsGateway {
	ag := &gmcv2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gw"}}
	ag.Spec.DefaultProxyRef = &gmcv2alpha1.ProxyObjectRef{Name: "shared"}
	if ns != "provider" {
		ag.Spec.DefaultProxyRef.Namespace = "provider"
	}
	if perJob {
		ag.Spec.ProxyFailureAttribution = proxyFailureAttributionPerJob
	}
	return ag
}

func reconcilePool(t *testing.T, c client.Client) {
	t.Helper()
	r := &EgressProxyReconciler{Client: c, Scheme: c.Scheme(), IPCache: &IPRangeCache{}, ProxyImage: "proxy:test"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "provider", Name: "shared"}})
	require.NoError(t, err)
}

func poolEnv(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "provider", Name: "shared-proxy"}, &dep))
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

func TestAllocateAttributionPorts_KeepsHeldPortsAndFillsTheLowestFree(t *testing.T) {
	got := allocateAttributionPorts([]string{"a", "b", "c"}, map[string]int{"b": 8101, "gone": 8100})
	assert.Equal(t, map[string]int{"a": 8100, "b": 8101, "c": 8102}, got,
		"b keeps 8101 and the freed 8100 goes to the first new namespace")

	got = allocateAttributionPorts([]string{"a", "b"}, map[string]int{"a": 9000, "b": 8100})
	assert.Equal(t, map[string]int{"a": 8101, "b": 8100}, got, "a port outside the range is reassigned")

	assert.Nil(t, allocateAttributionPorts(nil, map[string]int{"a": 8100}))
}

func TestAttributionPortsEnv_RoundTripsThroughTheDeployment(t *testing.T) {
	ports := map[string]int{"team-b": 8101, "team-a": 8100}
	assert.Equal(t, "team-a=8100,team-b=8101", attributionPortsEnv(ports))

	dep := &appsv1.Deployment{}
	dep.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: proxyContainerName,
		Env:  []corev1.EnvVar{{Name: "PROXY_ATTRIBUTION_PORTS", Value: attributionPortsEnv(ports)}},
	}}
	assert.Equal(t, ports, liveAttributionPorts(dep))
	assert.Empty(t, liveAttributionPorts(&appsv1.Deployment{}))
}

func TestAttributedNamespacesFor_NeedsOptInReferenceAndGrant(t *testing.T) {
	other := gatewayUsing("team-e", true)
	other.Spec.DefaultProxyRef.Name = "another-pool"
	rsOnly := &gmcv2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Namespace: "team-f", Name: "rs"}}
	rsOnly.Spec.ProxyRef = &gmcv2alpha1.ProxyObjectRef{Namespace: "provider", Name: "shared"}
	optInNoRef := &gmcv2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Namespace: "team-f", Name: "gw"}}
	optInNoRef.Spec.ProxyFailureAttribution = proxyFailureAttributionPerJob

	ep := attributingPool("team-a", "team-b", "team-e", "team-f")
	c := fake.NewClientBuilder().WithScheme(egressProxyTestScheme(t)).WithObjects(
		gatewayUsing("provider", true), // the pool's own namespace, no grant needed
		gatewayUsing("team-a", true),   // granted and opted in
		gatewayUsing("team-b", false),  // granted, not opted in
		gatewayUsing("team-d", true),   // opted in, not granted
		other,                          // opted in and granted, names another pool
		rsOnly, optInNoRef,             // referenced by a RunnerSet, opted in by a gateway that names no pool
	).Build()

	got, err := attributedNamespacesFor(context.Background(), c, ep)
	require.NoError(t, err)
	assert.Equal(t, []string{"provider", "team-a", "team-f"}, got)

	ep.Spec.FailureAttribution = "Disabled"
	got, err = attributedNamespacesFor(context.Background(), c, ep)
	require.NoError(t, err)
	assert.Empty(t, got, "a pool that has not opted in attributes nothing")
}

// Off must be off at the pool: an upgraded GMC rolls no pool whose spec did not
// opt in, so the children it builds are exactly the ones it built before.
func TestEgressProxyReconcile_UnattributedPoolIsUnchanged(t *testing.T) {
	for name, ep := range map[string]*gmcv2alpha1.EgressProxy{
		"pool disabled": newEP("shared", "provider", func(ep *gmcv2alpha1.EgressProxy) {
			ep.Spec.Sharing = &gmcv2alpha1.ProxySharing{AllowedNamespaces: []string{"team-a"}}
		}),
		"pool on, no namespace opted in": attributingPool("team-a"),
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(egressProxyTestScheme(t)).
				WithObjects(ep, gatewayUsing("team-a", name == "pool disabled")).WithStatusSubresource(ep).Build()
			reconcilePool(t, c)
			ctx := context.Background()

			var dep appsv1.Deployment
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-proxy"}, &dep))
			want := buildEgressProxyDeployment(ep, "proxy:test", nil)
			assert.Equal(t, want.Spec.Template, dep.Spec.Template)

			var svc corev1.Service
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-proxy"}, &svc))
			assert.Equal(t, buildEgressProxyService(ep).Spec.Ports, svc.Spec.Ports)

			for _, key := range []types.NamespacedName{
				{Namespace: "provider", Name: "shared-proxy-attr"},
			} {
				assert.True(t, apierrors.IsNotFound(c.Get(ctx, key, &corev1.Service{})), "%s must not exist", key)
			}
			for _, name := range []string{"shared-attribution-tls", "shared-attribution-clients"} {
				assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: name}, &corev1.Secret{})), "%s must not exist", name)
			}
		})
	}
}

func TestEgressProxyReconcile_GivesEachOptedInNamespaceItsOwnPort(t *testing.T) {
	ep := attributingPool("team-a", "team-b")
	c := fake.NewClientBuilder().WithScheme(egressProxyTestScheme(t)).WithObjects(
		ep, gatewayUsing("provider", true), gatewayUsing("team-a", true), gatewayUsing("team-b", false),
	).WithStatusSubresource(ep).Build()
	reconcilePool(t, c)
	ctx := context.Background()

	env := poolEnv(t, c)
	assert.Equal(t, "provider=8100,team-a=8101", env["PROXY_ATTRIBUTION_PORTS"])
	assert.Equal(t, "8444", env["PROXY_ATTRIBUTION_PORT"])
	assert.NotEmpty(t, env["PROXY_ATTRIBUTION_CLIENT_CA_FILE"])

	var svc corev1.Service
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-proxy"}, &svc))
	targets := map[string]int32{}
	for _, p := range svc.Spec.Ports {
		targets[p.Name] = p.TargetPort.IntVal
	}
	assert.Equal(t, int32(8100), targets["attr-8100"])
	assert.Equal(t, int32(8101), targets["attr-8101"])

	var headless corev1.Service
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-proxy-attr"}, &headless))
	assert.Equal(t, corev1.ClusterIPNone, headless.Spec.ClusterIP)

	// team-a's workers on 8101 only, and its AGC on the read port, each as one peer
	// so the namespace and pod selectors AND.
	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-proxy"}, &np))
	rulesFor := func(ns string) map[int32]metav1.LabelSelector {
		out := map[int32]metav1.LabelSelector{}
		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] == ns {
					require.Len(t, rule.From, 1)
					require.NotNil(t, peer.PodSelector)
					out[rule.Ports[0].Port.IntVal] = *peer.PodSelector
				}
			}
		}
		return out
	}
	teamA := rulesFor("team-a")
	assert.Equal(t, map[string]string{labelComponent: componentWorkload}, teamA[8101].MatchLabels)
	assert.Equal(t, agcAppName, teamA[attributionReadPort].MatchLabels["app.kubernetes.io/name"])
	assert.Len(t, teamA, 3, "the sharing rule on 8080, its own port, and the read port")
	assert.Empty(t, rulesFor("team-b")[8100], "a namespace that did not opt in is admitted on no attributed port")

	// One client cert per attributed namespace, naming it and chaining to the CA
	// the pool verifies against; the server cert covers the Service name.
	var server, clients corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-attribution-tls"}, &server))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-attribution-clients"}, &clients))
	assert.Equal(t, []string{"provider", "team-a"}, attributionClientNamespaces(&clients))
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(server.Data[metricsCACertKey]))
	for _, ns := range []string{"provider", "team-a"} {
		cert, err := parseCertPEM(clients.Data[ns+".crt"])
		require.NoError(t, err)
		assert.Equal(t, ns, cert.Subject.CommonName)
		_, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		assert.NoError(t, err, "%s's client cert must chain to the pool's attribution CA", ns)
	}
	serverCert, err := parseCertPEM(server.Data[corev1.TLSCertKey])
	require.NoError(t, err)
	_, err = serverCert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "shared-proxy.provider.svc"})
	assert.NoError(t, err)
}

// A grant change moves no namespace that keeps its port, and the bundle is
// reissued only when the namespace set changes, rolling the pool through the CA
// hash on its pod template.
func TestEgressProxyReconcile_PortsAndBundleAreStableAcrossGrantChanges(t *testing.T) {
	ep := attributingPool("team-a", "team-b")
	own := gatewayUsing("provider", true)
	teamB := gatewayUsing("team-b", false)
	c := fake.NewClientBuilder().WithScheme(egressProxyTestScheme(t)).WithObjects(
		ep, own, gatewayUsing("team-a", true), teamB,
	).WithStatusSubresource(ep).Build()
	ctx := context.Background()
	caHash := func() string {
		var dep appsv1.Deployment
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-proxy"}, &dep))
		return dep.Spec.Template.Annotations[annAttributionCA]
	}

	reconcilePool(t, c)
	first := caHash()
	require.NotEmpty(t, first)
	reconcilePool(t, c)
	assert.Equal(t, "provider=8100,team-a=8101", poolEnv(t, c)["PROXY_ATTRIBUTION_PORTS"])
	assert.Equal(t, first, caHash(), "an unchanged namespace set keeps the bundle")

	own.Spec.ProxyFailureAttribution = ""
	require.NoError(t, c.Update(ctx, own))
	reconcilePool(t, c)
	assert.Equal(t, "team-a=8101", poolEnv(t, c)["PROXY_ATTRIBUTION_PORTS"], "team-a keeps its port")
	second := caHash()
	assert.NotEqual(t, first, second, "a namespace leaving reissues the bundle")

	teamB.Spec.ProxyFailureAttribution = proxyFailureAttributionPerJob
	require.NoError(t, c.Update(ctx, teamB))
	reconcilePool(t, c)
	assert.Equal(t, "team-a=8101,team-b=8100", poolEnv(t, c)["PROXY_ATTRIBUTION_PORTS"], "the freed port is reused")
	assert.NotEqual(t, second, caHash())
}

// The GMC's role grants no delete on Secrets, so a reconcile that deletes one is
// refused 403 on a real cluster. The fake client and envtest run as admin, so the
// refusal is injected here.
func TestEgressProxyReconcile_NeverDeletesSecrets(t *testing.T) {
	ep := attributingPool("team-a")
	teamA := gatewayUsing("team-a", true)
	base := fake.NewClientBuilder().WithScheme(egressProxyTestScheme(t)).
		WithObjects(ep, teamA).WithStatusSubresource(ep).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewForbidden(corev1.Resource("secrets"), obj.GetName(), nil)
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
	ctx := context.Background()

	reconcilePool(t, c)
	require.Equal(t, "team-a=8100", poolEnv(t, c)["PROXY_ATTRIBUTION_PORTS"])

	teamA.Spec.ProxyFailureAttribution = ""
	require.NoError(t, c.Update(ctx, teamA))
	reconcilePool(t, c) // a pool that stops attributing
	reconcilePool(t, c) // and one that never did, in steady state
	assert.Empty(t, poolEnv(t, c)["PROXY_ATTRIBUTION_PORTS"])
	for _, name := range []string{"shared-attribution-tls", "shared-attribution-clients"} {
		var s corev1.Secret
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: name}, &s))
		for k, v := range s.Data {
			assert.Empty(t, v, "%s[%s] must hold no key material once nothing is attributed", name, k)
		}
	}
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: "provider", Name: "shared-proxy-attr"}, &corev1.Service{})))
}
