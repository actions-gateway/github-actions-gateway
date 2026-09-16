package mdroadmap

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestMarkerKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"one binding", "- **Thing** <!-- q:Q10 --> text", "Q10"},
		{"a comma list", "- **Thing** <!-- q:Q10,Q11 --> text", "Q10,Q11"},
		{"spaces are stripped", "- **Thing** <!--  q: Q10 , Q11  --> text", "Q10,Q11"},
		{"tabs are stripped", "- **Thing** <!--\tq:Q10,\tQ11 --> text", "Q10,Q11"},
		{"two annotations both count", "- a <!-- q:Q10 --> b <!-- q:Q11 -->", "Q10,Q11"},
		{"not a bullet", "  <!-- q:Q10 -->", ""},
		{"a heading is not a bullet", "## Exploring <!-- q:Q10 -->", ""},
		{"no annotation", "- **Thing** with no binding", ""},
		{"an ID the backlog would not recognize", "- a <!-- q:notanid -->", ""},
		{"one bad ID unkeys the whole bullet", "- a <!-- q:Q10,notanid -->", ""},
		{"an empty payload keys nothing", "- a <!-- q: -->", ""},
		{"a dash in the payload is not an annotation", "- a <!-- q:Q10-Q11 -->", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MarkerKey(tc.line); got != tc.want {
				t.Errorf("MarkerKey(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// An encoded record holds several source lines, so an annotation the encode
// folded a line break into is not an annotation: keying a bullet by a binding
// that spans two source lines would key it by text nothing on the page reads
// that way.
func TestMarkerKeyStopsAtTheRecordSeparator(t *testing.T) {
	rec := "- **Thing** <!-- q:Q10" + Sep + "Q99 --> text"
	if got := MarkerKey(rec); got != "" {
		t.Errorf("MarkerKey across a separator = %q, want the empty key", got)
	}
}

// A record spanning several lines keys off whatever annotations it really
// carries, wherever in the bullet they sit — a wrapped title can push the
// binding onto the second line.
func TestMarkerKeyReadsAContinuationLine(t *testing.T) {
	rec := "- **A title long enough to wrap**" + Sep + "  <!-- q:Q10 --> the rest"
	if got := MarkerKey(rec); got != "Q10" {
		t.Errorf("MarkerKey = %q, want %q", got, "Q10")
	}
}

// The whole-page uniqueness check runs over decoded lines, including bullets the
// driver left in prose because nothing could key them. Two of those naming one
// row are still two bullets for one row.
func TestDupeKeyDoesNotRequireAWellFormedID(t *testing.T) {
	if got := DupeKey("- a <!-- q:notanid -->"); got != "notanid" {
		t.Errorf("DupeKey = %q, want %q", got, "notanid")
	}
	if got := MarkerKey("- a <!-- q:notanid -->"); got != "" {
		t.Errorf("MarkerKey = %q, want the empty key", got)
	}
}

func page(lines ...string) []string { return lines }

func TestSplitCarvesListsFromProse(t *testing.T) {
	doc, err := Split(page(
		"# Roadmap",
		"",
		"- **A** <!-- q:Q10 --> text",
		"  second line",
		"",
		"- **B** <!-- q:Q11 --> text",
		"",
		"## Next",
		"",
		"- **C** <!-- q:Q12 --> text",
		"",
	))
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(doc.Lists) != 2 {
		t.Fatalf("got %d lists, want 2", len(doc.Lists))
	}
	if want := []string{"# Roadmap", ""}; !reflect.DeepEqual(doc.Lists[0].Pre, want) {
		t.Errorf("list 1 prose = %q, want %q", doc.Lists[0].Pre, want)
	}
	wantRecs := []string{
		"- **A** <!-- q:Q10 --> text" + Sep + "  second line",
		"- **B** <!-- q:Q11 --> text",
	}
	if !reflect.DeepEqual(doc.Lists[0].Records, wantRecs) {
		t.Errorf("list 1 records = %q, want %q", doc.Lists[0].Records, wantRecs)
	}
	if want := []int{1, 1}; !reflect.DeepEqual(doc.Lists[0].Blanks, want) {
		t.Errorf("list 1 blanks = %v, want %v", doc.Lists[0].Blanks, want)
	}
	if doc.Lists[0].Tail != 1 {
		t.Errorf("list 1 tail = %d, want 1", doc.Lists[0].Tail)
	}
	if want := []string{"## Next", ""}; !reflect.DeepEqual(doc.Lists[1].Pre, want) {
		t.Errorf("list 2 prose = %q, want %q", doc.Lists[1].Pre, want)
	}
	if len(doc.Post) != 0 {
		t.Errorf("post = %q, want nothing", doc.Post)
	}
}

// A blank line is undecided when it is read: absorbed into the record when a
// continuation follows, counted as a separator when a bullet does.
func TestSplitAbsorbsABlankBeforeAContinuation(t *testing.T) {
	doc, err := Split(page(
		"intro",
		"- **A** <!-- q:Q10 -->",
		"",
		"  a second paragraph of the same bullet",
		"",
		"- **B** <!-- q:Q11 -->",
		"",
	))
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	want := "- **A** <!-- q:Q10 -->" + Sep + "" + Sep + "  a second paragraph of the same bullet"
	if doc.Lists[0].Records[0] != want {
		t.Errorf("record = %q, want %q", doc.Lists[0].Records[0], want)
	}
	if want := []int{1, 1}; !reflect.DeepEqual(doc.Lists[0].Blanks, want) {
		t.Errorf("blanks = %v, want %v", doc.Lists[0].Blanks, want)
	}
}

func TestSplitTreatsAnUnkeyableRunAsProse(t *testing.T) {
	lines := page(
		"intro",
		"- an ordinary bullet with no binding",
		"- **A** <!-- q:Q10 -->",
		"",
		"## Next",
		"",
		"- **B** <!-- q:Q11 -->",
		"",
	)
	doc, err := Split(lines)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(doc.Lists) != 1 {
		t.Fatalf("got %d lists, want 1", len(doc.Lists))
	}
	if got := doc.Lists[0].Records[0]; !strings.Contains(got, "q:Q11") {
		t.Errorf("the surviving list is %q, want the annotated one", got)
	}
	if !reflect.DeepEqual(doc.Lists[0].Pre, lines[:6]) {
		t.Errorf("prose = %q, want the unkeyable run folded into it", doc.Lists[0].Pre)
	}
}

// A separator that is not an empty line could not be rebuilt from a count, so it
// disqualifies the run the same way an unannotated bullet does.
func TestSplitRefusesAWhitespaceOnlySeparator(t *testing.T) {
	_, err := Split(page(
		"intro",
		"- **A** <!-- q:Q10 -->",
		"   ",
		"- **B** <!-- q:Q11 -->",
		"",
	))
	if !errors.Is(err, ErrNoLists) {
		t.Fatalf("err = %v, want ErrNoLists", err)
	}
}

func TestSplitRefusesASourceLineHoldingTheSeparator(t *testing.T) {
	_, err := Split(page(
		"intro",
		"- **A** <!-- q:Q10 --> text with"+Sep+"one already",
		"",
	))
	if err == nil || !strings.Contains(err.Error(), "already contains the record separator (line 2)") {
		t.Fatalf("err = %v, want the separator refusal naming line 2", err)
	}
}

func TestSplitRefusesAPageWithNoAnnotatedList(t *testing.T) {
	if _, err := Split(page("# Roadmap", "", "prose only")); !errors.Is(err, ErrNoLists) {
		t.Fatalf("err = %v, want ErrNoLists", err)
	}
}

func list(tail int, recs []string, blanks []int) *List {
	return &List{Records: recs, Blanks: blanks, Tail: tail}
}

func TestDecodeRebuildsSpacing(t *testing.T) {
	a, b := "- a <!-- q:Q10 -->", "- b <!-- q:Q11 -->"
	side := list(1, []string{a, b}, []int{1, 1})
	got, err := Decode(side, side, side, []string{a, b})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []string{a, "", b, ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDecodeSplitsAnEncodedRecordBackIntoLines(t *testing.T) {
	rec := "- a <!-- q:Q10 -->" + Sep + "  second"
	side := list(0, []string{rec}, []int{0})
	got, err := Decode(side, side, side, []string{rec})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := []string{"- a <!-- q:Q10 -->", "  second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The mechanism the tight-list case turns on: the separator a surviving last
// record carries described the bullet that used to follow it, so it takes the
// list's trailing count instead. Without the override, deleting a tight list's
// final bullet welds its predecessor to the next heading.
// Two survivors, not one: with a single record the last index is also the first,
// so a test built on one cannot tell the override from any other rule that
// happens to fire there. Confirmed by moving the override to k == 0, which this
// catches and the one-record version did not.
func TestDecodeGivesTheLastRecordTheListTail(t *testing.T) {
	a, b, c := "- a <!-- q:Q10 -->", "- b <!-- q:Q11 -->", "- c <!-- q:Q12 -->"
	// A tight list: no blank between bullets, one blank before the next heading.
	base := list(1, []string{a, b, c}, []int{0, 0, 1})
	ours := list(1, []string{a, b, c}, []int{0, 0, 1})
	theirs := list(1, []string{a, b}, []int{0, 1})
	got, err := Decode(base, ours, theirs, []string{a, b})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := []string{a, b, ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q — the new last bullet kept its own 0 and welded itself to the next heading", got, want)
	}
}

func TestDecodeTakesTheSideThatRespaced(t *testing.T) {
	a := "- a <!-- q:Q10 -->"
	base := list(1, []string{a}, []int{1})
	ours := list(1, []string{a}, []int{1})
	theirs := list(2, []string{a}, []int{2})
	got, err := Decode(base, ours, theirs, []string{a})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := []string{a, "", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDecodeRefusesWhenBothSidesRespacedDifferently(t *testing.T) {
	a, b := "- a <!-- q:Q10 -->", "- b <!-- q:Q11 -->"
	base := list(1, []string{a, b}, []int{1, 1})
	ours := list(1, []string{a, b}, []int{0, 1})
	theirs := list(1, []string{a, b}, []int{2, 1})
	_, err := Decode(base, ours, theirs, []string{a, b})
	if err == nil || !strings.Contains(err.Error(), "respaced differently on both sides") {
		t.Fatalf("err = %v, want the respacing refusal", err)
	}
}

func TestDecodeRefusesWhenBothSidesChangedTheTrailingBlanks(t *testing.T) {
	a := "- a <!-- q:Q10 -->"
	base := list(1, []string{a}, []int{1})
	ours := list(0, []string{a}, []int{0})
	theirs := list(2, []string{a}, []int{2})
	_, err := Decode(base, ours, theirs, []string{a})
	if err == nil || !strings.Contains(err.Error(), "blank lines after the list were changed on both sides") {
		t.Fatalf("err = %v, want the trailing-blank refusal", err)
	}
}

// A record only one side holds takes that side's spacing, which is what makes an
// added bullet land with the spacing the side that added it chose.
func TestDecodeTakesAnAddedRecordsOwnSpacing(t *testing.T) {
	a, c := "- a <!-- q:Q10 -->", "- c <!-- q:Q12 -->"
	base := list(1, []string{a}, []int{1})
	ours := list(1, []string{a, c}, []int{2, 1})
	theirs := list(1, []string{a}, []int{1})
	got, err := Decode(base, ours, theirs, []string{a, c})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := []string{a, "", "", c, ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
