package controller

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func attributionProjection(ns, proxyNS, proxyName, port string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: apinames.Join(apinames.MaxLabelValue, "proxy-attribution", proxyNS, proxyName), Namespace: ns},
		Data:       map[string]string{proxyAttributionPortKey: port, proxyShareProxyNSKey: proxyNS, proxyShareProxyNameKey: proxyName},
	}
}

// A worker moves onto the port the GMC projected for its namespace, from a pool in
// its own namespace and from a shared one alike, and stays on the unattributed
// port when nothing is projected.
func TestResolveRunnerSetRefs_UsesTheProjectedAttributionPort(t *testing.T) {
	const ns, proxyNS = "team-a", "platform-egress"
	share := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: proxyShareConfigMapName(proxyNS, "shared"), Namespace: ns},
		Data: map[string]string{proxyShareHostKey: "shared-proxy." + proxyNS + ".svc.cluster.local", proxySharePortKey: "8080",
			proxyShareProxyNSKey: proxyNS, proxyShareProxyNameKey: "shared"},
	}
	local := &v2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: ns}}

	for _, tc := range []struct {
		name     string
		ref      v2.ProxyObjectRef
		extra    []client.Object
		wantPort int
	}{
		{"own pool, not attributed", v2.ProxyObjectRef{Name: "local"}, nil, 8080},
		{"own pool, attributed", v2.ProxyObjectRef{Name: "local"}, []client.Object{attributionProjection(ns, ns, "local", "8101")}, 8101},
		{"shared pool, not attributed", v2.ProxyObjectRef{Name: "shared", Namespace: proxyNS}, nil, 8080},
		{"shared pool, attributed", v2.ProxyObjectRef{Name: "shared", Namespace: proxyNS}, []client.Object{attributionProjection(ns, proxyNS, "shared", "8102")}, 8102},
		{"another pool's projection", v2.ProxyObjectRef{Name: "local"}, []client.Object{attributionProjection(ns, proxyNS, "shared", "8102")}, 8080},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := tc.ref
			rs := rsObj("set", ns, func(rs *v2.RunnerSet) { rs.Spec.ProxyRef = &ref })
			objs := append([]client.Object{gwObj("gw", ns, ""), tmplObj("tmpl", ns), share, local, rs}, tc.extra...)
			c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(objs...).Build()

			refs, res := resolveRunnerSetRefs(context.Background(), c, c, rs)
			require.True(t, res.resolved(), "%+v", res)
			assert.Equal(t, tc.wantPort, refs.proxy.port)
		})
	}
}

func TestResolveRunnerSetRefs_RejectsABadAttributionPort(t *testing.T) {
	for port, want := range map[string]string{"eighty": "unparseable", "8080": "outside", "8133": "outside"} {
		t.Run(port, func(t *testing.T) {
			const ns = "team-a"
			rs := rsObj("set", ns, func(rs *v2.RunnerSet) { rs.Spec.ProxyRef = &v2.ProxyObjectRef{Name: "local"} })
			c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(
				gwObj("gw", ns, ""), tmplObj("tmpl", ns), rs,
				&v2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: ns}},
				attributionProjection(ns, ns, "local", port),
			).Build()

			_, res := resolveRunnerSetRefs(context.Background(), c, c, rs)
			require.Error(t, res.err)
			assert.Contains(t, res.err.Error(), want)
		})
	}
}

// ("a-b", "c") and ("a", "b-c") derive one projection name, so the AGC accepts a
// projection only when its identity keys name the pool it is resolving: a set
// referencing one pool never takes the other's proxy or port (Q1323).
func TestResolveRunnerSetRefs_RefusesAProjectionNamingACollidingPool(t *testing.T) {
	const ns = "team-a"
	require.Equal(t, proxyShareConfigMapName("a-b", "c"), proxyShareConfigMapName("a", "b-c"), "the premise: the two names collide")
	other := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: proxyShareConfigMapName("a", "b-c"), Namespace: ns},
		Data: map[string]string{proxyShareHostKey: "c-proxy.a-b.svc.cluster.local", proxySharePortKey: "8080",
			proxyShareProxyNSKey: "a-b", proxyShareProxyNameKey: "c"},
	}
	rs := rsObj("set", ns, func(rs *v2.RunnerSet) { rs.Spec.ProxyRef = &v2.ProxyObjectRef{Name: "b-c", Namespace: "a"} })
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(
		gwObj("gw", ns, ""), tmplObj("tmpl", ns), rs, other,
	).Build()

	_, res := resolveRunnerSetRefs(context.Background(), c, c, rs)
	require.False(t, res.resolved(), "a projection naming a-b/c must not resolve a reference to a/b-c")
	assert.Equal(t, v2.ReasonProxyShareNotGranted, res.reason)
	assert.Contains(t, res.message, "a-b/c")

	t.Run("attribution port", func(t *testing.T) {
		mine := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: proxyShareConfigMapName("a", "b-c"), Namespace: ns},
			Data: map[string]string{proxyShareHostKey: "b-c-proxy.a.svc.cluster.local", proxySharePortKey: "8080",
				proxyShareProxyNSKey: "a", proxyShareProxyNameKey: "b-c"},
		}
		otherPort := attributionProjection(ns, "a-b", "c", "8105")
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(
			gwObj("gw", ns, ""), tmplObj("tmpl", ns), rs, mine, otherPort,
		).Build()
		refs, res := resolveRunnerSetRefs(context.Background(), c, c, rs)
		require.True(t, res.resolved(), "%+v", res)
		assert.Equal(t, 8080, refs.proxy.port, "a-b/c's attributed port must not move a/b-c's workers")
	})
}
