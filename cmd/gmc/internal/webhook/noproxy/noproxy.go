// Package noproxy holds the EgressProxy webhook's admission-time guard: a
// tenant-authored NO_PROXY exclusion list must never route
// GitHub traffic around the per-tenant egress proxy, or the egress-IP attribution
// that isolates tenants is silently defeated.
package noproxy

import (
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/idna"
)

// GitHubHosts is the set of public GitHub hostnames that the AGC and worker pods
// must always reach *through* the per-tenant egress proxy for the egress-IP
// attribution to hold. A NO_PROXY entry that matches any of these would route that
// tenant's GitHub traffic around the proxy. Callers that know a tenant-specific
// GitHub host (a GitHub Enterprise Server host from gitHubURL) append it to the
// protected set they pass ValidateEntries.
var GitHubHosts = []string{
	"github.com",
	"api.github.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"raw.githubusercontent.com",
	"pkg-containers.githubusercontent.com",
	"ghcr.io",
}

// ValidateEntries rejects any NO_PROXY entry that would route traffic for one of the
// protected hosts around the per-tenant egress proxy, defeating the egress-IP
// attribution that isolates tenants. The entries are joined with commas into the
// AGC/worker NO_PROXY env var (buildNoProxy), so the guard reads each entry the way
// the AGC's consumer, Go's net/http, parses that variable (matches). fieldPath names
// the CRD field in the error (e.g. "spec.noProxyCIDRs").
//
// The check is surgical, not a blanket "CIDRs only" rule: NO_PROXY legitimately
// takes domain-suffix entries for cluster-internal destinations (the GMC's own
// default appends svc.cluster.local/localhost, and tenants reach in-cluster
// services that way), so forbidding all hostnames would break a supported,
// load-bearing pattern. Only entries that NO_PROXY-match a protected host are
// rejected. A CIDR/IP entry matches only a protected host that is itself an IP
// literal, because net/http compares an address entry with an IP-literal host and
// never resolves a name first. An address entry covering GitHub's published ranges
// is therefore allowed through; those ranges rotate, and what the other NO_PROXY
// consumers in a worker pod do with one is the operator's residual
// (05-security.md §5.2).
//
// This is a webhook check, not a CRD CEL rule, because the protected set is dynamic
// (the referring gateways' gitHubURL hosts) and the check is version-agnostic.
func ValidateEntries(fieldPath string, entries, protectedHosts []string) error {
	for i, entry := range entries {
		for _, host := range protectedHosts {
			if matches(entry, host) {
				return fmt.Errorf(
					"%s[%d]: %q would route GitHub traffic (%s) around the per-tenant egress proxy, "+
						"defeating egress-IP attribution; remove it — GitHub must always traverse the proxy. "+
						"noProxyCIDRs may exclude internal destinations (CIDRs or domain suffixes), never GitHub",
					fieldPath, i, entry, host)
			}
		}
	}
	return nil
}

// matches reports whether one noProxyCIDRs entry exempts host from the proxy once it
// reaches NO_PROXY. It follows the parse in Go's net/http (the vendored
// golang.org/x/net/http/httpproxy), pinned by TestMatchesAgreesWithNetHTTP against
// go1.27.3: the value splits on commas, so one entry can carry several; "*" exempts
// every host; a CIDR or IP exempts only an IP-literal host; a "*." or "." prefix
// and a ":port" suffix are dropped before the domain-suffix comparison; and a
// non-ASCII name is compared in its IDNA ASCII form.
//
// It is stricter than that parse in two places. A port-qualified entry is matched
// whatever its port, because the guard is handed a host, not the port its traffic
// uses. A leading-dot entry also matches the bare domain, which net/http leaves
// proxied but other NO_PROXY consumers in a worker pod may not.
func matches(entry, host string) bool {
	hostIP := net.ParseIP(host)
	host = toASCII(strings.ToLower(host))
	for _, p := range strings.Split(entry, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == "*" {
			return true
		}
		if _, cidr, err := net.ParseCIDR(p); err == nil {
			if hostIP != nil && cidr.Contains(hostIP) {
				return true
			}
			continue
		}
		if h, _, err := net.SplitHostPort(p); err == nil {
			p = h
		}
		if ip := net.ParseIP(p); ip != nil {
			if ip.Equal(hostIP) {
				return true
			}
			continue
		}
		if hostIP != nil {
			continue
		}
		if strings.HasPrefix(p, "*.") {
			p = p[1:]
		}
		p = toASCII(strings.TrimPrefix(p, "."))
		if p == "" {
			continue
		}
		if host == p || strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

// toASCII returns the IDNA ASCII form of a name, or the name unchanged when it has
// none, which is what the consumer does with an entry it cannot convert.
func toASCII(name string) string {
	if ascii, err := idna.Lookup.ToASCII(name); err == nil {
		return ascii
	}
	return name
}
