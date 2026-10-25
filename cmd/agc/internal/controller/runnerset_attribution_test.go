package controller

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
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
		Data:       map[string]string{proxyAttributionPortKey: port},
	}
}

// A worker moves onto the port the GMC projected for its namespace, from a pool in
// its own namespace and from a shared one alike, and stays on the unattributed
// port when nothing is projected.
func TestResolveRunnerSetRefs_UsesTheProjectedAttributionPort(t *testing.T) {
	const ns, proxyNS = "team-a", "platform-egress"
	share := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: proxyShareConfigMapName(proxyNS, "shared"), Namespace: ns},
		Data:       map[string]string{proxyShareHostKey: "shared-proxy." + proxyNS + ".svc.cluster.local", proxySharePortKey: "8080"},
	}
	local := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: ns}}

	for _, tc := range []struct {
		name     string
		ref      v2alpha1.ProxyObjectRef
		extra    []client.Object
		wantPort int
	}{
		{"own pool, not attributed", v2alpha1.ProxyObjectRef{Name: "local"}, nil, 8080},
		{"own pool, attributed", v2alpha1.ProxyObjectRef{Name: "local"}, []client.Object{attributionProjection(ns, ns, "local", "8101")}, 8101},
		{"shared pool, not attributed", v2alpha1.ProxyObjectRef{Name: "shared", Namespace: proxyNS}, nil, 8080},
		{"shared pool, attributed", v2alpha1.ProxyObjectRef{Name: "shared", Namespace: proxyNS}, []client.Object{attributionProjection(ns, proxyNS, "shared", "8102")}, 8102},
		{"another pool's projection", v2alpha1.ProxyObjectRef{Name: "local"}, []client.Object{attributionProjection(ns, proxyNS, "shared", "8102")}, 8080},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := tc.ref
			rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) { rs.Spec.ProxyRef = &ref })
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
			rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) { rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "local"} })
			c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(
				gwObj("gw", ns, ""), tmplObj("tmpl", ns), rs,
				&v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: ns}},
				attributionProjection(ns, ns, "local", port),
			).Build()

			_, res := resolveRunnerSetRefs(context.Background(), c, c, rs)
			require.Error(t, res.err)
			assert.Contains(t, res.err.Error(), want)
		})
	}
}
