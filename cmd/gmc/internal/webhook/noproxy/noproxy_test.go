package noproxy

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
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

// TestValidateEntries_BlankEntryAllowed asserts an entry with no host part is inert
// in NO_PROXY, and a dot-only one is inert for a host written without a trailing dot.
func TestValidateEntries_BlankEntryAllowed(t *testing.T) {
	require.NoError(t, ValidateEntries("spec.noProxyCIDRs", []string{"", " ", ".", ",", ":443", "x *"}, GitHubHosts))
}

// TestValidateEntries_TrailingDot covers a host or an entry spelled with the root
// label's dot: a dot-only entry exempts every such host in net/http, and libcurl
// ignores one trailing dot on either side.
func TestValidateEntries_TrailingDot(t *testing.T) {
	dotted := []string{"ghes.corp.example."}
	for _, entry := range []string{".", "*.", ".:443", "[.]:443", "x,.", "ghes.corp.example", "corp.example", "example.", ".example"} {
		require.Errorf(t, ValidateEntries("spec.noProxyCIDRs", []string{entry}, dotted),
			"entry %q exempts a host written with a trailing dot", entry)
	}
	for _, entry := range []string{"github.com.", "com.", ".github.com.", "*.github.com.", "github.com.:443"} {
		require.Errorf(t, ValidateEntries("spec.noProxyCIDRs", []string{entry}, GitHubHosts),
			"entry %q exempts GitHub with its trailing dot ignored", entry)
	}
	require.NoError(t, ValidateEntries("spec.noProxyCIDRs",
		[]string{"github.com..", "svc.cluster.local.", "internal.example.com."},
		append([]string{"ghes.corp.example."}, GitHubHosts...)))
}

// TestValidateEntries_RejectsBlankSeparatedForms covers the separators libcurl
// honours and net/http does not: a blank or a tab ends one entry and starts the next.
func TestValidateEntries_RejectsBlankSeparatedForms(t *testing.T) {
	for _, entry := range []string{"x github.com", "x\tghcr.io", "10.0.0.0/8  api.github.com", "x .github.com:443"} {
		err := ValidateEntries("spec.noProxyCIDRs", []string{entry}, GitHubHosts)
		require.Errorf(t, err, "entry %q should be rejected", entry)
		assert.Contains(t, err.Error(), fmt.Sprintf("%q", entry))
	}
	require.NoError(t, ValidateEntries("spec.noProxyCIDRs",
		[]string{"10.0.0.0/8 svc.cluster.local", "x\ngithub.com", "x github.com"}, GitHubHosts),
		"only a blank or a tab separates entries, and only a GitHub host is refused")
}

// TestValidateEntries_RejectsCommaWildcardAndPortForms covers the NO_PROXY forms
// that exempt a GitHub host without spelling it as a bare domain suffix.
func TestValidateEntries_RejectsCommaWildcardAndPortForms(t *testing.T) {
	for _, entry := range []string{
		"x,github.com",
		"10.0.0.0/8, ghcr.io ",
		"*",
		"internal.example.com,*",
		"*.github.com",
		"github.com:443",
		"*.githubusercontent.com:443",
		"[github.com]:443",
		"ｇｉｔｈｕｂ.com", // fullwidth letters: IDNA maps them to github.com
	} {
		err := ValidateEntries("spec.noProxyCIDRs", []string{entry}, GitHubHosts)
		require.Errorf(t, err, "entry %q should be rejected", entry)
		assert.Contains(t, err.Error(), fmt.Sprintf("%q", entry))
	}
}

// TestValidateEntries_AddressEntryAgainstIPLiteralHost covers a GitHub Enterprise
// Server addressed by IP: an address entry is a bypass exactly when it covers that
// literal, and never for a named host.
func TestValidateEntries_AddressEntryAgainstIPLiteralHost(t *testing.T) {
	for _, entry := range []string{"10.0.0.0/8", "10.1.2.3", "10.1.2.3/32", "10.1.2.3:443", "::ffff:10.1.2.3"} {
		require.Errorf(t, ValidateEntries("spec.noProxyCIDRs", []string{entry}, []string{"10.1.2.3"}),
			"entry %q covers the protected address", entry)
	}
	for _, entry := range []string{"fd00::/8", "fd00::1", "[fd00::1]:443"} {
		require.Errorf(t, ValidateEntries("spec.noProxyCIDRs", []string{entry}, []string{"fd00::1"}),
			"entry %q covers the protected address", entry)
	}
	require.NoError(t, ValidateEntries("spec.noProxyCIDRs",
		[]string{"10.0.0.0/8", "10.1.2.3", "192.168.0.0/16", "2.3", "fd00::/8"},
		[]string{"10.9.9.9.example", "172.16.0.1", "ghes.corp.example"}),
		"an address entry exempts only an IP-literal host it covers")
}

// probeEnv, when set, turns the test binary into the NO_PROXY consumer: TestMain
// answers for the URLs it lists instead of running tests. net/http reads the proxy
// environment once per process, so each NO_PROXY value needs a process of its own.
const probeEnv = "NOPROXY_PROBE_URLS"

func TestMain(m *testing.M) {
	urls := os.Getenv(probeEnv)
	if urls == "" {
		os.Exit(m.Run())
	}
	for _, raw := range strings.Split(urls, " ") {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			fmt.Printf("%s error %v\n", raw, err)
			os.Exit(1)
		}
		proxy, err := http.ProxyFromEnvironment(req)
		if err != nil {
			fmt.Printf("%s error %v\n", raw, err)
			os.Exit(1)
		}
		if proxy == nil {
			fmt.Printf("%s direct\n", raw)
		} else {
			fmt.Printf("%s proxied\n", raw)
		}
	}
	os.Exit(0)
}

// consumerExempts runs net/http in a fresh process with entry in NO_PROXY, placed
// ahead of the mandatory tail as buildNoProxy places it, and reports which of hosts
// that process would dial directly rather than through HTTPS_PROXY.
func consumerExempts(t *testing.T, entry string, hosts []string) map[string]bool {
	t.Helper()
	urlToHost := map[string]string{}
	urls := make([]string, 0, len(hosts))
	for _, h := range hosts {
		u := "https://" + net.JoinHostPort(h, "443") + "/org"
		urlToHost[u] = h
		urls = append(urls, u)
	}
	cmd := exec.Command(os.Args[0]) //nolint:gosec // G204: re-runs this test binary, with no arguments.
	cmd.Env = []string{
		probeEnv + "=" + strings.Join(urls, " "),
		"HTTPS_PROXY=http://proxy.invalid:3128",
		"NO_PROXY=" + entry + ",svc.cluster.local,localhost,127.0.0.1",
	}
	out, err := cmd.Output()
	require.NoErrorf(t, err, "probe for entry %q: %s", entry, out)
	exempt := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		u, verdict, ok := strings.Cut(line, " ")
		require.Truef(t, ok, "probe line %q", line)
		require.Containsf(t, urlToHost, u, "probe line %q", line)
		require.Containsf(t, []string{"direct", "proxied"}, verdict, "probe line %q", line)
		exempt[urlToHost[u]] = verdict == "direct"
	}
	require.Len(t, exempt, len(hosts), "probe answered for every host")
	return exempt
}

// TestMatchesAgreesWithNetHTTP pins matches to the consumer it models. For every
// entry form and host, a host net/http would dial directly must be one matches
// reports, or the guard admits a bypass; and matches may report a host net/http
// still proxies only where a case declares it (stricter), so the guard does not
// refuse forms the consumer ignores.
func TestMatchesAgreesWithNetHTTP(t *testing.T) {
	hosts := []string{
		"github.com", "api.github.com", "ghes.corp.example", "10.1.2.3", "fd00::1",
		// A githubURL may spell its host with the root label's trailing dot.
		"github.com.", "ghes.corp.example.",
	}
	cases := []struct {
		entry string
		// stricter lists the hosts matches reports although net/http proxies them.
		stricter []string
	}{
		// Trailing dot ignored: net/http compares the dot literally, so an entry
		// without one leaves the dotted host proxied, and the reverse.
		{entry: "github.com", stricter: []string{"github.com."}},
		{entry: "GitHub.COM", stricter: []string{"github.com."}},
		{entry: "com", stricter: []string{"github.com."}},
		{entry: "x,github.com", stricter: []string{"github.com."}},
		{entry: "x, github.com ", stricter: []string{"github.com."}},
		{entry: "com.", stricter: []string{"github.com", "api.github.com"}},
		{entry: "github.com.", stricter: []string{"github.com", "api.github.com"}},
		{entry: "github.com.."},
		{entry: "ｇｉｔｈｕｂ.com", stricter: []string{"github.com."}},
		{entry: "corp.example", stricter: []string{"ghes.corp.example."}},
		// Leading dot or "*." also matches the bare domain.
		{entry: ".github.com", stricter: []string{"github.com", "github.com."}},
		{entry: "*.github.com", stricter: []string{"github.com", "github.com."}},
		{entry: "*github.com"},
		// A dot-only entry is the suffix ".", which a dotted host ends in.
		{entry: "."},
		{entry: "*."},
		{entry: ".:443"},
		{entry: "[.]:443"},
		{entry: ".."},
		// The whole of a comma-separated part, never a blank-separated one.
		{entry: "*"},
		{entry: "x,*"},
		{entry: " * "},
		{entry: "x *"},
		// Any port.
		{entry: "github.com:443", stricter: []string{"github.com."}},
		{entry: "github.com:", stricter: []string{"github.com."}},
		{entry: "[github.com]:443", stricter: []string{"github.com."}},
		{entry: "github.com:8080", stricter: []string{"github.com", "api.github.com", "github.com."}},
		{entry: "github.com:https", stricter: []string{"github.com", "api.github.com", "github.com."}},
		{entry: "*.github.com:443", stricter: []string{"github.com", "github.com."}},
		{entry: "ghes.corp.example:8443", stricter: []string{"ghes.corp.example", "ghes.corp.example."}},
		{entry: ":443"},
		// A blank or a tab separates entries; other whitespace does not.
		{entry: "x github.com", stricter: []string{"github.com", "api.github.com", "github.com."}},
		{entry: "x\tgithub.com", stricter: []string{"github.com", "api.github.com", "github.com."}},
		{entry: "x\ngithub.com"},
		{entry: "x github.com"},
		{entry: "xn--github-abc.com"},
		{entry: "internal.example.com"},
		{entry: "svc.cluster.local"},
		{entry: "10.0.0.0/8"},
		{entry: "10.1.2.3"},
		{entry: "10.1.2.3/32"},
		{entry: "10.1.2.3:443"},
		{entry: "10.1.2.3:8443", stricter: []string{"10.1.2.3"}},
		{entry: "::ffff:10.1.2.3"},
		{entry: "010.1.2.3"},
		{entry: "2.3"},
		{entry: ".2.3"},
		{entry: "192.168.0.0/16"},
		{entry: "fd00::1"},
		{entry: "[fd00::1]"},
		{entry: "[fd00::1]:443"},
		{entry: "fd00::/8"},
	}
	var direct, proxied int
	for _, tc := range cases {
		exempt := consumerExempts(t, tc.entry, hosts)
		for _, host := range hosts {
			got := matches(tc.entry, host)
			want := exempt[host]
			for _, s := range tc.stricter {
				if s == host {
					require.Falsef(t, want, "entry %q, host %q: declared stricter, but net/http exempts it too", tc.entry, host)
					want = true
				}
			}
			assert.Equalf(t, want, got, "entry %q, host %q: net/http exempts=%v", tc.entry, host, exempt[host])
			if exempt[host] {
				direct++
			} else {
				proxied++
			}
		}
	}
	// A probe that never saw the environment would answer the same for every pair.
	assert.Positive(t, direct, "the probe reported no exempted host")
	assert.Positive(t, proxied, "the probe reported no proxied host")
}
