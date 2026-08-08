package v2alpha1

import (
	"context"
	"net"
	"strings"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
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

func newEgressProxy(namespace, name string, fqdns, cidrs []string) *agcv2alpha1.EgressProxy {
	return &agcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.EgressProxySpec{
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
		spec            *agcv2alpha1.EgressProxySpec
		list            *allowlist.EgressDestinationAllowlist
		wantErr         bool
		wantErrContains string
	}{
		{
			name: "no extra destinations admitted",
			spec: &agcv2alpha1.EgressProxySpec{},
			list: list,
		},
		{
			name: "allowlisted FQDN admitted",
			spec: &agcv2alpha1.EgressProxySpec{DestinationFQDNs: []string{"proxy.golang.org"}},
			list: list,
		},
		{
			name:            "off-allowlist FQDN rejected",
			spec:            &agcv2alpha1.EgressProxySpec{DestinationFQDNs: []string{"evil.example.com"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "destinationFQDNs",
		},
		{
			name: "allowlisted CIDR admitted",
			spec: &agcv2alpha1.EgressProxySpec{DestinationCIDRs: []string{"10.1.0.0/16"}},
			list: list,
		},
		{
			name:            "off-allowlist CIDR rejected",
			spec:            &agcv2alpha1.EgressProxySpec{DestinationCIDRs: []string{"192.168.0.0/16"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "destinationCIDRs",
		},
		{
			name:            "malformed CIDR rejected as defense in depth",
			spec:            &agcv2alpha1.EgressProxySpec{DestinationCIDRs: []string{"not-a-cidr"}},
			list:            list,
			wantErr:         true,
			wantErrContains: "not a valid CIDR",
		},
		{
			name:            "nil allowlist denies everything (secure default)",
			spec:            &agcv2alpha1.EgressProxySpec{DestinationFQDNs: []string{"golang.org"}},
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
func epWithNoProxy(entries ...string) *agcv2alpha1.EgressProxy {
	return &agcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "team-a"},
		Spec:       agcv2alpha1.EgressProxySpec{NoProxyCIDRs: entries},
	}
}

// TestEgressProxyCustomValidator_RejectsGitHubHostInNoProxyCIDRs is the v2 port of
// the v1 ActionsGateway guard: a spec.noProxyCIDRs entry that NO_PROXY-matches a
// public GitHub host would route GitHub traffic around the per-tenant egress proxy
// and must be rejected. (The EgressProxy has no gitHubURL, so a referrer's GHES host
// is not covered — Q322.)
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
		rs := classicRS("rs", "team-a", "gw", "linux")
		rs.Spec.ProxyRef = &agcv2alpha1.ProxyObjectRef{Name: "ep"}
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
// hostname entry is rejected (fail closed) — while an all-CIDR/IP list never reads
// the API at all, so it is admitted even when the reader is down.
func TestEgressProxyCustomValidator_ReferrerCheckFailsClosed(t *testing.T) {
	v := &EgressProxyCustomValidator{reader: failingReader{}}

	_, err := v.ValidateCreate(context.Background(), epWithNoProxy("internal.example.com"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot verify")

	_, err = v.ValidateCreate(context.Background(), epWithNoProxy("10.0.0.0/8", "203.0.113.5"))
	require.NoError(t, err, "CIDR/IP-only entries cannot suffix-match a host, so no referrer read is needed")
}

// epWithMode builds a minimal EgressProxy carrying only an egressPolicyMode, for the
// Q245 intent/backend admission cases.
func epWithMode(mode agcv2alpha1.EgressPolicyMode) *agcv2alpha1.EgressProxy {
	return &agcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "team-a"},
		Spec:       agcv2alpha1.EgressProxySpec{EgressPolicyMode: mode},
	}
}

// TestEgressProxyCustomValidator_FQDNBackend covers the Q245 intent/backend split at
// admission: FQDN intent is rejected when the cluster declares no backend (fail-closed
// and loud), admitted when a backend is configured, and CIDR/deprecated modes are never
// gated by the backend.
func TestEgressProxyCustomValidator_FQDNBackend(t *testing.T) {
	list := allowlist.NewEgressDestination(nil, nil)

	cases := []struct {
		name    string
		mode    agcv2alpha1.EgressPolicyMode
		backend controller.FQDNBackend
		wantErr bool
	}{
		{"FQDN + none rejected", agcv2alpha1.EgressPolicyModeFQDN, controller.FQDNBackendNone, true},
		{"FQDN + empty (zero value) rejected", agcv2alpha1.EgressPolicyModeFQDN, "", true},
		{"FQDN + cilium admitted", agcv2alpha1.EgressPolicyModeFQDN, controller.FQDNBackendCilium, false},
		{"FQDN + gke admitted", agcv2alpha1.EgressPolicyModeFQDN, controller.FQDNBackendGKE, false},
		{"CIDR + none admitted", agcv2alpha1.EgressPolicyModeCIDR, controller.FQDNBackendNone, false},
		{"deprecated Cilium + none admitted", agcv2alpha1.EgressPolicyModeCiliumFQDN, controller.FQDNBackendNone, false},
		{"deprecated Calico + none admitted", agcv2alpha1.EgressPolicyModeCalicoFQDN, controller.FQDNBackendNone, false},
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

// TestEgressProxyCustomValidator_DeprecationWarnings asserts the deprecated CNI-specific
// modes are admitted with a non-blocking warning that names the removal release, while
// FQDN/CIDR emit none. The release is part of the contract, not decoration: an operator
// plans the migration from it, and v2beta1's beta contract puts it at v3.0.0 rather than
// the v2.0.0 that removes v1alpha1/v2alpha1/classic (Q428).
func TestEgressProxyCustomValidator_DeprecationWarnings(t *testing.T) {
	v := &EgressProxyCustomValidator{
		Allowlist:   allowlist.NewEgressDestination(nil, nil),
		FQDNBackend: controller.FQDNBackendCilium,
	}

	cases := []struct {
		mode      agcv2alpha1.EgressPolicyMode
		wantWarn  bool
		wantToken string
	}{
		{agcv2alpha1.EgressPolicyModeCiliumFQDN, true, "CiliumFQDN is deprecated"},
		{agcv2alpha1.EgressPolicyModeCalicoFQDN, true, "CalicoFQDN is deprecated"},
		{agcv2alpha1.EgressPolicyModeFQDN, false, ""},
		{agcv2alpha1.EgressPolicyModeCIDR, false, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			warns, err := v.ValidateCreate(context.Background(), epWithMode(tc.mode))
			require.NoError(t, err)
			if !tc.wantWarn {
				assert.Empty(t, warns)
				return
			}
			require.Len(t, warns, 1)
			assert.Contains(t, warns[0], tc.wantToken)
			assert.Contains(t, warns[0], "v3.0.0",
				"the warning must name the removal release so an operator can plan the migration")
		})
	}
}
