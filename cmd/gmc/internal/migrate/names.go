package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// maxNameLen is the v2 CR name cap (§H.6): every v2 object name is bounded to 52
// characters so the GMC/AGC can derive <name>-<suffix> child names and label
// values that stay under the 63-char RFC 1123 budget. The migration must emit
// names that satisfy the same cap the v2 CRD CEL rules enforce, or `--apply`
// would be rejected at admission.
const maxNameLen = 52

// shortHash returns the first n hex characters of the SHA-256 of s. It is used
// to disambiguate a truncated name and to derive a content-addressed template
// name; n is small (6–12) because these are collision-resistance hints over a
// per-namespace object set, not cryptographic identifiers.
func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// cap52 bounds name to maxNameLen characters. A name already within the cap is
// returned unchanged so migrated objects keep their recognizable v1 names. A name
// over the cap is truncated and suffixed with a 6-hex content hash so two distinct
// long names cannot collide after truncation; the boolean reports whether
// truncation happened so the caller can warn the operator that a name changed.
func cap52(name string) (string, bool) {
	if len(name) <= maxNameLen {
		return name, false
	}
	h := shortHash(name, 6)
	keep := maxNameLen - 1 - len(h) // room for "-<hash>"
	return name[:keep] + "-" + h, true
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
	return "rt-" + shortHash(key, 12), nil
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
