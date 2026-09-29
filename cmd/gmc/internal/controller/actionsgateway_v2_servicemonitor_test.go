package controller

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// v2ServiceMonitorScheme is actionsGatewayV2TestScheme with the ServiceMonitor GVK
// mapped to unstructured, simulating a cluster with the monitoring.coreos.com CRD.
func v2ServiceMonitorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := actionsGatewayV2TestScheme(t)
	s.AddKnownTypeWithName(serviceMonitorGVK, &unstructured.Unstructured{})
	listGVK := serviceMonitorGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

// v2GatewayWithUID returns a gateway with a UID so SetControllerReference stamps a
// usable owner reference on children.
func v2GatewayWithUID(name, ns string) *gmcv2alpha1.ActionsGateway {
	ag := v2Gateway(name, ns, "github-app", "")
	ag.UID = "ag-uid-1"
	return ag
}

func TestBuildAGCServiceMonitorV2(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "")
	sm := buildAGCServiceMonitorV2(ag)

	assert.Equal(t, "gw-agc-metrics", sm.GetName())
	assert.Equal(t, "team-a", sm.GetNamespace())
	assert.Equal(t, serviceMonitorGVK, sm.GroupVersionKind())

	endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, endpoints, 1)
	ep0 := endpoints[0].(map[string]interface{})
	assert.Equal(t, "metrics", ep0["port"])
	assert.Equal(t, "https", ep0["scheme"])
	assert.NotContains(t, ep0, "relabelings", "the AGC stamps its own namespace label")

	tlsCfg := ep0["tlsConfig"].(map[string]interface{})
	assert.NotContains(t, tlsCfg, "insecureSkipVerify")
	for _, key := range []string{"ca", "cert"} {
		ref := tlsCfg[key].(map[string]interface{})["secret"].(map[string]interface{})
		assert.Equal(t, "gw-agc-metrics-client", ref["name"], key)
	}
	assert.Equal(t, "gw-agc-metrics-client", tlsCfg["keySecret"].(map[string]interface{})["name"])

	// serverName must be a SAN on the cert the AGC actually serves, or the scrape
	// fails verification.
	bundle, err := generateMetricsCertsV2(ag.Namespace, agcNameV2(ag))
	require.NoError(t, err)
	cert, err := parseCertPEM(bundle.serverCertPEM)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(bundle.caPEM))
	_, err = cert.Verify(x509.VerifyOptions{DNSName: tlsCfg["serverName"].(string), Roots: roots})
	require.NoError(t, err, "serverName %q must verify against the metrics server cert", tlsCfg["serverName"])
}

// TestBuildAGCServiceMonitorV2_SelectsOnlyItsOwnService proves the monitor's
// selector matches the AGC Service the reconciler creates, and not a sibling
// gateway's in the same namespace (§H.16 #1).
func TestBuildAGCServiceMonitorV2_SelectsOnlyItsOwnService(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "")
	sibling := v2Gateway("other", "team-a", "github-app", "")

	matchLabels, found, err := unstructured.NestedStringMap(buildAGCServiceMonitorV2(ag).Object, "spec", "selector", "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	sel := labels.SelectorFromSet(matchLabels)

	assert.True(t, sel.Matches(labels.Set(buildAGCServiceV2(ag).Labels)), "must select its own AGC Service")
	assert.False(t, sel.Matches(labels.Set(buildAGCServiceV2(sibling).Labels)), "must not select a sibling gateway's AGC Service")
}

func TestV2ApplyOrPruneServiceMonitor_DisabledIsNoOp(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t) // no ServiceMonitor GVK
	ag := v2GatewayWithUID("gw", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.applyOrPruneServiceMonitor(context.Background(), ag))
}

func TestV2ApplyOrPruneServiceMonitor_EnabledCreatesOwned(t *testing.T) {
	scheme := v2ServiceMonitorScheme(t)
	ag := v2GatewayWithUID("gw", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, EnableServiceMonitor: true}
	ctx := context.Background()

	require.NoError(t, r.applyOrPruneServiceMonitor(ctx, ag))

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "gw-agc-metrics"}, sm))
	require.Len(t, sm.GetOwnerReferences(), 1, "ServiceMonitor must be owned for GC on delete")
	assert.Equal(t, "gw", sm.GetOwnerReferences()[0].Name)
}

func TestV2ApplyOrPruneServiceMonitor_EnabledMissingCRDSkips(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t) // no ServiceMonitor GVK → NoMatch
	ag := v2GatewayWithUID("gw", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, EnableServiceMonitor: true}

	require.NoError(t, r.applyOrPruneServiceMonitor(context.Background(), ag))
}

func TestV2ApplyOrPruneServiceMonitor_EnabledPropagatesRealError(t *testing.T) {
	scheme := v2ServiceMonitorScheme(t)
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return boom
		},
	}).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, EnableServiceMonitor: true}

	err := r.applyOrPruneServiceMonitor(context.Background(), v2GatewayWithUID("gw", "team-a"))
	require.ErrorIs(t, err, boom, "a non-NoMatch apply error must fail the reconcile, not be swallowed")
}

func TestV2ApplyOrPruneServiceMonitor_DisabledPrunesExisting(t *testing.T) {
	scheme := v2ServiceMonitorScheme(t)
	ag := v2GatewayWithUID("gw", "team-a")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, EnableServiceMonitor: true}
	ctx := context.Background()

	require.NoError(t, r.applyOrPruneServiceMonitor(ctx, ag))
	r.EnableServiceMonitor = false
	require.NoError(t, r.applyOrPruneServiceMonitor(ctx, ag))

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "gw-agc-metrics"}, sm)
	assert.True(t, apierrors.IsNotFound(err), "pruned ServiceMonitor must be gone")
}
