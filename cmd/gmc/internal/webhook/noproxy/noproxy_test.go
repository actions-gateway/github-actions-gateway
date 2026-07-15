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

package noproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateEntries_RejectsProtectedHostMatches covers the NO_PROXY domain-suffix
// semantics the guard exists for: exact host, leading-dot, parent-suffix, over-broad
// suffix, and case-insensitive entries must all be rejected against GitHubHosts.
func TestValidateEntries_RejectsProtectedHostMatches(t *testing.T) {
	for _, entry := range []string{
		"github.com",
		".github.com",
		"api.github.com",
		"githubusercontent.com",
		"GHCR.IO",
		".com", // over-broad suffix covers every protected host
	} {
		err := ValidateEntries("spec.noProxyCIDRs", []string{entry}, GitHubHosts)
		require.Errorf(t, err, "entry %q should be rejected", entry)
		assert.Contains(t, err.Error(), "spec.noProxyCIDRs[0]")
		assert.Contains(t, err.Error(), "around the per-tenant egress proxy")
	}
}

// TestValidateEntries_AllowsInternalEntries asserts the guard is surgical: CIDRs,
// bare IPs, and non-GitHub domain suffixes (the supported internal-destination
// pattern) all pass.
func TestValidateEntries_AllowsInternalEntries(t *testing.T) {
	err := ValidateEntries("spec.noProxyCIDRs", []string{
		"10.0.0.0/8", "203.0.113.5/32", "fd00::/8", // CIDRs
		"10.0.0.5",                       // bare IP
		"svc.cluster.local", "localhost", // cluster-internal domain suffixes
		"internal.example.com", // a non-GitHub internal domain
	}, GitHubHosts)
	require.NoError(t, err)
}

// TestValidateEntries_ExtraProtectedHost mirrors the v1 GHES case: a caller-appended
// protected host (a GitHub Enterprise Server hostname) is guarded like the static set.
func TestValidateEntries_ExtraProtectedHost(t *testing.T) {
	protected := append([]string{}, GitHubHosts...)
	protected = append(protected, "ghes.example.com")
	err := ValidateEntries("proxy.noProxyCIDRs", []string{"example.com"}, protected)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghes.example.com")
}

// TestValidateEntries_EmptyAndIndex asserts nil/empty lists pass and the error names
// the offending index, not just the first entry.
func TestValidateEntries_EmptyAndIndex(t *testing.T) {
	require.NoError(t, ValidateEntries("spec.noProxyCIDRs", nil, GitHubHosts))

	err := ValidateEntries("spec.noProxyCIDRs", []string{"10.0.0.0/8", "ghcr.io"}, GitHubHosts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.noProxyCIDRs[1]")
}

// TestValidateEntries_BlankEntryAllowed guards the hostnameMatches empty-entry
// short-circuit: a whitespace/dot-only entry is inert in NO_PROXY, not a bypass.
func TestValidateEntries_BlankEntryAllowed(t *testing.T) {
	require.NoError(t, ValidateEntries("spec.noProxyCIDRs", []string{"", " ", "."}, GitHubHosts))
}

// TestIsHostnameEntry pins the hostname-vs-address split callers rely on to skip
// the API-reading referrer resolution: only hostname entries can suffix-match a
// protected host.
func TestIsHostnameEntry(t *testing.T) {
	cases := map[string]bool{
		"10.0.0.0/8":        false,
		"203.0.113.5":       false,
		"fd00::/8":          false,
		"2001:db8::1":       false,
		"github.com":        true,
		".corp.example":     true,
		"svc.cluster.local": true,
		"localhost":         true,
	}
	for entry, want := range cases {
		assert.Equalf(t, want, IsHostnameEntry(entry), "IsHostnameEntry(%q)", entry)
	}
}
