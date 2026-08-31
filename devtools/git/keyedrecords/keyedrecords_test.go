package keyedrecords

import (
	"errors"
	"strings"
	"testing"
)

// firstField keys a record on its leading token, so a test record reads
// "A text" and the rest of the line is the payload the merge rules compare.
// The literal "junk" is the unparseable record.
func firstField(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 || f[0] == "junk" {
		return ""
	}
	return f[0]
}

func lines(s ...string) []string { return s }

func merge(t *testing.T, base, ours, theirs []string) []string {
	t.Helper()
	got, err := Merge(base, ours, theirs, firstField)
	if err != nil {
		t.Fatalf("Merge: unexpected error %v", err)
	}
	return got
}

func refuse(t *testing.T, base, ours, theirs []string, want string) {
	t.Helper()
	got, err := Merge(base, ours, theirs, firstField)
	if err == nil {
		t.Fatalf("Merge: want refusal %q, got clean result %q", want, got)
	}
	var u *Uncertain
	if !errors.As(err, &u) {
		t.Fatalf("Merge: want *Uncertain, got %T", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Merge: reason %q does not contain %q", err.Error(), want)
	}
}

func requireSeq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("record count: want %d %q, got %d %q", len(want), want, len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

// --- the per-key rules ------------------------------------------------------

func TestRules(t *testing.T) {
	tests := []struct {
		name               string
		base, ours, theirs []string
		want               []string
	}{
		{
			name: "untouched on both sides survives",
			base: lines("A one"), ours: lines("A one"), theirs: lines("A one"),
			want: lines("A one"),
		},
		{
			name: "added on ours is present",
			base: lines("A one"), ours: lines("A one", "B two"), theirs: lines("A one"),
			want: lines("A one", "B two"),
		},
		{
			name: "added on theirs is present",
			base: lines("A one"), ours: lines("A one"), theirs: lines("A one", "B two"),
			want: lines("A one", "B two"),
		},
		{
			name: "deleted on ours is deleted",
			base: lines("A one", "B two"), ours: lines("A one"), theirs: lines("A one", "B two"),
			want: lines("A one"),
		},
		{
			name: "deleted on theirs is deleted",
			base: lines("A one", "B two"), ours: lines("A one", "B two"), theirs: lines("A one"),
			want: lines("A one"),
		},
		{
			name: "deleted on both is deleted",
			base: lines("A one", "B two"), ours: lines("A one"), theirs: lines("A one"),
			want: lines("A one"),
		},
		{
			name: "changed on ours only takes that change",
			base: lines("A one"), ours: lines("A edited"), theirs: lines("A one"),
			want: lines("A edited"),
		},
		{
			name: "changed on theirs only takes that change",
			base: lines("A one"), ours: lines("A one"), theirs: lines("A edited"),
			want: lines("A edited"),
		},
		{
			name: "changed identically on both takes that change",
			base: lines("A one"), ours: lines("A edited"), theirs: lines("A edited"),
			want: lines("A edited"),
		},
		{
			name: "same new key added identically on both",
			base: lines("A one"), ours: lines("A one", "B two"), theirs: lines("A one", "B two"),
			want: lines("A one", "B two"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireSeq(t, merge(t, tc.base, tc.ours, tc.theirs), tc.want...)
		})
	}
}

func TestRulesRefuse(t *testing.T) {
	tests := []struct {
		name               string
		base, ours, theirs []string
		want               string
	}{
		{
			name: "changed differently on both sides",
			base: lines("A one"), ours: lines("A ours"), theirs: lines("A theirs"),
			want: "A was changed differently on both sides",
		},
		{
			name: "deleted on theirs, changed on ours",
			base: lines("A one", "B two"), ours: lines("A one", "B edited"), theirs: lines("A one"),
			want: "B was deleted on one side and changed on the other",
		},
		{
			name: "deleted on ours, changed on theirs",
			base: lines("A one", "B two"), ours: lines("A one"), theirs: lines("A one", "B edited"),
			want: "B was deleted on one side and changed on the other",
		},
		{
			name: "same new key filed differently on both sides",
			base: lines("A one"), ours: lines("A one", "B ours"), theirs: lines("A one", "B theirs"),
			want: "B was filed on both sides with different content",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refuse(t, tc.base, tc.ours, tc.theirs, tc.want)
		})
	}
}

// --- malformed input --------------------------------------------------------

func TestUnparseableRecordIsRefusedPerSide(t *testing.T) {
	good := lines("A one")
	bad := lines("junk here")
	tests := []struct {
		name, want         string
		base, ours, theirs []string
	}{
		{"base", "base: not a well-formed record", bad, good, good},
		{"ours", "ours: not a well-formed record", good, bad, good},
		{"theirs", "theirs: not a well-formed record", good, good, bad},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refuse(t, tc.base, tc.ours, tc.theirs, tc.want)
		})
	}
}

func TestDuplicateKeyInOneSideIsRefused(t *testing.T) {
	refuse(t, lines("A one"), lines("A one", "A again"), lines("A one"),
		"ours: A appears twice in the block")
}

func TestBlankLinesAreSkipped(t *testing.T) {
	requireSeq(t, merge(t, lines("A one"), lines("", "A one", "   "), lines("A one")), "A one")
}

// --- order reconstruction ---------------------------------------------------
//
// Nothing in the four driver suites reaches these cases directly: they drive a
// whole file merge, so a reordering is only ever observed through the rendered
// result. This is the logic the awk could not expose.

func TestOrderBothSidesAgree(t *testing.T) {
	base := lines("A a", "B b", "C c")
	requireSeq(t, merge(t, base, base, base), "A a", "B b", "C c")
}

func TestOrderOursReorderedTheirsDidNot(t *testing.T) {
	base := lines("A a", "B b", "C c")
	ours := lines("C c", "A a", "B b")
	requireSeq(t, merge(t, base, ours, base), "C c", "A a", "B b")
}

func TestOrderTheirsReorderedOursDidNot(t *testing.T) {
	base := lines("A a", "B b", "C c")
	theirs := lines("B b", "C c", "A a")
	requireSeq(t, merge(t, base, base, theirs), "B b", "C c", "A a")
}

func TestOrderBothSidesReorderedIsRefused(t *testing.T) {
	base := lines("A a", "B b", "C c")
	ours := lines("C c", "A a", "B b")
	theirs := lines("B b", "A a", "C c")
	refuse(t, base, ours, theirs, "rows were reordered on both sides")
}

func TestOrderBothReorderedIdenticallyIsAccepted(t *testing.T) {
	base := lines("A a", "B b", "C c")
	moved := lines("C c", "B b", "A a")
	requireSeq(t, merge(t, base, moved, moved), "C c", "B b", "A a")
}

// --- splice positions -------------------------------------------------------

func TestAdditionSplicesAtItsOwnPosition(t *testing.T) {
	base := lines("A a", "C c")
	ours := lines("A a", "B b", "C c")
	theirs := lines("A a", "C c", "D d")
	requireSeq(t, merge(t, base, ours, theirs), "A a", "B b", "C c", "D d")
}

func TestAdditionAtTheHeadOfEachSide(t *testing.T) {
	base := lines("C c")
	ours := lines("A a", "C c")
	theirs := lines("B b", "C c")
	requireSeq(t, merge(t, base, ours, theirs), "A a", "B b", "C c")
}

func TestAdditionsPastTheLastSkeletonEntry(t *testing.T) {
	base := lines("A a")
	ours := lines("A a", "B b")
	theirs := lines("A a", "C c")
	requireSeq(t, merge(t, base, ours, theirs), "A a", "B b", "C c")
}

func TestEmptyBaseTakesBothSidesAdditions(t *testing.T) {
	requireSeq(t, merge(t, nil, lines("A a"), lines("B b")), "A a", "B b")
}

func TestEverythingDeletedIsAnEmptyResult(t *testing.T) {
	requireSeq(t, merge(t, lines("A a"), nil, nil))
}

// --- completeness -----------------------------------------------------------

// Every surviving record appears exactly once whatever the ordering pass did.
// This is the invariant the awk's backstop existed to protect and could not
// assert.
func TestNoSurvivingRecordIsEverDropped(t *testing.T) {
	base := lines("A a", "B b", "C c", "D d")
	ours := lines("D d", "A a", "X x", "C c")
	theirs := lines("A a", "B b", "C c", "D d", "Y y")
	got, err := Merge(base, ours, theirs, firstField)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	seen := map[string]int{}
	for _, line := range got {
		seen[firstField(line)]++
	}
	for _, want := range []string{"A", "C", "D", "X", "Y"} {
		if seen[want] != 1 {
			t.Errorf("key %s: want exactly 1 occurrence, got %d (result %q)", want, seen[want], got)
		}
	}
	if _, ok := seen["B"]; ok {
		t.Errorf("B was deleted on ours and untouched on theirs, so it must not survive: %q", got)
	}
}
