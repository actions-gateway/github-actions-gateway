package migrate

import (
	"encoding/json"
	"fmt"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// maxNameLen is the v2 CR name cap (§H.6): every v2 object name is bounded to 52
// characters so the GMC/AGC can derive <name>-<suffix> child names and label
// values that stay under the 63-char RFC 1123 budget. The migration must emit
// names that satisfy the same cap the v2 CRD CEL rules enforce, or `--apply`
// would be rejected at admission.
const maxNameLen = 52

// cap52 bounds name to maxNameLen characters. A name already within the cap is
// returned unchanged so migrated objects keep their recognizable v1 names. A name
// over the cap is truncated and suffixed with a 6-hex content hash so two distinct
// long names cannot collide after truncation; the boolean reports whether
// truncation happened so the caller can warn the operator that a name changed.
//
// The truncation itself is [apinames.Truncate], which additionally trims a hyphen
// the cut exposes — so a cut landing on one now yields "<head>-<hash>" rather than
// "<head>--<hash>". Both are valid; the doubled separator was only ever cosmetic,
// since the hash tail already guaranteed a valid final character.
func cap52(name string) (string, bool) {
	if len(name) <= maxNameLen {
		return name, false
	}
	return apinames.Truncate(name, maxNameLen, 6), true
}

// derive builds a "<base>-<suffix>" child name bounded to maxNameLen. When the
// joined name fits it is returned verbatim (the readable, recognizable form);
// otherwise the whole joined name is hashed-and-truncated by cap52 so the result
// stays deterministic and within the cap. The boolean reports truncation.
func derive(base, suffix string) (string, bool) {
	return cap52(base + "-" + suffix)
}

// egressProxyName is the generated EgressProxy name for a gateway's inline proxy
// (§H.11): "<gateway>-egress", bounded to the 52-char cap. It is distinct from the
// runtime per-gateway derivation (<ep>-proxy Service) so the extracted proxy name
// cannot collide with a gateway-derived child name.
func egressProxyName(gatewayName string) (string, bool) {
	return derive(gatewayName, "egress")
}

// templateName is the content-addressed RunnerTemplate name: "rt-<12 hex of the
// canonical-JSON SHA-256 of the built v2 RunnerTemplateSpec>". Because the name is
// a pure function of the (podTemplate, workerImage) content, K groups that share an
// identical template map to one name — so the object-size reuse invariant (§H.17
// invariant 2) holds by construction, not just by an explicit dedup pass. The
// "rt-" prefix keeps it human-recognizable; 12 hex (48 bits) is ample for a
// per-namespace template set and the result (15 chars) is always within the cap.
func templateName(spec v2alpha1.RunnerTemplateSpec) (string, error) {
	key, err := canonicalKey(spec)
	if err != nil {
		return "", err
	}
	return "rt-" + apinames.ShortHash(key, 12), nil
}

// clusterTemplateName is the ClusterRunnerTemplate name emitted for a privileged
// (DinD/sysbox) v1 worker shape: "crt-<namespace>-<12 hex of the canonical-JSON
// SHA-256 of the built spec>", bounded to the 52-char cap.
//
// It is namespace-QUALIFIED where templateName is not, and that difference is
// load-bearing. ClusterRunnerTemplate is cluster-scoped, so a bare content address
// would make two tenants with an identical privileged worker shape share ONE object:
// the second migration would silently adopt the first tenant's template, and a later
// edit by either tenant's operator would change the other's worker pods. v1 gave each
// namespace its own inline podTemplate, so per-namespace names are what preserves the
// v1 property. Within one namespace the content address still collapses K identical
// groups to one object, exactly as the namespaced kind does.
//
// The boolean reports truncation so the caller can warn that a name changed: a long
// namespace can push the joined name past the cap, and cap52's hash-suffixed
// truncation keeps the result deterministic and unique but no longer readable.
func clusterTemplateName(namespace string, spec v2alpha1.RunnerTemplateSpec) (string, bool, error) {
	key, err := canonicalKey(spec)
	if err != nil {
		return "", false, err
	}
	name, truncated := cap52("crt-" + namespace + "-" + apinames.ShortHash(key, 12))
	return name, truncated, nil
}

// canonicalKey serializes a RunnerTemplateSpec to a stable string used both as the
// reuse-dedup key and as the input to the content-addressed template name. Go's
// encoding/json emits struct fields in declaration order and map keys sorted, so
// the encoding is deterministic across runs and processes for a given spec value.
// Two groups whose authored (podTemplate, workerImage) are identical produce the
// same key and therefore collapse to one RunnerTemplate; two that differ in any
// field — including workerImage — produce distinct keys and stay separate.
func canonicalKey(spec v2alpha1.RunnerTemplateSpec) (string, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("canonicalize RunnerTemplateSpec: %w", err)
	}
	return string(b), nil
}
