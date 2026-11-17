package allowlist

import (
	"net"
	"sort"
	"strings"
)

// EgressDestinationAllowlist is the platform-owned gate (Q242 G.1) on which
// non-GitHub destinations a tenant-authored EgressProxy may request via
// spec.destinationFQDNs / spec.destinationCIDRs. It holds two allowlists, FQDN
// host suffixes and IP CIDRs, fixed at construction from the GMC
// --allowed-egress-fqdns / --allowed-egress-cidrs flags, so it is read without a
// lock and a change takes a GMC restart.
//
// Empty (no flags) ⇒ CoversFQDN/CoversCIDR always return false ⇒
// deny-all-non-GitHub, the secure default: out of the box no tenant can open
// egress beyond the implicit GitHub set.
type EgressDestinationAllowlist struct {
	fqdns []string
	cidrs []*net.IPNet
}

// NewEgressDestination returns the allowlist of the --allowed-egress-fqdns
// suffixes and --allowed-egress-cidrs ranges.
func NewEgressDestination(fqdns []string, cidrs []*net.IPNet) *EgressDestinationAllowlist {
	return &EgressDestinationAllowlist{
		fqdns: normalizeFQDNSuffixes(fqdns),
		cidrs: cidrs,
	}
}

// CoversFQDN reports whether host is permitted by the FQDN allowlist: it is equal
// to, or a subdomain of, some allowlisted suffix (so an allowlisted "golang.org"
// covers a requested "proxy.golang.org"). A leading "*." wildcard on the requested
// host is treated as the bare parent domain.
func (a *EgressDestinationAllowlist) CoversFQDN(host string) bool {
	if a == nil {
		return false
	}
	host = normalizeFQDN(host)
	if host == "" {
		return false
	}
	return matchesAnySuffix(host, a.fqdns)
}

// CoversCIDR reports whether the requested network is fully contained within some
// allowlisted CIDR of the same address family (so an allowlisted "10.0.0.0/8"
// covers a requested "10.1.0.0/16"). An exact match is contained in itself.
func (a *EgressDestinationAllowlist) CoversCIDR(req *net.IPNet) bool {
	if a == nil || req == nil {
		return false
	}
	return cidrContainedInAny(req, a.cidrs)
}

// FQDNSuffixes returns the FQDN allowlist as a sorted, de-duplicated slice for
// deterministic admission-rejection messages.
func (a *EgressDestinationAllowlist) FQDNSuffixes() []string {
	if a == nil {
		return nil
	}
	set := make(map[string]bool, len(a.fqdns))
	for _, s := range a.fqdns {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// CIDRStrings returns the CIDR allowlist as a sorted, de-duplicated slice of CIDR
// strings for deterministic admission-rejection messages.
func (a *EgressDestinationAllowlist) CIDRStrings() []string {
	if a == nil {
		return nil
	}
	set := make(map[string]bool, len(a.cidrs))
	for _, c := range a.cidrs {
		set[c.String()] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// matchesAnySuffix reports whether host equals or is a subdomain of any suffix.
// Both sides are assumed already normalized (lowercase, no trailing dot, no "*.").
func matchesAnySuffix(host string, suffixes []string) bool {
	for _, suf := range suffixes {
		if suf == "" {
			continue
		}
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return true
		}
	}
	return false
}

// cidrContainedInAny reports whether req is a subnet of (or equal to) any allowed
// CIDR of the same address family. Containment requires the same total bit width
// (so an IPv4 request is never matched by an IPv6 allow-entry), the allow network
// to contain the request's base IP, and the request prefix to be at least as
// specific as the allow prefix.
func cidrContainedInAny(req *net.IPNet, allowed []*net.IPNet) bool {
	reqOnes, reqBits := req.Mask.Size()
	for _, a := range allowed {
		if a == nil {
			continue
		}
		aOnes, aBits := a.Mask.Size()
		if aBits != reqBits {
			continue
		}
		if aOnes <= reqOnes && a.Contains(req.IP) {
			return true
		}
	}
	return false
}

// normalizeFQDNSuffixes normalizes and de-empties a list of host-suffix entries.
func normalizeFQDNSuffixes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if n := normalizeFQDN(s); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// normalizeFQDN lowercases a host/suffix, trims surrounding whitespace and dots,
// and strips a leading "*." wildcard so "*.golang.org" and "golang.org" compare
// equal (the suffix matcher already covers subdomains).
func normalizeFQDN(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "*.")
	return strings.Trim(s, ".")
}
