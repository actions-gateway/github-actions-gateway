// Package apinames centralises how the GMC and AGC derive Kubernetes object
// names, label values, and GitHub runner names from tenant-controlled input.
//
// It lives in the neutral api module for the same reason [apilabels] does: both
// controllers derive names from the same CRs, and a divergence between their two
// ideas of a name is not cosmetic. The v1 RunnerGroup name is derived by the GMC,
// replicated by `gag-migrate` so its synthesized names match what the GMC would
// have materialized, and consumed by the AGC as a label value on every worker pod
// and agent Secret. Before this package there were four near-copies of the logic
// across three packages, two of them held in sync by a comment.
//
// # The failure mode this package exists to prevent
//
// A derived name is valid at the layer that creates it and fatal at the layer that
// consumes it. A Kubernetes object name may be 253 characters, but a *label value*
// and a *Service name* may be only 63, and every name here must also start and end
// with an alphanumeric character. Two shipped bugs came from that gap:
//
//   - Q467: the worker pod name was assembled and then cut at 63. The cut landed on
//     one of the job UUID's hyphens for five owner-name lengths, so the apiserver
//     rejected every worker pod for those tenants and no job ever ran.
//   - The v1 `<gateway>-<label>` RunnerGroup name was never bounded at all. Past 63
//     characters the CR is still created — it is a legal object name — but the AGC
//     then stamps it as a label value on every worker pod, and those creates fail.
//
// Both present identically to an operator: no worker pod, and GitHub reporting that
// the runner "lost communication". Both are deterministic per tenant-name length,
// not intermittent.
//
// # The rules
//
//  1. [Segment] sanitises one piece of arbitrary input into a valid label segment.
//  2. [Join] composes segments under a total budget. It splits the budget BEFORE
//     joining, so no cut can ever land on a separator, and returns the plain
//     concatenation untouched whenever it already fits — an existing name is never
//     churned by adopting this package.
//  3. [Truncate] shortens one segment by replacing the discarded tail with a hash of
//     the whole segment. Trimming alone is not enough: it shortens the
//     entropy-bearing suffix, and two objects colliding on a name is a worse failure
//     than one the apiserver rejects.
//
// [apilabels]: https://pkg.go.dev/github.com/actions-gateway/github-actions-gateway/api/apilabels
package apinames

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

const (
	// MaxLabelValue is the RFC 1123 ceiling shared by a DNS label, a Kubernetes
	// label value, and a Service name — the tightest limit a derived name meets in
	// practice, and the one to budget against unless a caller knows better.
	MaxLabelValue = 63

	// MaxObjectName is the RFC 1123 subdomain ceiling for an object's metadata.name.
	// It is deliberately NOT the default budget: a name that satisfies only this one
	// is accepted on create and then rejected wherever it is used as a label value.
	MaxObjectName = 253

	// HashLen is the number of hex digits of a SHA-256 digest appended to a
	// sanitised or truncated segment. Hex digits are always legal DNS-label
	// characters, so a segment ending in one can never be rejected for its final
	// character.
	HashLen = 7

	// maxSegmentBody is how much readable input one Segment keeps before the hash
	// suffix, so a Segment is at most maxSegmentBody+1+HashLen = 48 characters.
	maxSegmentBody = 40
)

// ShortHash returns the first n hex digits of the SHA-256 digest of s. n is small
// (6–12 at the call sites) because these disambiguate a truncated name within one
// namespace's object set; they are collision-resistance hints, not identifiers.
func ShortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// Segment converts arbitrary input into a valid DNS-1123 label segment: lowercase
// alphanumerics and hyphens, at most 48 characters, always non-empty, and always
// starting and ending with an alphanumeric character.
//
// The trailing hash is derived from the WHOLE input, so two inputs sharing a
// sanitised prefix still produce distinct segments. fallback names the segment when
// nothing survives sanitisation (an all-punctuation runner label, an empty string);
// it is a caller-visible string because the existing names on live clusters differ —
// "job" on the AGC's worker-pod path, "label" on the GMC's runner-label path — and
// changing either would rename objects that are already running.
//
// Sanitisation is byte-wise: each byte outside [a-z0-9-] becomes one hyphen, so a
// multi-byte rune contributes one hyphen per byte. That is the behaviour tenant
// runner labels have always had, and it is preserved deliberately.
func Segment(s, fallback string) string {
	hash := ShortHash(s, HashLen)
	out := make([]byte, 0, len(s))
	for _, c := range []byte(s) {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '-')
		}
	}
	seg := strings.Trim(string(out), "-")
	if len(seg) > maxSegmentBody {
		seg = strings.TrimRight(seg[:maxSegmentBody], "-")
	}
	if seg == "" {
		seg = fallback
	}
	return seg + "-" + hash
}

// Truncate shortens an already-sanitised segment to at most max characters without
// discarding what makes it unique: the cut tail is replaced by a hash of the whole
// segment, and any hyphen the cut exposes is trimmed, so the result never ends on
// one. hashLen is the digest width to spend on that tail.
//
// A segment already within max is returned unchanged, so adopting Truncate never
// renames a name that already fits. max below 1 is clamped to 1; the result is
// always non-empty and always ends with an alphanumeric character.
func Truncate(s string, max, hashLen int) string {
	if max < 1 {
		max = 1
	}
	if len(s) <= max {
		return s
	}
	hash := ShortHash(s, hashLen)
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

// Join composes parts into one "-"-separated name of at most max characters, each
// part truncated by [Truncate] to its share of the budget.
//
// Splitting the budget across the parts BEFORE joining them is the whole point: a
// cut taken after joining can land on a separator, which is how a worker pod name
// came to end in a hyphen and be rejected by the apiserver for every job (Q467).
// Here every part is truncated within itself, so a separator is never reachable.
//
// The budget is shared out by [Shares], which leaves every part that already fits
// untouched — so a name under the limit is returned as the plain concatenation of
// its parts, byte for byte, and only a name that would have overrun changes shape.
// Empty parts are dropped rather than emitting a doubled separator.
//
// max must leave room for at least one character per part plus the separators
// between them (max >= 2*len(parts)-1 once empty parts are dropped); below that the
// result cannot honour the budget, since [Truncate] never returns an empty segment.
// Every caller budgets against [MaxLabelValue] or the v2 52-char cap, both of which
// are far above that floor for the two- and three-part names derived here.
func Join(max int, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	lens := make([]int, len(kept))
	for i, p := range kept {
		lens[i] = len(p)
	}
	budget := Shares(lens, max-(len(kept)-1))
	for i := range kept {
		kept[i] = Truncate(kept[i], budget[i], HashLen)
	}
	return strings.Join(kept, "-")
}

// Shares distributes avail characters across parts of the given natural lengths.
// Every part that fits within an equal share keeps its full length and releases the
// surplus, smallest first; whatever is left is divided evenly among the parts that
// do not fit, with the remainder going to the LAST of them.
//
// The result is that a short part is never padded at a long one's expense, a long
// part is never starved by its neighbour, and — because a part that already fits is
// returned at its natural length — the common case is no truncation at all.
//
// Exported so a caller composing names by hand can budget the same way [Join] does.
func Shares(lens []int, avail int) []int {
	n := len(lens)
	out := make([]int, n)
	open := make([]int, n)
	for i := range open {
		open[i] = i
	}
	remaining := avail

	// Release the surplus of every part that fits within the current equal share,
	// shortest first: satisfying one raises the share available to the rest.
	for len(open) > 0 {
		fair := remaining / len(open)
		next := -1
		for _, idx := range open {
			if lens[idx] > fair {
				continue
			}
			if next == -1 || lens[idx] < lens[next] {
				next = idx
			}
		}
		if next == -1 {
			break // nothing left that fits; the rest all share what remains
		}
		out[next] = lens[next]
		remaining -= lens[next]
		open = remove(open, next)
	}

	// Split what is left evenly among the parts that cannot fit. Extra characters
	// from an uneven division go to the last parts, so the earlier (more
	// human-meaningful) segments have a stable share.
	if k := len(open); k > 0 {
		base, extra := remaining/k, remaining%k
		sort.Ints(open)
		for pos, idx := range open {
			out[idx] = base
			if pos >= k-extra {
				out[idx]++
			}
		}
	}
	return out
}

// remove deletes the first occurrence of v from s, preserving order.
func remove(s []int, v int) []int {
	for i, x := range s {
		if x == v {
			return append(s[:i:i], s[i+1:]...)
		}
	}
	return s
}
