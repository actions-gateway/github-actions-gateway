package allowlist

import (
	"net"
	"testing"
)

// mustCIDRs parses CIDR strings for test setup, failing the test on a bad entry.
func mustCIDRs(t *testing.T, ss ...string) []*net.IPNet {
	t.Helper()
	out := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("parse CIDR %q: %v", s, err)
		}
		out = append(out, n)
	}
	return out
}

// mustCIDR parses a single CIDR for a request, failing on error.
func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	return mustCIDRs(t, s)[0]
}

func TestEgressDestination_FQDNSuffixMatch(t *testing.T) {
	a := NewEgressDestination([]string{"golang.org", "Example.COM."}, nil)

	cases := []struct {
		host string
		want bool
	}{
		{"golang.org", true},           // exact
		{"proxy.golang.org", true},     // subdomain
		{"sum.golang.org", true},       // subdomain
		{"PROXY.GOLANG.ORG", true},     // case-insensitive
		{"proxy.golang.org.", true},    // trailing dot
		{"example.com", true},          // normalized static entry
		{"a.b.example.com", true},      // deep subdomain
		{"notgolang.org", false},       // suffix is not a label boundary
		{"golang.org.evil.com", false}, // suffix in the middle
		{"org", false},                 // parent of the suffix
		{"", false},                    // empty
	}
	for _, c := range cases {
		if got := a.CoversFQDN(c.host); got != c.want {
			t.Errorf("CoversFQDN(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestEgressDestination_FQDNWildcardEntryEquivalentToBare(t *testing.T) {
	a := NewEgressDestination([]string{"*.golang.org"}, nil)
	if !a.CoversFQDN("proxy.golang.org") {
		t.Error("a *.golang.org entry must cover proxy.golang.org")
	}
	if !a.CoversFQDN("golang.org") {
		t.Error("a *.golang.org entry normalizes to golang.org and covers the bare domain")
	}
}

func TestEgressDestination_CIDRContainment(t *testing.T) {
	a := NewEgressDestination(nil, mustCIDRs(t, "10.0.0.0/8", "192.168.1.0/24"))

	cases := []struct {
		cidr string
		want bool
	}{
		{"10.0.0.0/8", true},     // exact
		{"10.1.0.0/16", true},    // subnet
		{"10.1.2.0/24", true},    // deeper subnet
		{"192.168.1.0/24", true}, // exact
		{"192.168.1.128/25", true},
		{"10.0.0.0/7", false},     // broader than the allow entry
		{"172.16.0.0/12", false},  // disjoint
		{"192.168.2.0/24", false}, // adjacent but not contained
	}
	for _, c := range cases {
		if got := a.CoversCIDR(mustCIDR(t, c.cidr)); got != c.want {
			t.Errorf("CoversCIDR(%q) = %v, want %v", c.cidr, got, c.want)
		}
	}
}

func TestEgressDestination_CIDRFamilyMismatch(t *testing.T) {
	a := NewEgressDestination(nil, mustCIDRs(t, "10.0.0.0/8"))
	// An IPv6 request must never be matched by an IPv4 allow-entry.
	if a.CoversCIDR(mustCIDR(t, "2001:db8::/32")) {
		t.Error("IPv6 request must not match an IPv4 allow-entry")
	}
}

func TestEgressDestination_EmptyDeniesAll(t *testing.T) {
	a := NewEgressDestination(nil, nil)
	if a.CoversFQDN("proxy.golang.org") {
		t.Error("empty allowlist must deny every FQDN (secure default)")
	}
	if a.CoversCIDR(mustCIDR(t, "10.0.0.0/8")) {
		t.Error("empty allowlist must deny every CIDR (secure default)")
	}
	// A nil receiver is also deny-all (defensive).
	var nilA *EgressDestinationAllowlist
	if nilA.CoversFQDN("golang.org") || nilA.CoversCIDR(mustCIDR(t, "10.0.0.0/8")) {
		t.Error("nil allowlist must deny all")
	}
}

func TestEgressDestination_DynamicIsAdditiveAndFailSafe(t *testing.T) {
	a := NewEgressDestination([]string{"golang.org"}, mustCIDRs(t, "10.0.0.0/8"))

	// Dynamic widens.
	a.SetDynamic([]string{"npmjs.org"}, mustCIDRs(t, "172.16.0.0/12"))
	if !a.CoversFQDN("registry.npmjs.org") {
		t.Error("dynamic FQDN entry should widen the allowlist")
	}
	if !a.CoversCIDR(mustCIDR(t, "172.16.5.0/24")) {
		t.Error("dynamic CIDR entry should widen the allowlist")
	}
	// Static base still in force alongside the dynamic set.
	if !a.CoversFQDN("proxy.golang.org") || !a.CoversCIDR(mustCIDR(t, "10.1.0.0/16")) {
		t.Error("static base must remain in force when dynamic is set")
	}

	// Fail-safe: clearing the dynamic set leaves only the static base.
	a.SetDynamic(nil, nil)
	if a.CoversFQDN("registry.npmjs.org") || a.CoversCIDR(mustCIDR(t, "172.16.5.0/24")) {
		t.Error("clearing dynamic must drop the dynamic entries")
	}
	if !a.CoversFQDN("proxy.golang.org") || !a.CoversCIDR(mustCIDR(t, "10.1.0.0/16")) {
		t.Error("clearing dynamic must NOT drop the static base")
	}
}

func TestEgressDestination_EffectiveListsSortedDeduped(t *testing.T) {
	a := NewEgressDestination([]string{"golang.org"}, mustCIDRs(t, "10.0.0.0/8"))
	a.SetDynamic([]string{"golang.org", "npmjs.org"}, mustCIDRs(t, "10.0.0.0/8", "172.16.0.0/12"))

	fqdns := a.FQDNSuffixes()
	if len(fqdns) != 2 || fqdns[0] != "golang.org" || fqdns[1] != "npmjs.org" {
		t.Errorf("FQDNSuffixes = %v, want sorted+deduped [golang.org npmjs.org]", fqdns)
	}
	cidrs := a.CIDRStrings()
	if len(cidrs) != 2 || cidrs[0] != "10.0.0.0/8" || cidrs[1] != "172.16.0.0/12" {
		t.Errorf("CIDRStrings = %v, want sorted+deduped [10.0.0.0/8 172.16.0.0/12]", cidrs)
	}
}
