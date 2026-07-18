// Package validation holds admission checks shared by the GMC's versioned
// webhook packages. The v1alpha1 and v2alpha1 validators enforce the same
// platform invariants on different API groups; keeping the single source of
// truth here (rather than a copy per version, the drift the Q323 audit found)
// means the guards survive the planned v1 sunset unchanged.
package validation

import (
	"fmt"
	"net/url"
	"strings"
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
