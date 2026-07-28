package apinames_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The two implementations this package replaced, verbatim. They are the contract:
// their outputs are RunnerGroup names, worker pod names, and Secret names on
// running clusters, so a divergence would rename live objects. The differential
// tests below are what allow the originals to be deleted with confidence.

var legacyDNSLabelRe = regexp.MustCompile(`[^a-z0-9-]`)

// legacySafeName is the AGC's former provisioner.safeName.
func legacySafeName(s string) string {
	hash := apinames.ShortHash(s, 7)
	s = strings.ToLower(s)
	s = legacyDNSLabelRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	s = strings.TrimRight(s, "-")
	if s == "" {
		s = "job"
	}
	return s + "-" + hash
}

// legacyLabelSafe is the GMC's former controller.labelSafe (and its byte-for-byte
// replica in the migrate package).
func legacyLabelSafe(s string) string {
	hash := apinames.ShortHash(s, 7)
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
	if len(seg) > 40 {
		seg = strings.TrimRight(seg[:40], "-")
	}
	if seg == "" {
		seg = "label"
	}
	return seg + "-" + hash
}

// asciiCorpus covers every input shape the two legacy implementations could see:
// Kubernetes object names, arbitrary runner labels, and the degenerate cases.
var asciiCorpus = []string{
	"", "a", "-", "---", "..", "a.b", "A", "ABC", "MyRunnerGroup",
	"self-hosted", "gpu/a100", "gpu_a100", "linux-x64", "ubuntu-22.04",
	"dogfood-migrate", "dfmigrate", "gag-migrate-v1", "-leading", "trailing-",
	"UPPER_CASE_LABEL", "with spaces", "tabs\tand\nnewlines", "sym!@#$%^&*()",
	strings.Repeat("a", 39), strings.Repeat("a", 40), strings.Repeat("a", 41),
	strings.Repeat("a", 63), strings.Repeat("a", 253), strings.Repeat("ab-", 30),
	strings.Repeat("-", 45) + "tail", strings.Repeat("a", 38) + "--" + "b",
	"a" + strings.Repeat("-", 40) + "b",
}

// TestSegmentMatchesLegacyImplementations is the safety net for the consolidation:
// for every reachable input, the shared Segment reproduces the exact bytes the two
// implementations it replaced would have produced. A regression here renames live
// objects — RunnerGroup CRs, worker pods, agent Secrets — rather than merely
// producing an ugly name.
func TestSegmentMatchesLegacyImplementations(t *testing.T) {
	for _, in := range asciiCorpus {
		if got, want := apinames.Segment(in, "job"), legacySafeName(in); got != want {
			t.Errorf("Segment(%q, \"job\") = %q, legacy safeName = %q", in, got, want)
		}
		if got, want := apinames.Segment(in, "label"), legacyLabelSafe(in); got != want {
			t.Errorf("Segment(%q, \"label\") = %q, legacy labelSafe = %q", in, got, want)
		}
	}
}

// TestSegmentNonASCIIFollowsTheLabelPath pins the one deliberate divergence. The
// legacy implementations disagreed with each other on multi-byte input: labelSafe
// mapped each BYTE to a hyphen, safeName lowercased first and mapped each RUNE.
// Segment keeps labelSafe's byte-wise behaviour, because its inputs are arbitrary
// tenant runner labels that may already have produced such a name on a cluster.
// safeName's inputs cannot reach this: they are Kubernetes object names and GitHub
// plan/job IDs, all ASCII by construction.
func TestSegmentNonASCIIFollowsTheLabelPath(t *testing.T) {
	// The multi-byte run has to sit between ASCII characters for the two to differ:
	// a string that sanitises to hyphens alone is trimmed to the fallback either way.
	const in = "aラb" // "ラ" is one rune, three bytes
	if got, want := apinames.Segment(in, "label"), legacyLabelSafe(in); got != want {
		t.Errorf("Segment(%q) = %q, want the byte-wise legacy result %q", in, got, want)
	}
	if got, legacy := apinames.Segment(in, "job"), legacySafeName(in); got == legacy {
		t.Errorf("expected Segment to diverge from the rune-wise legacy safeName on %q (both %q)", in, got)
	} else {
		t.Logf("byte-wise %q vs rune-wise %q — unreachable from safeName's inputs", got, legacy)
	}
}

func TestSegmentAlwaysValid(t *testing.T) {
	for _, in := range append(asciiCorpus, "ランナー", "café", "\x00\xff") {
		got := apinames.Segment(in, "job")
		if errs := validation.IsDNS1123Label(got); len(errs) > 0 {
			t.Errorf("Segment(%q) = %q: %s", in, got, strings.Join(errs, "; "))
		}
		if len(got) > 48 {
			t.Errorf("Segment(%q) = %q: len %d exceeds 48", in, got, len(got))
		}
	}
}

func TestTruncate(t *testing.T) {
	const (
		uuidSeg  = "a20852f8-1e2b-4c3d-9f10-77b6d4c1a9e5"
		hyphenAt = 8 // the first hyphen in uuidSeg
		// allHyphens cannot reach Truncate through Join (Segment strips leading
		// hyphens first); it pins the branch that would otherwise emit a leading "-".
		allHyphens = "----------abc"
	)
	h := func(s string) string { return apinames.ShortHash(s, apinames.HashLen) }

	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits exactly", "abcdefg", 7, "abcdefg"},
		{"shorter than budget", "abc", 40, "abc"},
		{"cut on a hyphen trims it", uuidSeg, hyphenAt + 1 + apinames.HashLen, "a20852f8-" + h(uuidSeg)},
		{"cut just before a hyphen", uuidSeg, hyphenAt + apinames.HashLen, "a20852f-" + h(uuidSeg)},
		{"hash only when no head survives", allHyphens, apinames.HashLen + 2, h(allHyphens)},
		{"hash only at exactly the hash length", uuidSeg, apinames.HashLen, h(uuidSeg)},
		{"hash prefix below the hash length", uuidSeg, 3, h(uuidSeg)[:3]},
		{"clamped to one char", uuidSeg, 0, h(uuidSeg)[:1]},
		{"clamped from negative", uuidSeg, -5, h(uuidSeg)[:1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apinames.Truncate(tc.in, tc.max, apinames.HashLen)
			if got != tc.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if got == "" {
				t.Error("must never be empty")
			}
			if strings.HasSuffix(got, "-") || strings.HasPrefix(got, "-") {
				t.Errorf("%q must not start or end on a hyphen", got)
			}
			if limit := max(tc.max, 1); len(got) > limit {
				t.Errorf("len(%q) = %d exceeds %d", got, len(got), limit)
			}
		})
	}
}

// TestTruncateInjective checks the property the uniqueness argument rests on:
// segments sharing a visible prefix but differing past the cut still produce
// distinct results. Budgets below HashLen+2 cannot hold a whole hash and so cannot
// promise this; Shares never hands out one that small (see TestSharesTruncatingFloor).
func TestTruncateInjective(t *testing.T) {
	for _, budget := range []int{9, 12, 27, 28, 44, 48} {
		seen := map[string]int{}
		for i := range 500 {
			s := apinames.Segment(fmt.Sprintf("%s-%d", strings.Repeat("p", 30), i), "job")
			got := apinames.Truncate(s, budget, apinames.HashLen)
			if prev, dup := seen[got]; dup {
				t.Errorf("budget %d: collision between %d and %d: %q", budget, prev, i, got)
			}
			seen[got] = i
		}
	}
}

func TestShares(t *testing.T) {
	cases := []struct {
		name  string
		lens  []int
		avail int
		want  []int
	}{
		{"both fit", []int{10, 20}, 55, []int{10, 20}},
		{"exactly fits", []int{25, 30}, 55, []int{25, 30}},
		{"both over half share evenly, extra to the last", []int{46, 44}, 55, []int{27, 28}},
		{"short first keeps its length", []int{9, 48}, 55, []int{9, 46}},
		{"short second keeps its length", []int{48, 9}, 55, []int{46, 9}},
		{"both at the maximum", []int{48, 48}, 55, []int{27, 28}},
		{"odd budget", []int{40, 40}, 41, []int{20, 21}},
		// The worker-pod shape: a short fixed prefix releases its surplus to the two
		// segments, which then split what is left. This must keep producing 6/27/28,
		// or every worker pod in the fleet is renamed.
		{"worker pod prefix, owner, id", []int{6, 46, 44}, 61, []int{6, 27, 28}},
		{"three parts all short", []int{5, 6, 7}, 61, []int{5, 6, 7}},
		{"three parts, two long", []int{6, 60, 60}, 61, []int{6, 27, 28}},
		{"single part", []int{100}, 61, []int{61}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apinames.Shares(tc.lens, tc.avail)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("Shares(%v, %d) = %v, want %v", tc.lens, tc.avail, got, tc.want)
			}
			sum := 0
			for _, v := range got {
				sum += v
			}
			if sum > tc.avail {
				t.Errorf("shares sum to %d, over the %d budget", sum, tc.avail)
			}
		})
	}
}

// TestSharesTruncatingFloor pins the floor the uniqueness argument needs: a part is
// only ever cut short of its natural length when it still gets at least an equal
// share of the budget, so a truncated segment always has room for a readable head
// AND a whole hash.
func TestSharesTruncatingFloor(t *testing.T) {
	avail := apinames.MaxLabelValue - len("runner") - 2
	half := avail / 2
	for a := apinames.HashLen + 2; a <= 48; a++ {
		for b := apinames.HashLen + 2; b <= 48; b++ {
			got := apinames.Shares([]int{a, b}, avail)
			if got[0] < a && got[0] < half {
				t.Fatalf("Shares(%d, %d) starved the first part: %v", a, b, got)
			}
			if got[1] < b && got[1] < half {
				t.Fatalf("Shares(%d, %d) starved the second part: %v", a, b, got)
			}
		}
	}
}

func TestJoinLeavesFittingNamesUntouched(t *testing.T) {
	cases := [][]string{
		{"dogfood-migrate", "gag-migrate-v1-18c32e1"},
		{"gw", "0"},
		{"runner", "abc-1234567", "def-7654321"},
	}
	for _, parts := range cases {
		want := strings.Join(parts, "-")
		if got := apinames.Join(apinames.MaxLabelValue, parts...); got != want {
			t.Errorf("Join(%v) = %q, want the plain concatenation %q", parts, got, want)
		}
	}
}

// TestJoinWorkerPodGolden pins the exact worker pod name shipped for the Q467 case.
// Consolidating the derivation into this package must not rename worker pods.
func TestJoinWorkerPodGolden(t *testing.T) {
	owner := apinames.Segment("dogfood-migrate-gag-migrate-v1-18c32e1", "job")
	id := apinames.Segment("a20852f8-1e2b-4c3d-9f10-77b6d4c1a9e5", "job")
	const want = "runner-dogfood-migrate-gag-3994471-a20852f8-1e2b-4c3d-9-bb3b3f9"
	if got := apinames.Join(apinames.MaxLabelValue, "runner", owner, id); got != want {
		t.Errorf("worker pod name = %q, want %q", got, want)
	}
}

func TestJoinIsBoundedAndValid(t *testing.T) {
	for _, aLen := range []int{0, 1, 15, 40, 63, 253} {
		for _, bLen := range []int{0, 1, 15, 40, 63, 253} {
			for _, max := range []int{apinames.MaxLabelValue, 52, 20, 10} {
				a, b := strings.Repeat("a", aLen), strings.Repeat("b", bLen)
				got := apinames.Join(max, a, b)
				if got == "" {
					continue // both parts empty
				}
				if len(got) > max {
					t.Errorf("Join(%d, %d chars, %d chars) = %q: len %d over budget", max, aLen, bLen, got, len(got))
				}
				if errs := validation.IsDNS1123Label(got); len(errs) > 0 && max <= apinames.MaxLabelValue {
					t.Errorf("Join(%d, %d, %d) = %q: %s", max, aLen, bLen, got, strings.Join(errs, "; "))
				}
			}
		}
	}
}

// TestJoinHonoursTheBudgetDownToItsFloor walks max down to the documented floor —
// one character per part plus the separators — for two- and three-part names, the
// shapes actually derived here.
func TestJoinHonoursTheBudgetDownToItsFloor(t *testing.T) {
	two := []string{strings.Repeat("a", 48), strings.Repeat("b", 48)}
	three := append([]string{"runner"}, two...)
	for max := 63; max >= 3; max-- {
		if got := apinames.Join(max, two...); len(got) > max {
			t.Errorf("Join(%d, 2 parts) = %q: len %d over budget", max, got, len(got))
		}
	}
	for max := 63; max >= 5; max-- {
		if got := apinames.Join(max, three...); len(got) > max {
			t.Errorf("Join(%d, 3 parts) = %q: len %d over budget", max, got, len(got))
		}
	}
}

func TestJoinDropsEmptyParts(t *testing.T) {
	if got, want := apinames.Join(63, "a", "", "b"), "a-b"; got != want {
		t.Errorf("Join with an empty part = %q, want %q", got, want)
	}
	if got := apinames.Join(63, "", ""); got != "" {
		t.Errorf("Join of only empty parts = %q, want \"\"", got)
	}
}

// TestJoinUnique is the counterweight to truncation: two distinct inputs must not
// collapse onto one name, because two objects sharing a name is a worse failure
// than one the apiserver rejects.
func TestJoinUnique(t *testing.T) {
	seen := map[string]string{}
	for i := range 2000 {
		owner := apinames.Segment(fmt.Sprintf("%s-%d", strings.Repeat("tenant-", 8), i), "label")
		id := apinames.Segment(fmt.Sprintf("%08x-1e2b-4c3d-9f10-77b6d4c1a9e5", i), "job")
		name := apinames.Join(apinames.MaxLabelValue, "runner", owner, id)
		if prev, dup := seen[name]; dup {
			t.Fatalf("collision between %q and %q: %q", prev, owner, name)
		}
		seen[name] = owner
	}
}

// FuzzJoin asserts the invariant over arbitrary input: whatever the parts are, the
// composed name is one the apiserver accepts as a label value.
func FuzzJoin(f *testing.F) {
	f.Add("dogfood-migrate-gag-migrate-v1-18c32e1", "a20852f8-1e2b-4c3d-9f10-77b6d4c1a9e5")
	f.Add("", "")
	f.Add("---", "-")
	f.Add(strings.Repeat("a", 253), strings.Repeat("b", 253))
	f.Add("MyGateway", "gpu/a100")
	f.Fuzz(func(t *testing.T, a, b string) {
		name := apinames.Join(apinames.MaxLabelValue, apinames.Segment(a, "job"), apinames.Segment(b, "label"))
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			t.Fatalf("Join(Segment(%q), Segment(%q)) = %q: %s", a, b, name, strings.Join(errs, "; "))
		}
		if len(name) > apinames.MaxLabelValue {
			t.Fatalf("Join(%q, %q) = %q: len %d", a, b, name, len(name))
		}
	})
}
