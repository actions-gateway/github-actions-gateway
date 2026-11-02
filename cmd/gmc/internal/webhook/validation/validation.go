// Package validation holds the platform checks shared by the GMC's versioned
// webhook packages, so each version's validator enforces one copy of each
// platform invariant rather than a copy per version (the drift the Q323 audit
// found).
package validation

import (
	"fmt"
	"net/url"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// DeletionOnlyUpdate reports whether an update is a deletion-only write: the
// object already carries a deletionTimestamp (set only by the API server on
// delete, immutable once set) and the incoming spec is deep-equal to the stored
// spec — the shape of a finalizer removal or any other metadata-only teardown
// write. ValidateUpdate admits such writes without re-running the platform gates
// (Q518): an object whose spec is identical to the already-stored spec cannot
// reference anything the stored object does not, so skipping re-validation
// widens nothing — while denying the write wedges teardown whenever a dynamic
// gate has tightened since the object was stored (a PriorityClass allowlist
// narrowed, an eligibility label removed), leaving finalizers permanently stuck
// and the tenant namespace in Terminating (the Q499 wedge). A spec CHANGE on a
// deleting object is not exempt and stays fully validated. The
// priorityclass-allowlist-guard VAP carries the same exemption as a
// matchCondition, so the two admission layers agree.
func DeletionOnlyUpdate(newObj metav1.Object, oldSpec, newSpec any) bool {
	return newObj.GetDeletionTimestamp() != nil && apiequality.Semantic.DeepEqual(oldSpec, newSpec)
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
