package v2alpha1

import (
	"context"
	"errors"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/validation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeReader returns a client.Reader preloaded with the given objects, standing in
// for the manager's uncached API reader in the Q322 referrer-graph guards.
func fakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv2alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// failingReader errors every read, for the fail-closed paths of the Q322 guards.
type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("apiserver unavailable")
}

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("apiserver unavailable")
}

// v2Gateway builds a minimal v2 ActionsGateway bound to the given GitHub URL, with
// an optional defaultProxyRef.
func v2Gateway(namespace, name, gitHubURL, defaultProxy string) *agcv2alpha1.ActionsGateway {
	gw := &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
	if defaultProxy != "" {
		gw.Spec.DefaultProxyRef = &agcv2alpha1.ProxyObjectRef{Name: defaultProxy}
	}
	return gw
}

// proxyWithNoProxy builds an EgressProxy carrying only noProxyCIDRs.
func proxyWithNoProxy(namespace, name string, entries ...string) *agcv2alpha1.EgressProxy {
	return &agcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.EgressProxySpec{NoProxyCIDRs: entries},
	}
}

// TestV2ActionsGatewayWebhook_RejectsGHESHostExcludedByDefaultProxy is the referrer
// side of the Q322 guard: creating (or re-pointing) a gateway whose gitHubURL host —
// here a GitHub Enterprise Server host — falls in its defaultProxyRef proxy's
// noProxyCIDRs must be rejected, or the pair would route GHES traffic around the
// per-tenant proxy.
func TestV2ActionsGatewayWebhook_RejectsGHESHostExcludedByDefaultProxy(t *testing.T) {
	ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
	v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep)}

	gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep")
	_, err := v.ValidateCreate(context.Background(), gw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.defaultProxyRef")
	assert.Contains(t, err.Error(), "ghes.corp.example")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy")

	// The same pair must be rejected when assembled by an update (defaultProxyRef
	// is mutable even though gitHubURL is not).
	_, err = v.ValidateUpdate(context.Background(), v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", ""), gw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.defaultProxyRef")
}

// TestV2ActionsGatewayWebhook_RejectsGHESHostExcludedByRunnerSetProxy closes the
// create-order gap: a RunnerSet + EgressProxy pair naming a not-yet-applied gateway
// admits unchecked on both sides (§H.7), so the arriving gateway is the first object
// that can see the conflict — its admission must check the proxies bound via its
// RunnerSets, not just its own defaultProxyRef.
func TestV2ActionsGatewayWebhook_RejectsGHESHostExcludedByRunnerSetProxy(t *testing.T) {
	ep := proxyWithNoProxy("team-a", "ep", ".corp.example")
	rs := classicRS("rs", "team-a", "gw", "linux")
	rs.Spec.ProxyRef = &agcv2alpha1.ProxyObjectRef{Name: "ep"}
	v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep, rs)}

	_, err := v.ValidateCreate(context.Background(), v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `RunnerSet "rs"`)
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy")
}

// TestV2ActionsGatewayWebhook_AdmitsCompatibleProxyPairs asserts the guard is
// surgical: a missing proxy admits (referential integrity is a runtime condition,
// §H.7 — the proxy's own admission checks the pair when it arrives), as do proxies
// whose noProxyCIDRs are internal-only, gateways with no proxy bound at all, and a
// RunnerSet bound to a DIFFERENT gateway.
func TestV2ActionsGatewayWebhook_AdmitsCompatibleProxyPairs(t *testing.T) {
	ctx := context.Background()

	t.Run("missing proxy admits", func(t *testing.T) {
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t)}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep"))
		require.NoError(t, err)
	})

	t.Run("internal-only noProxyCIDRs admit", func(t *testing.T) {
		ep := proxyWithNoProxy("team-a", "ep", "10.0.0.0/8", "svc.cluster.local", "internal.example.com")
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep)}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep"))
		require.NoError(t, err)
	})

	t.Run("no proxy bound admits", func(t *testing.T) {
		ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep)}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", ""))
		require.NoError(t, err)
	})

	t.Run("RunnerSet under another gateway does not bind this host", func(t *testing.T) {
		ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
		rs := classicRS("rs", "team-a", "other-gw", "linux")
		rs.Spec.ProxyRef = &agcv2alpha1.ProxyObjectRef{Name: "ep"}
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep, rs)}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", ""))
		require.NoError(t, err)
	})

	t.Run("nil reader skips the check", func(t *testing.T) {
		v := &ActionsGatewayCustomValidator{}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep"))
		require.NoError(t, err)
	})
}

// TestV2ActionsGatewayWebhook_FailsClosedOnReadError asserts an unverifiable
// gitHubURL/noProxyCIDRs pair is rejected, not admitted on faith.
func TestV2ActionsGatewayWebhook_FailsClosedOnReadError(t *testing.T) {
	v := &ActionsGatewayCustomValidator{reader: failingReader{}}
	_, err := v.ValidateCreate(context.Background(), v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot verify")
}

// TestV2ActionsGatewayWebhook_RejectsReservedNamespaces is the v2 half of the v1
// reserved-namespace guard (Q323): a gateway makes the GMC provision an AGC control
// plane into its namespace, so creation in kube-system/kube-public, the default
// install namespace, or the POD_NAMESPACE-derived install namespace must be denied.
func TestV2ActionsGatewayWebhook_RejectsReservedNamespaces(t *testing.T) {
	ctx := context.Background()
	v := &ActionsGatewayCustomValidator{reservedNamespaces: validation.ReservedNamespaces("gag-operator")}

	for _, ns := range []string{"kube-system", "kube-public", "gmc-system", "gag-operator"} {
		_, err := v.ValidateCreate(ctx, v2Gateway(ns, "gw", "https://github.com/example-org", ""))
		require.Error(t, err, "namespace %q must be reserved", ns)
		assert.Contains(t, err.Error(), "reserved namespace")
	}

	// A tenant namespace is unaffected.
	_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://github.com/example-org", ""))
	require.NoError(t, err)
}

// TestV2ActionsGatewayWebhook_RejectsMalformedGitHubURL is the v2 half of the v1
// structural gitHubURL check (Q323): the CRD Pattern only guards the https scheme,
// so the webhook must reject a URL with no host or no org/enterprise/owner path
// segment — on update too (version-agnostic defense; the CRD immutability CEL
// normally makes the update path unreachable).
func TestV2ActionsGatewayWebhook_RejectsMalformedGitHubURL(t *testing.T) {
	ctx := context.Background()
	v := &ActionsGatewayCustomValidator{}

	cases := []struct {
		name, url, wantErr string
	}{
		{"empty", "", "gitHubURL is required"},
		{"http scheme", "http://github.com/example-org", "https scheme"},
		{"no host", "https:///example-org", "must include a host"},
		{"no org segment", "https://github.com", "path segment"},
		{"slash-only path", "https://github.com/", "path segment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := v2Gateway("team-a", "gw", tc.url, "")
			_, err := v.ValidateCreate(ctx, gw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			_, err = v.ValidateUpdate(ctx, v2Gateway("team-a", "gw", "https://github.com/example-org", ""), gw)
			require.Error(t, err, "update must apply the same structural check")
		})
	}

	// Well-formed org and owner/repo URLs are admitted on both verbs.
	for _, u := range []string{"https://github.com/example-org", "https://ghes.corp.example/owner/repo"} {
		gw := v2Gateway("team-a", "gw", u, "")
		_, err := v.ValidateCreate(ctx, gw)
		require.NoError(t, err)
		_, err = v.ValidateUpdate(ctx, gw, gw)
		require.NoError(t, err)
	}
}

// TestV2ActionsGatewayWebhook_ScaleSetLabelScope is the gateway corner of the Q791
// guard. A RunnerSet applied before its gateway has no resolvable GitHub scope and
// admits unchecked (§H.7), so without this half the guard is bypassable by apply
// order: the arriving gateway is the first object that can see the conflict.
func TestV2ActionsGatewayWebhook_ScaleSetLabelScope(t *testing.T) {
	ctx := context.Background()

	t.Run("gateway create closes the apply-order gap", func(t *testing.T) {
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t,
			v2Gateway("tenant-a", "gw-a", "https://github.com/acme", ""),
			scaleSetRS("held", "tenant-a", "gw-a", "linux"),
			// Applied before its gateway existed, so it was admitted unchecked.
			scaleSetRS("sneaky", "tenant-b", "gw-b", "linux"))}

		_, err := v.ValidateCreate(ctx, v2Gateway("tenant-b", "gw-b", "https://github.com/acme", ""))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.githubURL")
		assert.Contains(t, err.Error(), "github.com/acme")
		assert.Contains(t, err.Error(), "sneaky", "the referrer in this gateway's own namespace is named")
		assert.NotContains(t, err.Error(), "held", "the other tenant's set is not disclosed")
		assert.NotContains(t, err.Error(), "tenant-a")
	})

	t.Run("a different org admits the same referrer label", func(t *testing.T) {
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t,
			v2Gateway("tenant-a", "gw-a", "https://github.com/acme", ""),
			scaleSetRS("held", "tenant-a", "gw-a", "linux"),
			scaleSetRS("mine", "tenant-b", "gw-b", "linux"))}

		_, err := v.ValidateCreate(ctx, v2Gateway("tenant-b", "gw-b", "https://github.com/other", ""))
		require.NoError(t, err)
	})

	t.Run("a collision between unrelated sets is not this gateway's to reject", func(t *testing.T) {
		// Two sets elsewhere already share a name; a gateway with no referrer of its
		// own must not inherit their conflict.
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t,
			v2Gateway("tenant-a", "gw-a", "https://github.com/acme", ""),
			v2Gateway("tenant-b", "gw-b", "https://github.com/acme", ""),
			scaleSetRS("one", "tenant-a", "gw-a", "linux"),
			scaleSetRS("two", "tenant-b", "gw-b", "linux"))}

		_, err := v.ValidateCreate(ctx, v2Gateway("tenant-c", "gw-c", "https://github.com/acme", ""))
		require.NoError(t, err)
	})

	t.Run("a gateway with no ScaleSet referrers admits", func(t *testing.T) {
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t,
			v2Gateway("tenant-a", "gw-a", "https://github.com/acme", ""),
			scaleSetRS("held", "tenant-a", "gw-a", "linux"),
			classicRS("classic", "tenant-b", "gw-b", "linux"))}

		_, err := v.ValidateCreate(ctx, v2Gateway("tenant-b", "gw-b", "https://github.com/acme", ""))
		require.NoError(t, err)
	})

	t.Run("update re-checks", func(t *testing.T) {
		gw := v2Gateway("tenant-b", "gw-b", "https://github.com/acme", "")
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t,
			v2Gateway("tenant-a", "gw-a", "https://github.com/acme", ""),
			scaleSetRS("held", "tenant-a", "gw-a", "linux"),
			scaleSetRS("mine", "tenant-b", "gw-b", "linux"), gw)}

		_, err := v.ValidateUpdate(ctx, gw, gw)
		require.Error(t, err)
	})

	t.Run("read error fails closed", func(t *testing.T) {
		v := &ActionsGatewayCustomValidator{reader: failingReader{}}
		_, err := v.ValidateCreate(ctx, v2Gateway("tenant-b", "gw-b", "https://github.com/acme", ""))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot verify")
	})
}
