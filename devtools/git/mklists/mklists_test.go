package mklists

import (
	"reflect"
	"strings"
	"testing"
)

var vars = []string{"ALPHA", "BETA"}

func makefile(lines ...string) []string { return lines }

// assertContinued is the property BlockEntries is blind to. Reading the entries
// back finds them all whether or not the lines are joined, so it cannot tell a
// wrapped assignment from a list of separate ones — and a rendered block that
// drops a continuation backslash assigns only its first line and silently
// empties the gate list.
func assertContinued(t *testing.T, lines []string) {
	t.Helper()
	for i, line := range lines[:len(lines)-1] {
		if !strings.HasSuffix(line, `\`) {
			t.Errorf("line %d does not continue: %q", i, line)
		}
	}
	if strings.HasSuffix(lines[len(lines)-1], `\`) {
		t.Errorf("last line continues into nothing: %q", lines[len(lines)-1])
	}
}

func TestLiftReplacesEachAssignmentWithItsSlot(t *testing.T) {
	doc, err := Lift(makefile(
		"# prose",
		"ALPHA := a b",
		"",
		"BETA := c",
		".PHONY: all",
	), vars)
	if err != nil {
		t.Fatalf("Lift: %v", err)
	}
	want := []string{"# prose", Slot("ALPHA"), "", Slot("BETA"), ".PHONY: all"}
	if !reflect.DeepEqual(doc.Body, want) {
		t.Errorf("body = %q, want %q", doc.Body, want)
	}
	if got := doc.Blocks["ALPHA"].Entries; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ALPHA entries = %q", got)
	}
}

func TestLiftKeepsAContinuedAssignmentWhole(t *testing.T) {
	doc, err := Lift(makefile(
		"ALPHA := a b \\",
		"         c \\",
		"         d",
		"BETA := e",
		"other := not-managed",
	), vars)
	if err != nil {
		t.Fatalf("Lift: %v", err)
	}
	if got := doc.Blocks["ALPHA"].Entries; !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("entries = %q", got)
	}
	if got := len(doc.Blocks["ALPHA"].Lines); got != 3 {
		t.Errorf("lines = %d, want 3", got)
	}
	// The unmanaged assignment is body, not a block: only named lists are
	// merged as sets.
	if !reflect.DeepEqual(doc.Body, []string{Slot("ALPHA"), Slot("BETA"), "other := not-managed"}) {
		t.Errorf("body = %q", doc.Body)
	}
}

func TestLiftRefusesAMissingVariable(t *testing.T) {
	_, err := Lift(makefile("ALPHA := a"), vars)
	if err == nil || !strings.Contains(err.Error(), "BETA is not assigned in this file") {
		t.Fatalf("err = %v, want BETA not assigned", err)
	}
}

func TestLiftRefusesAVariableAssignedTwice(t *testing.T) {
	_, err := Lift(makefile("ALPHA := a", "BETA := b", "ALPHA := c"), vars)
	if err == nil || !strings.Contains(err.Error(), "ALPHA is assigned more than once") {
		t.Fatalf("err = %v, want ALPHA assigned twice", err)
	}
}

func TestLiftRefusesAnUnterminatedContinuation(t *testing.T) {
	_, err := Lift(makefile("BETA := b", "ALPHA := a \\"), vars)
	if err == nil || !strings.Contains(err.Error(), "ALPHA ends in an unterminated continuation") {
		t.Fatalf("err = %v, want unterminated continuation", err)
	}
}

// A trailing backslash is the continuation marker, never an entry. Reading it
// as one would put a bare `\` into the gate list and break every build.
func TestEntriesDropTheContinuationBackslash(t *testing.T) {
	doc, err := Lift(makefile("ALPHA := a \\", "   b", "BETA := c"), vars)
	if err != nil {
		t.Fatalf("Lift: %v", err)
	}
	for _, e := range doc.Blocks["ALPHA"].Entries {
		if strings.Contains(e, `\`) {
			t.Fatalf("entry %q carries the continuation backslash", e)
		}
	}
}

func TestStyleOfReadsTheOperatorAndIndentFromTheBlock(t *testing.T) {
	st := StyleOf(&Block{Name: "ALPHA", Lines: []string{
		"ALPHA ?= a \\",
		"\t\tb",
	}})
	if st.Op != "?=" {
		t.Errorf("op = %q, want ?=", st.Op)
	}
	if st.Indent != "\t\t" {
		t.Errorf("indent = %q, want two tabs", st.Indent)
	}
}

// The indent has to come from the block's own continuations. Taking it from
// anywhere else in the file picks up a recipe's tab and re-indents the list to
// something make reads as a command.
func TestStyleOfFallsBackWhenTheBlockHasNoIndentOfItsOwn(t *testing.T) {
	st := StyleOf(&Block{Name: "ALPHA", Lines: []string{"ALPHA := a \\", "b"}})
	if st.Indent != defaultIndent {
		t.Errorf("indent = %q, want the default", st.Indent)
	}
}

func TestRenderWrapsAtTheStyleWidth(t *testing.T) {
	st := Style{Op: ":=", Indent: "  ", Width: 20}
	got := Render("ALPHA", st, []string{"aaaa", "bbbb", "cccc", "dddd"})
	for _, line := range got {
		if len(strings.TrimSuffix(line, " \\")) > st.Width {
			t.Errorf("line over width: %q", line)
		}
	}
	if !reflect.DeepEqual(BlockEntries(got), []string{"aaaa", "bbbb", "cccc", "dddd"}) {
		t.Errorf("render did not round-trip: %q -> %q", got, BlockEntries(got))
	}
	if len(got) < 2 {
		t.Fatalf("nothing wrapped, so there is no continuation to check: %q", got)
	}
	assertContinued(t, got)
}

// The head line always takes the first entry, however long it is: wrapping
// before any entry would emit an assignment whose first line is bare.
func TestRenderNeverEmitsAnEmptyHead(t *testing.T) {
	got := Render("ALPHA", Style{Op: ":=", Indent: "  ", Width: 10}, []string{"a-very-long-entry", "b"})
	if strings.TrimSpace(strings.TrimSuffix(got[0], "\\")) == "ALPHA :=" {
		t.Fatalf("head line carries no entry: %q", got[0])
	}
}

// The zero-churn property, which is the whole reason Append exists beside
// Render: a merge that only gained entries must not rewrite the lines already
// in the file.
func TestAppendLeavesTheExistingLinesByteForByte(t *testing.T) {
	block := []string{"ALPHA := aaaa \\", "         bbbb"}
	got := Append(block, []string{"cccc"}, Style{Op: ":=", Indent: "         ", Width: 100})
	if got[0] != block[0] {
		t.Errorf("first line rewritten: %q -> %q", block[0], got[0])
	}
	// Only the line that used to end the assignment changes, and only by
	// gaining the continuation it now needs.
	if got[1] != block[1]+" \\" {
		t.Errorf("last line = %q, want %q", got[1], block[1]+" \\")
	}
	if !reflect.DeepEqual(BlockEntries(got), []string{"aaaa", "bbbb", "cccc"}) {
		t.Errorf("entries = %q", BlockEntries(got))
	}
}

func TestAppendWithNothingToAddChangesNothing(t *testing.T) {
	block := []string{"ALPHA := a \\", "   b"}
	got := Append(block, nil, Style{Op: ":=", Indent: "   ", Width: 100})
	if !reflect.DeepEqual(got, block) {
		t.Errorf("got %q, want %q", got, block)
	}
}

func TestAppendWrapsTheAdditions(t *testing.T) {
	got := Append([]string{"ALPHA := a"}, []string{"bbbb", "cccc", "dddd"},
		Style{Op: ":=", Indent: "  ", Width: 12})
	for _, line := range got {
		if len(strings.TrimSuffix(line, " \\")) > 12 {
			t.Errorf("line over width: %q", line)
		}
	}
	if !reflect.DeepEqual(BlockEntries(got), []string{"a", "bbbb", "cccc", "dddd"}) {
		t.Errorf("entries = %q", BlockEntries(got))
	}
	if len(got) < 2 {
		t.Fatalf("nothing wrapped, so there is no continuation to check: %q", got)
	}
	assertContinued(t, got)
}

func TestSubstitutePutsEachBlockBackAtItsSlot(t *testing.T) {
	body := []string{"# prose", Slot("ALPHA"), "mid", Slot("BETA")}
	got, err := Substitute(body, vars, map[string][]string{
		"ALPHA": {"ALPHA := a \\", "  b"},
		"BETA":  {"BETA := c"},
	})
	if err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	want := []string{"# prose", "ALPHA := a \\", "  b", "mid", "BETA := c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A sentinel the body merge dropped means the pairing is gone, and quietly
// carrying on would lose a whole list out of the gate.
func TestSubstituteRefusesAMissingSlot(t *testing.T) {
	_, err := Substitute([]string{Slot("ALPHA")}, vars, map[string][]string{
		"ALPHA": {"ALPHA := a"},
		"BETA":  {"BETA := b"},
	})
	if err == nil || !strings.Contains(err.Error(), "the BETA placeholder did not survive") {
		t.Fatalf("err = %v, want the BETA placeholder", err)
	}
}

// Lift and BlockEntries are the two halves of the driver's round-trip check, so
// they have to agree on what an entry is over the shapes the real file uses.
func TestLiftAndBlockEntriesAgree(t *testing.T) {
	for _, block := range [][]string{
		{"ALPHA := a b c"},
		{"ALPHA := a \\", "   b \\", "   c"},
		{"ALPHA :=   a\tb  \\", "\t c"},
		{"ALPHA :="},
	} {
		lines := append(append([]string{}, block...), "BETA := z")
		doc, err := Lift(lines, vars)
		if err != nil {
			t.Fatalf("Lift(%q): %v", block, err)
		}
		got, want := BlockEntries(block), doc.Blocks["ALPHA"].Entries
		if len(got) != len(want) || (len(got) > 0 && !reflect.DeepEqual(got, want)) {
			t.Errorf("%q: BlockEntries = %q, Lift = %q", block, got, want)
		}
	}
}

// Continued is the half of the round-trip check that entry membership cannot
// supply, so it is asserted against a block that round-trips perfectly and is
// still broken.
func TestContinuedRejectsABlockThatEndsEarly(t *testing.T) {
	broken := []string{"ALPHA := a", "   b"}
	if got, want := BlockEntries(broken), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %q, want %q — the premise is that membership is intact", got, want)
	}
	if err := Continued(broken); err == nil {
		t.Error("Continued accepted a block whose first line ends the assignment")
	}
}

func TestContinuedRejectsATrailingContinuation(t *testing.T) {
	if err := Continued([]string{"ALPHA := a \\"}); err == nil {
		t.Error("Continued accepted a block that continues into nothing")
	}
}

func TestContinuedAcceptsWhatRenderAndAppendProduce(t *testing.T) {
	st := Style{Op: ":=", Indent: "  ", Width: 20}
	for _, lines := range [][]string{
		Render("ALPHA", st, []string{"aaaa", "bbbb", "cccc", "dddd"}),
		Render("ALPHA", st, []string{"a"}),
		Append([]string{"ALPHA := a"}, []string{"bbbb", "cccc"}, st),
		Append([]string{"ALPHA := a \\", "  b"}, nil, st),
	} {
		if err := Continued(lines); err != nil {
			t.Errorf("Continued(%q) = %v", lines, err)
		}
	}
}
