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

// --- BaseThenAdditions -------------------------------------------------------
//
// The other order, for a block whose record order carries nothing: the gate
// lists in mk/gate-lists.mk, which make expands as sets. What these assert is
// that the survival rules are the ones above — one set of per-key rules serves
// both orders — while a reorder stops being something to infer from or to
// refuse over.

func mergeSet(t *testing.T, base, ours, theirs []string) []string {
	t.Helper()
	got, err := MergeOrdered(base, ours, theirs, firstField, BaseThenAdditions)
	if err != nil {
		t.Fatalf("MergeOrdered: unexpected error %v", err)
	}
	return got
}

func TestBaseThenAdditionsKeepsBaseOrderThenEachSidesAdditions(t *testing.T) {
	got := mergeSet(t,
		lines("A a", "B b", "C c"),
		lines("A a", "O o", "B b", "C c"),
		lines("A a", "B b", "T t", "C c"))
	want := lines("A a", "B b", "C c", "O o", "T t")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The refusal Reconstruct makes over a reorder is exactly what must not happen
// here: a Makefile list whose entries moved has not changed at all.
func TestBaseThenAdditionsAcceptsAReorderOnBothSides(t *testing.T) {
	got := mergeSet(t,
		lines("A a", "B b", "C c"),
		lines("C c", "A a", "B b"),
		lines("B b", "C c", "A a"))
	want := lines("A a", "B b", "C c")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
	// And the same input under the other order is refused, which is what makes
	// the two orders a real choice rather than a spelling.
	if _, err := MergeOrdered(
		lines("A a", "B b", "C c"),
		lines("C c", "A a", "B b"),
		lines("B b", "C c", "A a"), firstField, Reconstruct); err == nil {
		t.Error("Reconstruct accepted a both-sides reorder")
	}
}

// Every survival rule is shared with Reconstruct. A record deleted on one side
// and untouched on the other stays deleted, which is the rule a gate list
// depends on most: a suite deleted deliberately must not come back.
func TestBaseThenAdditionsKeepsTheSurvivalRules(t *testing.T) {
	got := mergeSet(t,
		lines("A a", "B b"),
		lines("A a"),
		lines("A a", "B b", "T t"))
	want := lines("A a", "T t")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("deleted record came back: got %q, want %q", got, want)
	}

	var u *Uncertain
	_, err := MergeOrdered(
		lines("A a"), lines("A ours"), lines("A theirs"), firstField, BaseThenAdditions)
	if !errors.As(err, &u) {
		t.Errorf("an edit/edit was not refused: %v", err)
	}
}

func TestBaseThenAdditionsDropsNoSurvivingRecord(t *testing.T) {
	base := lines("A a", "B b", "C c", "D d")
	ours := lines("D d", "A a", "O o", "C c")
	theirs := lines("B b", "T t", "A a", "C c")
	got := mergeSet(t, base, ours, theirs)
	// A and C survive on every side; B and D were each deleted on one side and
	// untouched on the other; O and T are one-sided additions.
	want := lines("A a", "C c", "O o", "T t")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Merge is MergeOrdered's Reconstruct, so a caller that wants the other order
// has to say so. Asserting it here keeps a default flip from being silent.
func TestMergeDefaultsToReconstruct(t *testing.T) {
	base := lines("A a", "B b", "C c")
	ours := lines("A a", "O o", "B b", "C c")
	theirs := lines("A a", "B b", "C c")
	viaMerge, err := Merge(base, ours, theirs, firstField)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	viaOrdered, err := MergeOrdered(base, ours, theirs, firstField, Reconstruct)
	if err != nil {
		t.Fatalf("MergeOrdered: %v", err)
	}
	if strings.Join(viaMerge, "|") != strings.Join(viaOrdered, "|") {
		t.Errorf("Merge = %q, Reconstruct = %q", viaMerge, viaOrdered)
	}
	// The two orders disagree on this input, so the assertion above is not
	// satisfied by both of them happening to agree.
	viaSet := mergeSet(t, base, ours, theirs)
	if strings.Join(viaMerge, "|") == strings.Join(viaSet, "|") {
		t.Error("the orders agree here, so this input cannot tell them apart")
	}
}
