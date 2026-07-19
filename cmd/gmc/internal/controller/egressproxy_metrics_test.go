package controller

import (
	"context"
	"errors"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// egressProxyServiceMonitorScheme returns a scheme that also maps the ServiceMonitor
// GVK to unstructured, so the fake client's RESTMapper resolves it — simulating a
// cluster with the monitoring.coreos.com CRD installed.
func egressProxyServiceMonitorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := egressProxyTestScheme(t)
	s.AddKnownTypeWithName(serviceMonitorGVK, &unstructured.Unstructured{})
	listGVK := serviceMonitorGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

// epWithUID returns an EgressProxy with a UID so SetControllerReference stamps a
// usable owner reference on children.
func epWithUID(name, ns string) *gmcv2alpha1.EgressProxy {
	ep := newEP(name, ns, nil)
	ep.UID = "ep-uid-1"
	return ep
}

func TestEgressProxyMetricsDerivedNames(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	assert.Equal(t, "shared-metrics-tls", egressProxyMetricsTLSSecretName(ep))
	assert.Equal(t, "shared-metrics-client", egressProxyMetricsClientSecretName(ep))
	assert.Equal(t, "shared-proxy-metrics", egressProxyMetricsServiceMonitorName(ep))
	assert.Equal(t, "shared-proxy.team-a.svc", egressProxyMetricsServiceDNSName(ep))
}

func TestBuildEgressProxyMetricsSecrets(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	b := &metricsCertBundle{
		caPEM:         []byte("ca"),
		serverCertPEM: []byte("server-cert"),
		serverKeyPEM:  []byte("server-key"),
		clientCertPEM: []byte("client-cert"),
		clientKeyPEM:  []byte("client-key"),
	}

	// Server bundle: mounted into the proxy pod. Carries CA + server cert/key.
	server := buildEgressProxyMetricsTLSSecret(ep, b)
	assert.Equal(t, "shared-metrics-tls", server.Name)
	assert.Equal(t, "team-a", server.Namespace)
	assert.Equal(t, corev1.SecretTypeTLS, server.Type)
	assert.Equal(t, []byte("server-cert"), server.Data[corev1.TLSCertKey])
	assert.Equal(t, []byte("server-key"), server.Data[corev1.TLSPrivateKeyKey])
	assert.Equal(t, []byte("ca"), server.Data[metricsCACertKey])
	assert.Equal(t, "shared", server.Labels[egressProxyComponentLabel])

	// Client bundle: published for the scraper. Carries CA + client cert/key —
	// never the server key.
	client := buildEgressProxyMetricsClientSecret(ep, b)
	assert.Equal(t, "shared-metrics-client", client.Name)
	assert.Equal(t, corev1.SecretTypeTLS, client.Type)
	assert.Equal(t, []byte("client-cert"), client.Data[corev1.TLSCertKey])
	assert.Equal(t, []byte("client-key"), client.Data[corev1.TLSPrivateKeyKey])
	assert.Equal(t, []byte("ca"), client.Data[metricsCACertKey])
	assert.NotEqual(t, b.serverKeyPEM, client.Data[corev1.TLSPrivateKeyKey],
		"the scraper bundle must not carry the server private key")
}

func TestBuildEgressProxyServiceMonitor(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	sm := buildEgressProxyServiceMonitor(ep)

	assert.Equal(t, "shared-proxy-metrics", sm.GetName())
	assert.Equal(t, "team-a", sm.GetNamespace())
	assert.Equal(t, serviceMonitorGVK, sm.GroupVersionKind())
	assert.Equal(t, "shared", sm.GetLabels()[egressProxyComponentLabel])

	// Selector matches the "<ep>-proxy" metrics Service by the per-EgressProxy labels.
	selLabels, found, err := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "shared", selLabels[egressProxyComponentLabel])

	endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, endpoints, 1)
	ep0 := endpoints[0].(map[string]interface{})
	assert.Equal(t, "metrics", ep0["port"])
	assert.Equal(t, "/metrics", ep0["path"])
	assert.Equal(t, "https", ep0["scheme"])

	tlsCfg := ep0["tlsConfig"].(map[string]interface{})
	assert.Equal(t, "shared-proxy.team-a.svc", tlsCfg["serverName"],
		"serverName must be a SAN on the metrics server cert so the scrape verifies")

	// mTLS client material is read from the per-EgressProxy scraper client Secret.
	ca := tlsCfg["ca"].(map[string]interface{})["secret"].(map[string]interface{})
	assert.Equal(t, "shared-metrics-client", ca["name"])
	assert.Equal(t, metricsCACertKey, ca["key"])
	cert := tlsCfg["cert"].(map[string]interface{})["secret"].(map[string]interface{})
	assert.Equal(t, "shared-metrics-client", cert["name"])
	assert.Equal(t, corev1.TLSCertKey, cert["key"])
	keySecret := tlsCfg["keySecret"].(map[string]interface{})
	assert.Equal(t, "shared-metrics-client", keySecret["name"])
	assert.Equal(t, corev1.TLSPrivateKeyKey, keySecret["key"])

	// The proxy metrics carry no intrinsic namespace label, so it is stamped from the
	// scrape target's namespace (Q314 parity).
	relabelings := ep0["relabelings"].([]interface{})
	require.Len(t, relabelings, 1)
	rl := relabelings[0].(map[string]interface{})
	assert.Equal(t, "namespace", rl["targetLabel"])
}

// TestEgressProxyEnsureMetricsCerts_CreatesOwnedBundle proves ensureMetricsCerts
// generates a per-EgressProxy metrics PKI and writes both the server bundle (mounted
// into the proxy) and the scraper client bundle (published for monitoring), each with
// an owner reference for GC.
func TestEgressProxyEnsureMetricsCerts_CreatesOwnedBundle(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := epWithUID("shared", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()

	require.NoError(t, r.ensureMetricsCerts(ctx, ep))

	var server corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-metrics-tls"}, &server))
	assert.Equal(t, corev1.SecretTypeTLS, server.Type)
	// Server bundle: CA + server cert/key, and the server cert parses + is valid for
	// the "<ep>-proxy" Service DNS name the ServiceMonitor pins.
	assert.NotEmpty(t, server.Data[corev1.TLSPrivateKeyKey])
	assert.NotEmpty(t, server.Data[metricsCACertKey])
	cert, err := parseCertPEM(server.Data[corev1.TLSCertKey])
	require.NoError(t, err)
	require.NoError(t, cert.VerifyHostname("shared-proxy.team-a.svc"))
	require.Len(t, server.OwnerReferences, 1)
	assert.Equal(t, "EgressProxy", server.OwnerReferences[0].Kind)

	var scraperClient corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-metrics-client"}, &scraperClient))
	assert.NotEmpty(t, scraperClient.Data[corev1.TLSCertKey])
	assert.NotEmpty(t, scraperClient.Data[metricsCACertKey])
	require.Len(t, scraperClient.OwnerReferences, 1)

	// Idempotent: a second call with a valid, non-expiring bundle is a no-op (no
	// error, no rotation).
	require.NoError(t, r.ensureMetricsCerts(ctx, ep))
}

// TestEgressProxyApplyOrPruneServiceMonitor_DisabledIsNoOp verifies that with the
// flag off (default) no ServiceMonitor is created — even when the CRD is absent, the
// call must not error.
func TestEgressProxyApplyOrPruneServiceMonitor_DisabledIsNoOp(t *testing.T) {
	scheme := egressProxyTestScheme(t) // no ServiceMonitor GVK
	ep := epWithUID("shared", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, EnableServiceMonitor: false}

	require.NoError(t, r.applyOrPruneServiceMonitor(context.Background(), ep))
}

// TestEgressProxyApplyOrPruneServiceMonitor_EnabledCreates verifies that with the
// flag on and the CRD present, the per-EgressProxy ServiceMonitor is created with the
// per-EgressProxy selector, the scraper client-secret refs, and an owner reference.
func TestEgressProxyApplyOrPruneServiceMonitor_EnabledCreates(t *testing.T) {
	scheme := egressProxyServiceMonitorScheme(t)
	ep := epWithUID("shared", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, EnableServiceMonitor: true}
	ctx := context.Background()

	require.NoError(t, r.applyOrPruneServiceMonitor(ctx, ep))

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-proxy-metrics"}, sm))
	require.Len(t, sm.GetOwnerReferences(), 1, "ServiceMonitor must be owned for GC on delete")
	assert.Equal(t, "shared", sm.GetOwnerReferences()[0].Name)
	matchLabels, _, _ := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
	assert.Equal(t, "shared", matchLabels[egressProxyComponentLabel])
}

// TestEgressProxyApplyOrPruneServiceMonitor_EnabledMissingCRDSkips verifies that
// opting in without the CRD installed does NOT fail the reconcile.
func TestEgressProxyApplyOrPruneServiceMonitor_EnabledMissingCRDSkips(t *testing.T) {
	scheme := egressProxyTestScheme(t) // no ServiceMonitor GVK → NoMatch
	ep := epWithUID("shared", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, EnableServiceMonitor: true}

	require.NoError(t, r.applyOrPruneServiceMonitor(context.Background(), ep))
}

// TestEgressProxyApplyOrPruneServiceMonitor_EnabledPropagatesRealError verifies a
// non-NoMatch apply error is not swallowed like the missing-CRD case.
func TestEgressProxyApplyOrPruneServiceMonitor_EnabledPropagatesRealError(t *testing.T) {
	scheme := egressProxyServiceMonitorScheme(t)
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return boom
		},
	}).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, EnableServiceMonitor: true}

	err := r.applyOrPruneServiceMonitor(context.Background(), epWithUID("shared", "team-a"))
	require.ErrorIs(t, err, boom, "a non-NoMatch apply error must fail the reconcile, not be swallowed")
}

// TestEgressProxyApplyOrPruneServiceMonitor_DisabledPrunesExisting verifies that
// flipping the flag off deletes a previously-created ServiceMonitor.
func TestEgressProxyApplyOrPruneServiceMonitor_DisabledPrunesExisting(t *testing.T) {
	scheme := egressProxyServiceMonitorScheme(t)
	ep := epWithUID("shared", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()

	r.EnableServiceMonitor = true
	require.NoError(t, r.applyOrPruneServiceMonitor(ctx, ep))

	r.EnableServiceMonitor = false
	require.NoError(t, r.applyOrPruneServiceMonitor(ctx, ep))

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-proxy-metrics"}, sm)
	assert.True(t, apierrors.IsNotFound(err), "pruned ServiceMonitor must be gone")
}
