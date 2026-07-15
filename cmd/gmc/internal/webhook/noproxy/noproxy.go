/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package noproxy holds the admission-time guard shared by the v1 ActionsGateway and
// v2 EgressProxy webhooks: a tenant-authored NO_PROXY exclusion list must never route
// GitHub traffic around the per-tenant egress proxy, or the egress-IP attribution
// that isolates tenants is silently defeated.
package noproxy

import (
	"fmt"
	"net/netip"
	"strings"
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
// attribution that isolates tenants. Each entry is threaded verbatim into the
// AGC/worker NO_PROXY env var (builder.go buildNoProxy), where Go's httpproxy matches
// a hostname entry as a domain suffix. So an entry like "github.com" (or
// ".github.com", or an over-broad ".com") would silently exclude GitHub from the
// proxy — the documented footgun. fieldPath names the CRD field in the error (e.g.
// "proxy.noProxyCIDRs" or "spec.noProxyCIDRs").
//
// The check is surgical, not a blanket "CIDRs only" rule: NO_PROXY legitimately
// takes domain-suffix entries for cluster-internal destinations (the GMC's own
// default appends svc.cluster.local/localhost, and tenants reach in-cluster
// services that way), so forbidding all hostnames would break a supported,
// load-bearing pattern. Only entries that NO_PROXY-match a protected host are
// rejected. A CIDR/IP entry is allowed through here even if it happens to cover
// GitHub's published ranges — those ranges rotate and an in-tree IP blocklist would
// rot into a false sense of safety; that residual is the operator's responsibility
// (see 05-security.md §5.2).
//
// This is a webhook check, not a CRD CEL rule, because the protected set is dynamic
// for the v1 caller (the gitHubURL host parse) and the check is version-agnostic.
func ValidateEntries(fieldPath string, entries, protectedHosts []string) error {
	for i, entry := range entries {
		// CIDR / bare-IP entries cannot be a hostname-suffix bypass; the
		// IP-range residual is accepted and documented.
		if !IsHostnameEntry(entry) {
			continue
		}
		for _, host := range protectedHosts {
			if hostnameMatches(entry, host) {
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

// IsHostnameEntry reports whether a NO_PROXY entry is a hostname (domain-suffix)
// entry rather than a CIDR prefix or bare IP. Only hostname entries can suffix-match
// a protected GitHub host, so callers use this to skip the (API-reading) referrer
// resolution when every entry is an address.
func IsHostnameEntry(entry string) bool {
	if _, err := netip.ParsePrefix(entry); err == nil {
		return false
	}
	if _, err := netip.ParseAddr(entry); err == nil {
		return false
	}
	return true
}

// hostnameMatches reports whether a NO_PROXY hostname entry would match the given
// host under Go's httpproxy domain-suffix semantics: an entry matches a host that
// equals it or is a sub-domain of it. A leading dot on the entry is insignificant
// for this purpose (".github.com" and "github.com" both match "api.github.com");
// the comparison is case-insensitive.
func hostnameMatches(entry, host string) bool {
	e := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(entry), "."))
	h := strings.ToLower(host)
	if e == "" {
		return false
	}
	return h == e || strings.HasSuffix(h, "."+e)
}
