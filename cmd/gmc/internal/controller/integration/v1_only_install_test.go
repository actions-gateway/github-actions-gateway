//go:build integration

package integration_test

import (
	"net"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// TestV1OnlyInstall_GMCComesUpClean is the acceptance test for Q228: a GMC
// pointed at a cluster with ONLY the v1alpha1 CRDs installed — the main
// actions-gateway chart WITHOUT the opt-in actions-gateway-crds-v2 chart — must
// come up clean. The actions-gateway.com/v2alpha1 CRDs are absent, so:
//
//   - V2alpha1Installed reports false (so main() skips the v2 controllers and
//     their source.Kind retry loops),
//   - the IPRange reconciler's v2 passes are disabled (V2Enabled=false), so it
//     does not list EgressProxy/v2 ActionsGateway and never logs "no matches for
//     kind", and
//   - v1alpha1 NetworkPolicies still refresh.
//
// It runs against its own envtest with the v2 CRD path omitted, separate from
// the shared suite (which installs all CRDs).
func TestV1OnlyInstall_GMCComesUpClean(t *testing.T) {
	v1Env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			"../../../config/crd/bases",
			"../../../../agc/config/crd",
			// Deliberately NO "../../../../../api/config/crd": omitting the five v2
			// (actions-gateway.com/v2alpha1) CRDs reproduces a v1-only install.
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
	}
	cfg, err := v1Env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = v1Env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)

	// A manager gives us the same dynamic RESTMapper production uses; it need not
	// be started to resolve REST mappings.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	require.NoError(t, err)

	// Detection: v2 CRDs absent ⇒ false, with no error.
	installed, err := controller.V2alpha1Installed(mgr.GetRESTMapper())
	require.NoError(t, err)
	require.False(t, installed, "expected v2alpha1 CRDs to be reported absent on a v1-only install")

	// Document the failure mode the gate avoids: listing a v2 kind against this
	// apiserver returns a NoMatch — exactly what the IPRange v2 passes would hit
	// every tick if they were not gated.
	var eps gmcv2alpha1.EgressProxyList
	listErr := c.List(ctx, &eps)
	require.Error(t, listErr)
	require.True(t, meta.IsNoMatchError(listErr), "expected NoMatch listing EgressProxy on a v1-only install, got: %v", listErr)

	// v1alpha1 still reconciles: with V2Enabled=false the IPRange reconciler
	// refreshes a v1 proxy NetworkPolicy and skips the v2 passes entirely (no
	// error, no NoMatch). Seed a CIDR via the stub fetcher and assert it lands in
	// the proxy NetworkPolicy's egress. All objects are created via the v1-only
	// client c (the shared-suite helpers target a different apiserver).
	const ns = "v1-only-tenant"
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "github-app"},
			GitHubURL:    "https://github.com/example-org",
		},
	}
	require.NoError(t, c.Create(ctx, ag))

	// Pre-create the proxy NetworkPolicy the per-CR reconciler would own, with an
	// empty egress the IPRange refresh pass will overwrite.
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: proxyName, Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	require.NoError(t, c.Create(ctx, np))

	const githubCIDR = "192.0.2.0/24"
	_, ipNet, err := net.ParseCIDR(githubCIDR)
	require.NoError(t, err)

	rec := &controller.IPRangeReconciler{
		Client:    c,
		Fetcher:   &stubIPFetcher{cidrs: []net.IPNet{*ipNet}},
		Cache:     &controller.IPRangeCache{},
		V2Enabled: false,
	}
	rec.ReconcileNow(ctx)

	var got networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyName}, &got))

	var found bool
	for _, rule := range got.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == githubCIDR {
				found = true
			}
		}
	}
	require.True(t, found, "expected the v1 proxy NetworkPolicy egress to carry the GitHub CIDR after a V2-disabled refresh; egress=%+v", got.Spec.Egress)
}
