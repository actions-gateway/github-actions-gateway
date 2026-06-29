package allowlist

import (
	"net"
	"sort"
	"strings"
	"sync"
)

// EgressDestinationAllowlist is the platform-owned gate (Q242 G.1) on which
// non-GitHub destinations a tenant-authored EgressProxy may request via
// spec.destinationFQDNs / spec.destinationCIDRs. It holds two allowlists — FQDN
// host suffixes and IP CIDRs — each the union of an immutable static base (the GMC
// --allowed-egress-fqdns / --allowed-egress-cidrs flags) and a dynamic set sourced
// from a single watched ConfigMap (mirroring the PriorityClass allowlist, Q188).
//
// Both halves live in one type because they are governed by one ConfigMap with two
// keys and one reconciler, so a watch event updates both atomically.
//
// The dynamic set is strictly ADDITIVE: it only ever widens the allowlist beyond
// the static base, never narrows or replaces it. This is the fail-safe design — a
// missing, deleted, or malformed ConfigMap leaves the static flag allowlist in
// force (the reconciler clears the dynamic set via SetDynamic(nil, nil) in those
// cases), so a bad ConfigMap can never silently widen the guardrail nor strip an
// entry the platform pinned via a flag.
//
// Empty everywhere (no flags, no ConfigMap) ⇒ CoversFQDN/CoversCIDR always return
// false ⇒ deny-all-non-GitHub, the secure default: out of the box no tenant can
// open egress beyond the implicit GitHub set.
//
// All methods are safe for concurrent use. The admission webhook reads the
// effective set on every ValidateCreate/ValidateUpdate while the ConfigMap
// reconciler replaces the dynamic set on watch events.
type EgressDestinationAllowlist struct {
	// staticFQDNs / staticCIDRs are fixed at construction from the flags and never
	// mutated, so they are read without the lock.
	staticFQDNs []string
	staticCIDRs []*net.IPNet

	mu sync.RWMutex
	// dynamicFQDNs / dynamicCIDRs are the ConfigMap-sourced augmentation, replaced
	// wholesale by SetDynamic. nil until the reconciler first applies a ConfigMap.
	dynamicFQDNs []string
	dynamicCIDRs []*net.IPNet
}

// NewEgressDestination returns an allowlist whose static base is the
// --allowed-egress-fqdns suffixes and --allowed-egress-cidrs ranges. The dynamic
// set starts empty, so the effective allowlist equals the static base until a
// ConfigMap is applied. nil/empty inputs yield an allowlist that permits nothing
// until the dynamic set is populated — the secure default (unset flags forbid every
// non-GitHub destination).
func NewEgressDestination(fqdns []string, cidrs []*net.IPNet) *EgressDestinationAllowlist {
	return &EgressDestinationAllowlist{
		staticFQDNs: normalizeFQDNSuffixes(fqdns),
		staticCIDRs: cidrs,
	}
}

// CoversFQDN reports whether host is permitted by the effective FQDN allowlist: it
// is equal to, or a subdomain of, some allowlisted suffix (so an allowlisted
// "golang.org" covers a requested "proxy.golang.org"). A leading "*." wildcard on
// the requested host is treated as the bare parent domain.
func (a *EgressDestinationAllowlist) CoversFQDN(host string) bool {
	if a == nil {
		return false
	}
	host = normalizeFQDN(host)
	if host == "" {
		return false
	}
	if matchesAnySuffix(host, a.staticFQDNs) {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return matchesAnySuffix(host, a.dynamicFQDNs)
}

// CoversCIDR reports whether the requested network is fully contained within some
// allowlisted CIDR of the same address family (so an allowlisted "10.0.0.0/8"
// covers a requested "10.1.0.0/16"). An exact match is contained in itself.
func (a *EgressDestinationAllowlist) CoversCIDR(req *net.IPNet) bool {
	if a == nil || req == nil {
		return false
	}
	if cidrContainedInAny(req, a.staticCIDRs) {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cidrContainedInAny(req, a.dynamicCIDRs)
}

// SetDynamic replaces the dynamic (ConfigMap-sourced) sets, augmenting the static
// base. Passing nil/empty clears the dynamic sets, leaving only the static base in
// force — the fail-safe the reconciler invokes when the ConfigMap is absent or
// fails validation.
func (a *EgressDestinationAllowlist) SetDynamic(fqdns []string, cidrs []*net.IPNet) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dynamicFQDNs = normalizeFQDNSuffixes(fqdns)
	a.dynamicCIDRs = cidrs
}

// FQDNSuffixes returns the effective FQDN allowlist (static ∪ dynamic) as a sorted,
// de-duplicated slice for deterministic admission-rejection messages.
func (a *EgressDestinationAllowlist) FQDNSuffixes() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	set := make(map[string]bool, len(a.staticFQDNs)+len(a.dynamicFQDNs))
	for _, s := range a.staticFQDNs {
		set[s] = true
	}
	for _, s := range a.dynamicFQDNs {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// CIDRStrings returns the effective CIDR allowlist (static ∪ dynamic) as a sorted,
// de-duplicated slice of CIDR strings for deterministic admission-rejection messages.
func (a *EgressDestinationAllowlist) CIDRStrings() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	set := make(map[string]bool, len(a.staticCIDRs)+len(a.dynamicCIDRs))
	for _, c := range a.staticCIDRs {
		set[c.String()] = true
	}
	for _, c := range a.dynamicCIDRs {
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
