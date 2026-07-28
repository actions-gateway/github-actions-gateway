package provisioner

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// dnsLabelRe matches characters not allowed in Kubernetes DNS labels.
var dnsLabelRe = regexp.MustCompile(`[^a-z0-9-]`)

// repoSegmentRE accepts only the characters GitHub allows in org/repo names.
// Must start with an alphanumeric character so that ".." and other dot-leading
// strings cannot produce path-traversal sequences in the API URL.
var repoSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const (
	// maxDNSLabelLen is the Kubernetes DNS-1123 label limit every derived object
	// name here must satisfy. Length is only half the rule: a label must also start
	// and end with an alphanumeric character, which is what a naive cut at 63 chars
	// can violate (Q467).
	maxDNSLabelLen = 63

	// hashLen is how many hex characters of a SHA-256 digest are appended to a
	// truncated segment to keep distinct inputs distinct. Hex digits are always
	// legal DNS-label characters, so a segment ending in one can never be rejected
	// for its final character.
	hashLen = 7

	// workerPodPrefix marks every worker pod the AGC creates, on both acquisition
	// tiers.
	workerPodPrefix = "runner"
)

// shortHash returns the first hashLen hex characters of the SHA-256 digest of s.
func shortHash(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:hashLen]
}

// safeName converts an arbitrary string into a Kubernetes-safe DNS label
// (lowercase, alphanumeric and hyphens only). The output is at most 48 chars:
// up to 40 sanitised chars from the input, a "-" separator, and 7 hex chars
// derived from a SHA-256 hash of the original string. The hash suffix ensures
// uniqueness when two different inputs share the same 40-char sanitised prefix.
//
// The result is always non-empty and always starts and ends with an alphanumeric
// character, so it is a valid DNS label on its own. Composing two of them is what
// needs care — see workerPodName.
func safeName(s string) string {
	hash := shortHash(s)
	s = strings.ToLower(s)
	s = dnsLabelRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	s = strings.TrimRight(s, "-") // re-trim after truncation
	if s == "" {
		s = "job"
	}
	return s + "-" + hash
}

// truncateSegment shortens an already-sanitised DNS-label segment to at most max
// characters without discarding what makes it unique: the cut tail is replaced by
// a hash of the *whole* segment, so two segments that share a prefix still differ.
// Any hyphen the cut exposes is trimmed, so the result never ends on one.
//
// Trimming alone would not do: it shortens the entropy-bearing suffix, and two
// worker pods that collide on a name are a worse failure than one the apiserver
// rejects. Hashing the tail keeps the segment injective (up to a hashLen-hex
// collision) at every budget.
//
// max below 1 is clamped to 1; the result is always non-empty and always starts
// and ends with an alphanumeric character, given a sanitised input.
func truncateSegment(s string, max int) string {
	if max < 1 {
		max = 1
	}
	if len(s) <= max {
		return s
	}
	hash := shortHash(s)
	if max <= hashLen {
		// No room for both a readable head and a whole hash; the hash prefix alone
		// still distinguishes distinct segments.
		return hash[:max]
	}
	head := strings.TrimRight(s[:max-hashLen-1], "-")
	if head == "" {
		// Everything before the cut was hyphens — drop the empty head rather than
		// emit a leading "-".
		return hash
	}
	return head + "-" + hash
}

// splitBudget divides avail characters between two segments whose natural lengths
// are a and b. Each may claim half; whatever a segment shorter than its half leaves
// over goes to the other, so a long segment is never starved by its neighbour and a
// short one is never padded at the other's expense. Callers must pass avail >= 2.
func splitBudget(a, b, avail int) (int, int) {
	if a+b <= avail {
		return a, b
	}
	half := avail / 2
	switch {
	case a <= half:
		return a, avail - a
	case b <= half:
		return avail - b, b
	default:
		return half, avail - half
	}
}

// workerPodName derives the worker pod name for one job — "runner-<owner>-<id>" —
// as a valid DNS-1123 label of at most maxDNSLabelLen characters. Both acquisition
// tiers derive their pod names here: the v1 path from the RunnerGroup name and the
// job's plan ID, the v2 scale-set path from the RunnerSet name and GitHub's job ID.
//
// Truncation is unavoidable: "runner-" plus a sanitised owner plus a sanitised
// 36-char UUID overruns 63 characters for any realistic tenant name. Truncating the
// concatenation is what broke (Q467) — a cut that landed on one of a UUID's hyphens
// produced a name the apiserver rejected, so *every* worker pod for that owner
// failed to create and no job ever ran, while GitHub reported only that the runner
// had lost communication. The budget is therefore split before the segments are
// joined, and each segment is truncated by truncateSegment, which can only end on
// an alphanumeric character.
//
// Uniqueness after truncation: each segment is either the full safeName of its
// input — which already ends in a 7-hex digest of that whole input — or a
// truncateSegment of it, which ends in a 7-hex digest of that whole safeName. So a
// name collision needs *both* segments to collide, and a segment collision needs
// both a shared visible prefix and a shared digest. The id segment alone is
// therefore enough: distinct job identities give distinct pod names. A repeat
// delivery of the *same* job deliberately maps to the same name, which is what
// makes pod creation idempotent and lets the v2 completion path find the pod it
// created.
func workerPodName(owner, id string) string {
	ownerPart := safeName(owner)
	idPart := safeName(id)
	// Two separators: one after the prefix, one between the segments.
	avail := maxDNSLabelLen - len(workerPodPrefix) - 2
	ownerBudget, idBudget := splitBudget(len(ownerPart), len(idPart), avail)
	return workerPodPrefix + "-" +
		truncateSegment(ownerPart, ownerBudget) + "-" +
		truncateSegment(idPart, idBudget)
}
