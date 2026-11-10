package v2

import (
	"context"
	"net"
	"strings"
	"testing"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/validation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mustCIDR parses s as a CIDR, failing the test on error. Test helper only.
func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	require.NoError(t, err)
	return n
}

func newEgressProxy(namespace, name string, fqdns, cidrs []string) *agcv2.EgressProxy {
	return &agcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2.EgressProxySpec{
			DestinationFQDNs: fqdns,
			DestinationCIDRs: cidrs,
		},
	}
}

func TestValidateEgressDestinations(t *testing.T) {
	list := allowlist.NewEgressDestination(
		[]string{"golang.org"},
		[]*net.IPNet{mustCIDR(t, "10.0.0.0/8")},
	)

	tests := []struct {
		name            string
		spec            *agcv2.EgressProxySpec
		list            *allowlist.EgressDestinationAllowlist
		wantErr         bool
		wantErrContains string
	}{
		{
			name: "no extra destinations admitted",
			spec: &agcv2.EgressProxySpec{},
			list: list,
		},
		{
			name: "allowlisted FQDN admitted",
			spec: &agcv2.EgressProxySpec{DestinationFQDNs: []string{"proxy.golang.org"}},
			list: list,
		},
		{
			name:            "off-allowlist FQDN rejected",
			spec:            &agcv2.EgressProxySpec{DestinationFQDNs: []string{"evil.example.com"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "destinationFQDNs",
		},
		{
			name: "allowlisted CIDR admitted",
			spec: &agcv2.EgressProxySpec{DestinationCIDRs: []string{"10.1.0.0/16"}},
			list: list,
		},
		{
			name:            "off-allowlist CIDR rejected",
			spec:            &agcv2.EgressProxySpec{DestinationCIDRs: []string{"192.168.0.0/16"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "destinationCIDRs",
		},
		{
			name:            "malformed CIDR rejected as defense in depth",
			spec:            &agcv2.EgressProxySpec{DestinationCIDRs: []string{"not-a-cidr"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "not a valid CIDR",
		},
		{
			name:            "nil allowlist denies everything (secure default)",
			spec:            &agcv2.EgressProxySpec{DestinationFQDNs: []string{"golang.org"}},
			list:            nil,
			wantErr:         true,
			wantErrContains: "destinationFQDNs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEgressDestinations(tc.spec, tc.list)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrContains)
		})
	}
}

func TestEgressProxyCustomValidator_ValidateCreate(t *testing.T) {
	list := allowlist.NewEgressDestination([]string{"golang.org"}, nil)
	v := &EgressProxyCustomValidator{Allowlist: list}

	t.Run("valid destination admitted", func(t *testing.T) {
		_, err := v.ValidateCreate(context.Background(), newEgressProxy("team-a", "ep", []string{"golang.org"}, nil))
		require.NoError(t, err)
	})

	t.Run("invalid destination rejected and audited", func(t *testing.T) {
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateCreate(ctx, newEgressProxy("team-a", "ep", []string{"evil.example.com"}, nil))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not permitted")

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "admission denied")
		assert.Contains(t, joined, "create")
		assert.Contains(t, joined, "team-a")
		assert.Contains(t, joined, "ep")
	})
}

// TestEgressProxyCustomValidator_RejectsReservedNamespaces is the EgressProxy half
// of the Q323 reserved-namespace guard: the CR makes the GMC provision a proxy
// Deployment and NetworkPolicies into its namespace, so creation in
// kube-system/kube-public, the default install namespace, or the
// POD_NAMESPACE-derived install namespace must be denied. Create-only: updates to a
// (hypothetical) pre-existing object are not bricked, matching the v1 gateway guard.
func TestEgressProxyCustomValidator_RejectsReservedNamespaces(t *testing.T) {
	ctx := context.Background()
	v := &EgressProxyCustomValidator{reservedNamespaces: validation.ReservedNamespaces("gag-operator")}

	for _, ns := range []string{"kube-system", "kube-public", "gmc-system", "gag-operator"} {
		_, err := v.ValidateCreate(ctx, newEgressProxy(ns, "ep", nil, nil))
		require.Error(t, err, "namespace %q must be reserved", ns)
		assert.Contains(t, err.Error(), "reserved namespace")
	}

	// A tenant namespace is unaffected, and update never applies the guard.
	_, err := v.ValidateCreate(ctx, newEgressProxy("team-a", "ep", nil, nil))
	require.NoError(t, err)
	_, err = v.ValidateUpdate(ctx, newEgressProxy("kube-system", "ep", nil, nil), newEgressProxy("kube-system", "ep", nil, nil))
	require.NoError(t, err)
}

func TestEgressProxyCustomValidator_ValidateUpdate(t *testing.T) {
	list := allowlist.NewEgressDestination([]string{"golang.org"}, nil)
	v := &EgressProxyCustomValidator{Allowlist: list}
	oldObj := newEgressProxy("team-a", "ep", nil, nil)

	t.Run("widening to a valid destination admitted", func(t *testing.T) {
		newObj := newEgressProxy("team-a", "ep", []string{"golang.org"}, nil)
		_, err := v.ValidateUpdate(context.Background(), oldObj, newObj)
		require.NoError(t, err)
	})

	t.Run("widening to an invalid destination rejected and audited", func(t *testing.T) {
		newObj := newEgressProxy("team-a", "ep", []string{"evil.example.com"}, nil)
		ctx, lines := ctxWithCapture()
		_, err := v.ValidateUpdate(ctx, oldObj, newObj)
		require.Error(t, err)

		joined := strings.Join(*lines, "\n")
		assert.Contains(t, joined, "admission denied")
		assert.Contains(t, joined, "update")
	})
}

func TestEgressProxyCustomValidator_ValidateDelete(t *testing.T) {
	v := &EgressProxyCustomValidator{Allowlist: allowlist.NewEgressDestination(nil, nil)}
	_, err := v.ValidateDelete(context.Background(), newEgressProxy("team-a", "ep", []string{"anything-goes.example.com"}, nil))
	require.NoError(t, err, "delete is a no-op regardless of allowlist state")
}

// epWithNoProxy builds a minimal EgressProxy carrying only noProxyCIDRs, for the
// GitHub proxy-bypass admission cases.
func epWithNoProxy(entries ...string) *agcv2.EgressProxy {
	return &agcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "team-a"},
		Spec:       agcv2.EgressProxySpec{NoProxyCIDRs: entries},
	}
}

// TestEgressProxyCustomValidator_RejectsGitHubHostInNoProxyCIDRs is the v2 port of
// the v1 ActionsGateway guard: a spec.noProxyCIDRs entry that NO_PROXY-matches a
// public GitHub host would route GitHub traffic around the per-tenant egress proxy
// and must be rejected.
func TestEgressProxyCustomValidator_RejectsGitHubHostInNoProxyCIDRs(t *testing.T) {
	v := &EgressProxyCustomValidator{Allowlist: allowlist.NewEgressDestination(nil, nil)}
	for _, entry := range []string{"github.com", ".github.com", "api.github.com", "githubusercontent.com", "ghcr.io", ".com"} {
		_, err := v.ValidateCreate(context.Background(), epWithNoProxy(entry))
		require.Errorf(t, err, "entry %q should be rejected", entry)
		assert.Contains(t, err.Error(), "spec.noProxyCIDRs[0]")
		assert.Contains(t, err.Error(), "around the per-tenant egress proxy")
	}
}

// TestEgressProxyCustomValidator_AllowsNonGitHubNoProxyEntries asserts the guard is
// surgical: CIDRs, bare IPs, and non-GitHub domain suffixes (the supported
// internal-destination pattern) are all admitted, as is an empty list.
func TestEgressProxyCustomValidator_AllowsNonGitHubNoProxyEntries(t *testing.T) {
	v := &EgressProxyCustomValidator{Allowlist: allowlist.NewEgressDestination(nil, nil)}

	_, err := v.ValidateCreate(context.Background(), epWithNoProxy(
		"10.0.0.0/8", "203.0.113.5/32", "fd00::/8", // CIDRs
		"10.0.0.5",                       // bare IP
		"svc.cluster.local", "localhost", // cluster-internal domain suffixes
		"internal.example.com", // a non-GitHub internal domain
	))
	require.NoError(t, err)

	_, err = v.ValidateCreate(context.Background(), epWithNoProxy())
	require.NoError(t, err)
}

// TestEgressProxyCustomValidator_UpdateRejectsGitHubHostInNoProxyCIDRs asserts the
// guard also gates updates (adding a bypass to an existing proxy) and audits the
// rejection.
func TestEgressProxyCustomValidator_UpdateRejectsGitHubHostInNoProxyCIDRs(t *testing.T) {
	v := &EgressProxyCustomValidator{Allowlist: allowlist.NewEgressDestination(nil, nil)}
	ctx, lines := ctxWithCapture()
	_, err := v.ValidateUpdate(ctx, epWithNoProxy(), epWithNoProxy("10.0.0.0/8", "api.github.com"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.noProxyCIDRs[1]")

	joined := strings.Join(*lines, "\n")
	assert.Contains(t, joined, "admission denied")
	assert.Contains(t, joined, "update")
}

// TestEgressProxyCustomValidator_RejectsReferrerGHESHostInNoProxyCIDRs is the proxy
// side of the Q322 guard: the EgressProxy carries no gitHubURL, so the validator
// resolves its referrers and protects THEIR GitHub hosts — a noProxyCIDRs entry
// matching a referring gateway's GitHub Enterprise Server host must be rejected,
// whether the gateway refers via defaultProxyRef or a RunnerSet binds it via proxyRef.
func TestEgressProxyCustomValidator_RejectsReferrerGHESHostInNoProxyCIDRs(t *testing.T) {
	ctx := context.Background()

	t.Run("gateway defaultProxyRef referrer", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep")
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw)}
		for _, entry := range []string{"ghes.corp.example", ".corp.example", "corp.example"} {
			_, err := v.ValidateCreate(ctx, epWithNoProxy(entry))
			require.Errorf(t, err, "entry %q should be rejected against the referrer's GHES host", entry)
			assert.Contains(t, err.Error(), "ghes.corp.example")
			assert.Contains(t, err.Error(), `ActionsGateway "gw"`)
		}
	})

	t.Run("RunnerSet proxyRef referrer", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "")
		rs := labelledRS("rs", "team-a", "gw", "linux")
		rs.Spec.ProxyRef = &agcv2.ProxyObjectRef{Name: "ep"}
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw, rs)}
		_, err := v.ValidateCreate(ctx, epWithNoProxy("ghes.corp.example"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `RunnerSet "rs"`)
	})

	t.Run("update is gated too", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep")
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw)}
		_, err := v.ValidateUpdate(ctx, epWithNoProxy(), epWithNoProxy("10.0.0.0/8", "ghes.corp.example"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.noProxyCIDRs[1]")
	})
}

// TestEgressProxyCustomValidator_ReferrerCheckIsSurgical asserts the referrer half
// admits everything it should: a GHES-looking entry with no referrer binding that
// host, internal entries alongside a real referrer, and a gateway that references a
// different proxy.
func TestEgressProxyCustomValidator_ReferrerCheckIsSurgical(t *testing.T) {
	ctx := context.Background()

	t.Run("no referrer binds the host", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: fakeReader(t)}
		_, err := v.ValidateCreate(ctx, epWithNoProxy("ghes.corp.example"))
		require.NoError(t, err)
	})

	t.Run("gateway referencing a different proxy does not protect its host here", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "other-ep")
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw)}
		_, err := v.ValidateCreate(ctx, epWithNoProxy("ghes.corp.example"))
		require.NoError(t, err)
	})

	t.Run("internal entries admit alongside a live referrer", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep")
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw)}
		_, err := v.ValidateCreate(ctx, epWithNoProxy("10.0.0.0/8", "svc.cluster.local", "internal.example.com"))
		require.NoError(t, err)
	})
}

// TestEgressProxyCustomValidator_ReferrerCheckFailsClosed asserts an unverifiable
// entry is rejected (fail closed), an address entry included: it can cover a GitHub
// Enterprise Server a referrer addresses by IP. An empty list never reads the API,
// so it is admitted even when the reader is down.
func TestEgressProxyCustomValidator_ReferrerCheckFailsClosed(t *testing.T) {
	v := &EgressProxyCustomValidator{reader: failingReader{}}

	for _, entries := range [][]string{{"internal.example.com"}, {"10.0.0.0/8", "203.0.113.5"}} {
		_, err := v.ValidateCreate(context.Background(), epWithNoProxy(entries...))
		require.Errorf(t, err, "entries %v", entries)
		assert.Contains(t, err.Error(), "cannot verify")
	}

	_, err := v.ValidateCreate(context.Background(), epWithNoProxy())
	require.NoError(t, err, "no entry can match a host, so no referrer read is needed")
}

// TestEgressProxyCustomValidator_RejectsNoProxyFormsNetHTTPHonours asserts the
// webhook refuses the NO_PROXY forms that exempt a GitHub host without spelling it
// as a bare suffix, against the public hosts and against a referrer's host.
func TestEgressProxyCustomValidator_RejectsNoProxyFormsNetHTTPHonours(t *testing.T) {
	ctx := context.Background()

	t.Run("public GitHub hosts", func(t *testing.T) {
		v := &EgressProxyCustomValidator{}
		for _, entry := range []string{"x,github.com", "*", "*.github.com", "github.com:443", "ｇｉｔｈｕｂ.com"} {
			_, err := v.ValidateCreate(ctx, epWithNoProxy("10.0.0.0/8", entry))
			require.Errorf(t, err, "entry %q should be rejected", entry)
			assert.Contains(t, err.Error(), "spec.noProxyCIDRs[1]")
		}
	})

	t.Run("referrer GHES host", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep")
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw)}
		for _, entry := range []string{"x,corp.example", "*.corp.example", "ghes.corp.example:443"} {
			_, err := v.ValidateCreate(ctx, epWithNoProxy(entry))
			require.Errorf(t, err, "entry %q should be rejected", entry)
			assert.Contains(t, err.Error(), `ActionsGateway "gw"`)
		}
	})

	t.Run("referrer GHES addressed by IP", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://10.1.2.3/my-org", "ep")
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw)}
		for _, entry := range []string{"10.0.0.0/8", "10.1.2.3", "10.1.2.3:443"} {
			_, err := v.ValidateCreate(ctx, epWithNoProxy(entry))
			require.Errorf(t, err, "entry %q covers the referrer's GitHub address", entry)
			assert.Contains(t, err.Error(), "10.1.2.3")
		}
		_, err := v.ValidateCreate(ctx, epWithNoProxy("192.168.0.0/16", "10.1.2.4"))
		require.NoError(t, err, "an address entry that does not cover the referrer's address is admitted")
	})
}

// sharedProxy builds provider/shared granting the given namespaces.
func sharedProxy(entries []string, allowed ...string) *agcv2.EgressProxy {
	ep := proxyWithNoProxy("provider", "shared", entries...)
	if len(allowed) > 0 {
		ep.Spec.Sharing = &agcv2.ProxySharing{AllowedNamespaces: allowed}
	}
	return ep
}

// consumerGateway builds consumer/gw on a GHES host, naming provider/shared as its
// default proxy.
func consumerGateway() *agcv2.ActionsGateway {
	gw := v2Gateway("consumer", "gw", "https://ghes.corp.example/my-org", "")
	gw.Spec.DefaultProxyRef = &agcv2.ProxyObjectRef{Name: "shared", Namespace: "provider"}
	return gw
}

// TestEgressProxyCustomValidator_CrossNamespaceReferrers asserts a shared proxy's
// noProxyCIDRs are compared with the GitHub hosts of referrers in the namespaces it
// grants, and with no others.
func TestEgressProxyCustomValidator_CrossNamespaceReferrers(t *testing.T) {
	ctx := context.Background()
	entries := []string{"corp.example"}

	t.Run("granted gateway referrer rejects create and update", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: fakeReader(t, consumerGateway())}
		_, err := v.ValidateCreate(ctx, sharedProxy(entries, "consumer"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghes.corp.example")
		assert.Contains(t, err.Error(), `ActionsGateway "consumer/gw"`)

		_, err = v.ValidateUpdate(ctx, sharedProxy(nil, "consumer"), sharedProxy(entries, "consumer"))
		require.Error(t, err)
	})

	t.Run("granted RunnerSet referrer rejects", func(t *testing.T) {
		gw := v2Gateway("consumer", "gw", "https://ghes.corp.example/my-org", "")
		rs := labelledRS("rs", "consumer", "gw", "linux")
		rs.Spec.ProxyRef = &agcv2.ProxyObjectRef{Name: "shared", Namespace: "provider"}
		// A same-named gateway beside the proxy must not stand in for the set's own.
		decoy := v2Gateway("provider", "gw", "https://github.com/my-org", "")
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw, rs, decoy)}
		_, err := v.ValidateCreate(ctx, sharedProxy(entries, "consumer"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `RunnerSet "consumer/rs"`)
	})

	t.Run("ungranted referrer does not bind", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: fakeReader(t, consumerGateway())}
		_, err := v.ValidateCreate(ctx, sharedProxy(entries))
		require.NoError(t, err, "a proxy sharing nothing has no cross-namespace referrer")
		_, err = v.ValidateCreate(ctx, sharedProxy(entries, "someone-else"))
		require.NoError(t, err, "consumer is not granted, so its gateway binds nothing")
	})

	t.Run("a same-named proxy elsewhere is not this one", func(t *testing.T) {
		gw := v2Gateway("consumer", "gw", "https://ghes.corp.example/my-org", "shared") // consumer/shared
		v := &EgressProxyCustomValidator{reader: fakeReader(t, gw)}
		_, err := v.ValidateCreate(ctx, sharedProxy(entries, "consumer"))
		require.NoError(t, err)
	})
}

// TestEgressProxyCustomValidator_UpdateJudgesOnlyWhatItChanges asserts a proxy
// stored before the guard tightened still takes an unrelated edit, and that the
// edits which bind a new pair are judged.
func TestEgressProxyCustomValidator_UpdateJudgesOnlyWhatItChanges(t *testing.T) {
	ctx := context.Background()
	stored := []string{"*.github.com", "x,corp.example"}
	sameNSReferrer := v2Gateway("provider", "gw", "https://ghes.corp.example/my-org", "shared")

	t.Run("unchanged entries admit an unrelated edit without a read", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: failingReader{}}
		edited := sharedProxy(stored, "consumer")
		edited.Spec.DestinationCIDRs = nil
		edited.Labels = map[string]string{"edited": "true"}
		_, err := v.ValidateUpdate(ctx, sharedProxy(stored, "consumer"), edited)
		require.NoError(t, err)
	})

	t.Run("the same entries are refused on create", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: fakeReader(t)}
		_, err := v.ValidateCreate(ctx, sharedProxy(stored, "consumer"))
		require.Error(t, err)
	})

	t.Run("changing the list re-judges every entry", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: fakeReader(t)}
		_, err := v.ValidateUpdate(ctx, sharedProxy(stored), sharedProxy(append([]string{"10.0.0.0/8"}, stored...)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.noProxyCIDRs[1]")
	})

	t.Run("granting a namespace judges that namespace's referrers", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: fakeReader(t, sameNSReferrer, consumerGateway())}
		_, err := v.ValidateUpdate(ctx, sharedProxy(stored), sharedProxy(stored, "consumer"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `ActionsGateway "consumer/gw"`)
	})

	t.Run("granting a namespace does not re-judge stored pairs", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: fakeReader(t, sameNSReferrer, consumerGateway())}
		_, err := v.ValidateUpdate(ctx, sharedProxy(stored, "consumer"), sharedProxy(stored, "consumer", "empty-ns"))
		require.NoError(t, err, "the provider/gw and consumer/gw pairs were both stored before this write")
	})

	t.Run("revoking a grant binds nothing", func(t *testing.T) {
		v := &EgressProxyCustomValidator{reader: failingReader{}}
		_, err := v.ValidateUpdate(ctx, sharedProxy(stored, "consumer"), sharedProxy(stored))
		require.NoError(t, err)
	})
}

// epWithMode builds a minimal EgressProxy carrying only an egressPolicyMode, for the
// Q245 intent/backend admission cases.
func epWithMode(mode agcv2.EgressPolicyMode) *agcv2.EgressProxy {
	return &agcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "team-a"},
		Spec:       agcv2.EgressProxySpec{EgressPolicyMode: mode},
	}
}

// TestEgressProxyCustomValidator_FQDNBackend covers the Q245 intent/backend split at
// admission: FQDN intent is rejected when the cluster declares no backend (fail-closed
// and loud), admitted when a backend is configured, and CIDR is never gated by the
// backend.
func TestEgressProxyCustomValidator_FQDNBackend(t *testing.T) {
	list := allowlist.NewEgressDestination(nil, nil)

	cases := []struct {
		name    string
		mode    agcv2.EgressPolicyMode
		backend controller.FQDNBackend
		wantErr bool
	}{
		{name: "FQDN + none rejected", mode: agcv2.EgressPolicyModeFQDN, backend: controller.FQDNBackendNone, wantErr: true},
		{name: "FQDN + empty (zero value) rejected", mode: agcv2.EgressPolicyModeFQDN, backend: "", wantErr: true},
		{name: "FQDN + cilium admitted", mode: agcv2.EgressPolicyModeFQDN, backend: controller.FQDNBackendCilium},
		{name: "FQDN + gke admitted", mode: agcv2.EgressPolicyModeFQDN, backend: controller.FQDNBackendGKE},
		{name: "CIDR + none admitted", mode: agcv2.EgressPolicyModeCIDR, backend: controller.FQDNBackendNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &EgressProxyCustomValidator{Allowlist: list, FQDNBackend: tc.backend}
			_, err := v.ValidateCreate(context.Background(), epWithMode(tc.mode))
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "--fqdn-policy-backend")
				return
			}
			require.NoError(t, err)
		})
	}
}
