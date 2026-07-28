package provisioner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

// requireValidPodName asserts that name is a valid DNS-1123 label. Pod names are
// validated by the apiserver as DNS-1123 *subdomains*, which are laxer (dots
// allowed, up to 253 chars); the label rules are the stricter set the AGC holds
// itself to, because a worker pod's name also becomes its hostname.
func requireValidPodName(t *testing.T, name string) {
	t.Helper()
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		t.Fatalf("invalid pod name %q (len %d): %s", name, len(name), strings.Join(errs, "; "))
	}
}

// legacyPodName reproduces the pre-Q467 derivation: build the whole name, then cut
// it at the DNS-label limit. It exists so the boundary tests below can show which
// inputs it corrupted and prove the new derivation covers them.
func legacyPodName(owner, id string) string {
	name := fmt.Sprintf("runner-%s-%s", safeName(owner), safeName(id))
	if len(name) > maxDNSLabelLen {
		name = name[:maxDNSLabelLen]
	}
	return name
}

// uuid returns a distinct, deterministic UUID-shaped string for i. Only the shape
// matters here: the hyphens at indices 8, 13, 18 and 23 are what the naive cut
// landed on.
func uuid(i int) string {
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", i, i&0xffff, i&0xfff, i&0xfff, i)
}

// TestWorkerPodNameQ467Regression pins the live GKE failure: the observed owner and
// plan ID produced a name ending in one of the UUID's hyphens, which the apiserver
// rejected — so no worker pod was ever created and no job ever ran.
func TestWorkerPodNameQ467Regression(t *testing.T) {
	const (
		owner  = "dogfood-migrate-gag-migrate-v1-18c32e1"
		planID = "a20852f8-1e2b-4c3d-9f10-77b6d4c1a9e5"
	)

	legacy := legacyPodName(owner, planID)
	require.Len(t, legacy, maxDNSLabelLen)
	assert.True(t, strings.HasSuffix(legacy, "-"),
		"the reported failure was a name cut on a UUID hyphen; got %q", legacy)
	assert.NotEmpty(t, validation.IsDNS1123Label(legacy),
		"the legacy name must be the invalid one this fix removes")

	name := workerPodName(owner, planID)
	t.Logf("legacy: %q\nfixed:  %q", legacy, name)
	requireValidPodName(t, name)
	assert.Len(t, name, maxDNSLabelLen, "the fixed name should still use the whole budget")
	assert.True(t, strings.HasPrefix(name, "runner-"))
	// Both segments keep a readable head and a hash of their whole value: the owner
	// is still recognisable in `kubectl get pods` and the plan ID is still greppable.
	assert.Contains(t, name, "dogfood-migrate")
	assert.Contains(t, name, "a20852f8")
}

// TestWorkerPodNameOwnerLengthBoundary walks every owner-name length across the
// 63-char cut, including the four that land the naive cut exactly on a UUID hyphen
// and their immediate neighbours. Nothing tested at this boundary before, which is
// why the defect shipped.
func TestWorkerPodNameOwnerLengthBoundary(t *testing.T) {
	const planID = "a20852f8-1e2b-4c3d-9f10-77b6d4c1a9e5"

	// safeName appends "-" plus a 7-char hash, so a sanitised owner of length n
	// yields a segment of length n+8 and the pre-fix cut fell at index 54-(n+8) of
	// the sanitised plan ID. That is a hyphen at index 8, 13, 18 or 23 (the UUID's
	// own) or 36 (safeName's separator before its hash) — five tenant-name lengths,
	// one of them as short as 10 characters, for which no job could ever run.
	affected := map[int]bool{10: true, 23: true, 28: true, 33: true, 38: true}

	for n := 1; n <= 60; n++ {
		t.Run(fmt.Sprintf("owner-len-%d", n), func(t *testing.T) {
			owner := strings.Repeat("a", n)

			name := workerPodName(owner, planID)
			requireValidPodName(t, name)
			assert.LessOrEqual(t, len(name), maxDNSLabelLen)

			// The legacy derivation is invalid for exactly the lengths that put the
			// cut on a hyphen — deterministic per tenant name, not intermittent.
			legacyInvalid := len(validation.IsDNS1123Label(legacyPodName(owner, planID))) > 0
			assert.Equal(t, affected[n], legacyInvalid,
				"legacy validity at owner length %d", n)
		})
	}
}

// TestWorkerPodNameLengthSweep covers every combination of owner and id length
// across the budget, including maximum-length inputs and the point where both
// segments must be truncated.
func TestWorkerPodNameLengthSweep(t *testing.T) {
	for _, ownerLen := range []int{0, 1, 2, 23, 40, 41, 63, 100, 253} {
		for _, idLen := range []int{0, 1, 5, 36, 40, 41, 63, 253} {
			owner := strings.Repeat("o", ownerLen)
			id := strings.Repeat("i", idLen)
			name := workerPodName(owner, id)
			requireValidPodName(t, name)
			assert.LessOrEqualf(t, len(name), maxDNSLabelLen,
				"owner len %d, id len %d: %q", ownerLen, idLen, name)
		}
	}
}

// TestWorkerPodNameDegenerateInputs covers inputs that sanitise to nothing, to
// hyphens only, or to characters that are illegal in a DNS label — the cases where
// a leading hyphen or an empty segment could slip out.
func TestWorkerPodNameDegenerateInputs(t *testing.T) {
	cases := []struct{ name, owner, id string }{
		{"both empty", "", ""},
		{"hyphens only", "---", "-----"},
		{"leading hyphen", "-abc", "-def"},
		{"trailing hyphen", "abc-", "def-"},
		{"uppercase", "MyRunnerGroup", "AB-CD"},
		{"unicode", "ランナー", "ジョブ"},
		{"dots and slashes", "my.group/one", "a/b.c"},
		{"long hyphens then text", strings.Repeat("-", 50) + "tail", uuid(1)},
		{"single char", "a", "b"},
		{"very long", strings.Repeat("x", 500), strings.Repeat("y", 500)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := workerPodName(tc.owner, tc.id)
			requireValidPodName(t, name)
			assert.LessOrEqual(t, len(name), maxDNSLabelLen)
		})
	}
}

// TestWorkerPodNameUnique is the counterweight to truncation: shortening a name
// must not cost uniqueness, since two workers sharing a name is a worse failure
// than one pod the apiserver rejects. Every owner length here truncates the id.
func TestWorkerPodNameUnique(t *testing.T) {
	const jobs = 5000

	for _, ownerLen := range []int{10, 23, 38, 63, 253} {
		t.Run(fmt.Sprintf("owner-len-%d", ownerLen), func(t *testing.T) {
			owner := strings.Repeat("a", ownerLen)
			seen := make(map[string]int, jobs)
			for i := range jobs {
				name := workerPodName(owner, uuid(i))
				if prev, dup := seen[name]; dup {
					t.Fatalf("collision between job %d and job %d: %q", prev, i, name)
				}
				seen[name] = i
			}
		})
	}

	// Distinct owners sharing one id must stay distinct too, even when the owner
	// segment is truncated: it is the *whole* owner that is hashed into the tail,
	// so owners differing only past the cut still separate.
	t.Run("owners differing past the cut", func(t *testing.T) {
		prefix := strings.Repeat("a", 60)
		id := uuid(7)
		seen := map[string]bool{}
		for i := range 200 {
			name := workerPodName(fmt.Sprintf("%s-%d", prefix, i), id)
			require.Falsef(t, seen[name], "collision at owner %d: %q", i, name)
			seen[name] = true
		}
	})

	// Cross product: distinct (owner, id) pairs stay distinct.
	t.Run("owner and id cross product", func(t *testing.T) {
		seen := map[string]bool{}
		for o := range 100 {
			owner := fmt.Sprintf("%s-%d", strings.Repeat("tenant-", 8), o)
			for j := range 50 {
				name := workerPodName(owner, uuid(j))
				require.Falsef(t, seen[name], "collision at owner %d job %d: %q", o, j, name)
				seen[name] = true
			}
		}
	})
}

// TestWorkerPodNameDeterministic pins the property the v2 scale-set path depends on:
// the creating side and the completion-stamping side derive the same name from the
// same inputs, and a replayed delivery of one job maps onto its own pod.
func TestWorkerPodNameDeterministic(t *testing.T) {
	for _, ownerLen := range []int{5, 30, 60} {
		owner := strings.Repeat("z", ownerLen)
		id := uuid(42)
		assert.Equal(t, workerPodName(owner, id), workerPodName(owner, id))
		assert.Equal(t, workerPodName(owner, id), scaleSetPodName(owner, id))
	}
}

func TestTruncateSegment(t *testing.T) {
	const (
		uuidSeg  = "a20852f8-1e2b-4c3d-9f10-77b6d4c1a9e5"
		hyphenAt = 8 // the first hyphen in uuidSeg
		// allHyphens can never reach truncateSegment through workerPodName (safeName
		// strips leading hyphens first); it pins the defensive branch that would
		// otherwise emit a leading "-".
		allHyphens = "----------abc"
	)

	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits exactly", "abcdefg", 7, "abcdefg"},
		{"shorter than budget", "abc", 40, "abc"},
		{"cut on a hyphen trims it", uuidSeg, hyphenAt + 1 + hashLen, "a20852f8" + "-" + shortHash(uuidSeg)},
		{"cut just before a hyphen", uuidSeg, hyphenAt + hashLen, "a20852f" + "-" + shortHash(uuidSeg)},
		{"hash only when no head survives", allHyphens, hashLen + 2, shortHash(allHyphens)},
		{"hash only at exactly the hash length", uuidSeg, hashLen, shortHash(uuidSeg)[:hashLen]},
		{"hash prefix below the hash length", uuidSeg, 3, shortHash(uuidSeg)[:3]},
		{"clamped to one char", uuidSeg, 0, shortHash(uuidSeg)[:1]},
		{"clamped from negative", uuidSeg, -5, shortHash(uuidSeg)[:1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateSegment(tc.in, tc.max)
			assert.Equal(t, tc.want, got)
			assert.NotEmpty(t, got)
			assert.LessOrEqual(t, len(got), max(tc.max, 1))
			assert.False(t, strings.HasSuffix(got, "-"), "must never end on a hyphen")
			assert.False(t, strings.HasPrefix(got, "-"), "must never start with a hyphen")
		})
	}
}

// TestTruncateSegmentInjective checks the property the pod-name uniqueness argument
// rests on: segments sharing a visible prefix but differing past the cut still
// produce distinct results. Budgets below hashLen+2 cannot hold a whole hash and so
// cannot promise this; splitBudget never hands out one that small (see
// TestSplitBudgetTruncatingShareFloor).
func TestTruncateSegmentInjective(t *testing.T) {
	for _, budget := range []int{9, 12, 27, 28, 44, 48} {
		seen := map[string]int{}
		for i := range 500 {
			s := safeName(fmt.Sprintf("%s-%d", strings.Repeat("p", 30), i))
			got := truncateSegment(s, budget)
			if prev, dup := seen[got]; dup {
				t.Errorf("budget %d: collision between %d and %d: %q", budget, prev, i, got)
			}
			seen[got] = i
		}
	}
}

func TestSplitBudget(t *testing.T) {
	cases := []struct {
		name         string
		a, b, avail  int
		wantA, wantB int
	}{
		{"both fit", 10, 20, 55, 10, 20},
		{"exactly fits", 25, 30, 55, 25, 30},
		{"both over half share evenly", 46, 44, 55, 27, 28},
		{"short a keeps its length", 9, 48, 55, 9, 46},
		{"short b keeps its length", 48, 9, 55, 46, 9},
		{"both at the maximum", 48, 48, 55, 27, 28},
		{"odd budget", 40, 40, 41, 20, 21},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotA, gotB := splitBudget(tc.a, tc.b, tc.avail)
			assert.Equal(t, tc.wantA, gotA, "owner share")
			assert.Equal(t, tc.wantB, gotB, "id share")
			assert.LessOrEqual(t, gotA+gotB, tc.avail)
			assert.Positive(t, gotA)
			assert.Positive(t, gotB)
		})
	}
}

// TestSplitBudgetTruncatingShareFloor pins the floor the uniqueness argument needs:
// a segment is only ever cut short of its natural length when it still gets at least
// half the budget — so a truncated worker-pod segment is never shorter than 27 chars,
// leaving room for a readable head *and* the whole hash.
func TestSplitBudgetTruncatingShareFloor(t *testing.T) {
	avail := maxDNSLabelLen - len(workerPodPrefix) - 2
	half := avail / 2
	// safeName output is 9..48 chars, so those are the only natural lengths reachable.
	for a := hashLen + 2; a <= 48; a++ {
		for b := hashLen + 2; b <= 48; b++ {
			gotA, gotB := splitBudget(a, b, avail)
			assert.LessOrEqual(t, gotA+gotB, avail)
			if gotA < a {
				assert.GreaterOrEqualf(t, gotA, half, "owner share for natural (%d, %d)", a, b)
			}
			if gotB < b {
				assert.GreaterOrEqualf(t, gotB, half, "id share for natural (%d, %d)", a, b)
			}
		}
	}
}

// TestSafeNameAlwaysValid guards the per-segment invariant workerPodName builds on.
func TestSafeNameAlwaysValid(t *testing.T) {
	inputs := []string{"", "-", "---", "..", "a", "ABC", "ランナー", strings.Repeat("-a", 100),
		strings.Repeat("a", 40) + "-" + strings.Repeat("b", 40), "a/b/c", "-leading", "trailing-"}
	for _, in := range inputs {
		got := safeName(in)
		requireValidPodName(t, got)
		assert.LessOrEqual(t, len(got), 48)
		assert.GreaterOrEqual(t, len(got), hashLen+2)
	}
}

// FuzzWorkerPodName asserts the invariant over arbitrary inputs: whatever an owner
// is called and whatever a job ID looks like, the derived pod name is one the
// apiserver accepts.
func FuzzWorkerPodName(f *testing.F) {
	f.Add("dogfood-migrate-gag-migrate-v1-18c32e1", "a20852f8-1e2b-4c3d-9f10-77b6d4c1a9e5")
	f.Add("", "")
	f.Add("---", "-")
	f.Add(strings.Repeat("a", 253), strings.Repeat("b", 253))
	f.Add("MyGroup", "12345678")
	f.Fuzz(func(t *testing.T, owner, id string) {
		name := workerPodName(owner, id)
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			t.Fatalf("workerPodName(%q, %q) = %q: %s", owner, id, name, strings.Join(errs, "; "))
		}
	})
}
