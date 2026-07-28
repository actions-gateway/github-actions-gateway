// Package validation holds the platform checks shared by the GMC's versioned
// webhook packages. The v1alpha1 and v2alpha1 validators enforce the same
// platform invariants on different API groups; keeping the single source of
// truth here (rather than a copy per version, the drift the Q323 audit found)
// means the guards survive the planned v1 sunset unchanged.
//
// Non-webhook consumers that must reach the SAME verdict as admission live here
// too: `gag-migrate` reads the privileged-eligibility grant through
// PrivilegedGrantPresent so the tool and the webhook can never disagree about
// whether a namespace is granted (Q463).
package validation

import (
	"fmt"
	"net/url"
	"strings"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// defaultReservedNamespaces are namespaces in which tenant-facing GAG CRs are
// forbidden regardless of where the GMC is installed. `kube-system` and
// `kube-public` are universal; `gmc-system` is the default install namespace
// shipped by the project. Custom installs add their own namespace at setup
// time via the downward API (see ReservedNamespaces).
var defaultReservedNamespaces = []string{
	"kube-system",
	"kube-public",
	"gmc-system",
}

// ReservedNamespaces returns the full set of namespaces where tenant-facing
// CRs may not be created. The defaults always apply; podNamespace is added
// when non-empty so that a non-default install (e.g.
// `actions-gateway-operator`) is also protected.
func ReservedNamespaces(podNamespace string) map[string]bool {
	s := make(map[string]bool, len(defaultReservedNamespaces)+1)
	for _, ns := range defaultReservedNamespaces {
		s[ns] = true
	}
	if podNamespace != "" {
		s[podNamespace] = true
	}
	return s
}

// PrivilegedGrantPresent reports whether the supplied namespace labels carry the
// platform's privileged-eligibility grant on EITHER label domain: the v1
// actions-gateway.github.com/privileged-profile or the aligned v2
// actions-gateway.com/privileged-profile, each set to the "allowed" keyword.
//
// The dual-read spans the v1/v2 coexistence window (§H.12). The M5 migration
// relabels the grant onto the v2 domain, and a still-running v1 ActionsGateway in
// that namespace must stay admitted as privileged, so both spellings of the one
// platform grant are honored until v1alpha1 is removed. This widens only the
// accepted *spelling* of an existing grant — the value keyword is identical on
// both domains — so the check stays fail-closed: an absent label, or any other
// value, is not a grant.
//
// Every consumer that decides on this grant must call this rather than read one
// domain. Reading only v1 makes a v2-domain-only grant — legal, and admitted by
// the webhook — look absent; `gag-migrate` did exactly that and warned operators
// mid-migration that a grant they already held was missing (Q463).
func PrivilegedGrantPresent(nsLabels map[string]string) bool {
	return nsLabels[gmcv1alpha1.PrivilegedProfileLabel] == gmcv1alpha1.PrivilegedProfileAllowed ||
		nsLabels[v2alpha1.PrivilegedProfileLabel] == v2alpha1.PrivilegedProfileAllowed
}

// GitHubURL rejects a spec.gitHubURL that is not a well-formed GitHub
// org/enterprise/repo URL: it must parse, use the https scheme, name a host,
// and carry at least one path segment (the organization, enterprise, or
// owner). The AGC's GithubRegistrar derives its REST endpoints by
// string-splitting this URL (see cmd/agc/internal/agentpool/github_registrar.go),
// so a malformed value would silently produce broken registration calls rather
// than a clear failure. The check lives in the webhooks (not a CRD CEL rule)
// so the error can name the offending component; the CRD Pattern is only a
// cheap https scheme guard.
func GitHubURL(raw string) error {
	if raw == "" {
		// The CRDs mark gitHubURL required (MinLength=1); a hand-built object that
		// reaches a validator directly without it is still rejected here.
		return fmt.Errorf("gitHubURL is required: set the GitHub organization, enterprise, or repository URL the runners register against")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("gitHubURL %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("gitHubURL must use the https scheme (got %q)", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("gitHubURL must include a host (got %q)", raw)
	}
	if strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("gitHubURL must include an organization, enterprise, or owner/repo path segment (got %q)", raw)
	}
	return nil
}
