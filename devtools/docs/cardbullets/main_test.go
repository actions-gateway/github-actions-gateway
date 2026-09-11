package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// threeAcross is the shape docs/index.md uses: a `.gag-pillars` grid with no
// width modifier, so its bullets get the 50-character column. Every fixture
// below is a mutation of this, so a case that stops discriminating shows up as
// two fixtures that are equal.
const threeAcross = `# Page

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   __A card__

    ---

    Its lead-in:

    - Short enough
    - [A link whose text fits](somewhere.md)

</div>
</div>
`

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "page.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// check runs the gate over one source and reports its findings, bullets
// measured, and output.
func check(t *testing.T, content string) (findings, checked int, out string) {
	t.Helper()
	var buf bytes.Buffer
	findings, checked, err := run([]string{write(t, content)}, &buf, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return findings, checked, buf.String()
}

func TestCompliantCardPasses(t *testing.T) {
	findings, checked, out := check(t, threeAcross)
	if findings != 0 {
		t.Errorf("want 0 findings, got %d\n%s", findings, out)
	}
	if checked != 2 {
		t.Errorf("want 2 bullets measured, got %d", checked)
	}
}

// The card's own item is not a card bullet — it is the card. Measuring it
// would fail every card whose title plus body runs past the column, which is
// every card.
func TestCardItemIsNotMeasured(t *testing.T) {
	_, checked, _ := check(t, threeAcross)
	if checked != 2 {
		t.Errorf("the card item was measured too: want 2, got %d", checked)
	}
}

func TestOverBudgetBulletFails(t *testing.T) {
	src := strings.Replace(threeAcross, "    - Short enough",
		"    - This bullet is written well past the fifty character column budget", 1)
	findings, _, out := check(t, src)
	if findings != 1 {
		t.Fatalf("want 1 finding, got %d\n%s", findings, out)
	}
	if !strings.Contains(out, "against a 50-character column") {
		t.Errorf("finding does not name the three-across budget:\n%s", out)
	}
}

// The two-across grid is a wider column, so a bullet that fails at 50 passes
// at 77. Without the class check the gate would fail every `.gag-cols-2` page.
func TestTwoAcrossGetsTheWiderBudget(t *testing.T) {
	long := "    - This bullet is written well past the fifty character column budget"
	narrow := strings.Replace(threeAcross, "    - Short enough", long, 1)
	if findings, _, _ := check(t, narrow); findings != 1 {
		t.Fatalf("fixture does not fail at the narrow budget: %d findings", findings)
	}
	wide := strings.Replace(narrow, `class="gag-pillars"`, `class="gag-pillars gag-cols-2"`, 1)
	if findings, _, out := check(t, wide); findings != 0 {
		t.Errorf("want 0 findings in a two-across grid, got %d\n%s", findings, out)
	}
}

// A modifier class is not the column class. `gag-pillars--problem` ships on
// docs/why-gag.md beside the real one, and a substring match would read it as
// a second grid.
func TestModifierClassIsNotTheGridClass(t *testing.T) {
	src := strings.Replace(threeAcross, `class="gag-pillars"`, `class="gag-pillars--problem"`, 1)
	_, checked, _ := check(t, src)
	if checked != 0 {
		t.Errorf("a modifier-only div was read as a card grid: %d bullets measured", checked)
	}
}

// A bullet outside any card grid is ordinary prose with no column to fit, so
// it carries no budget however long it runs.
func TestBulletOutsideAGridIsNotMeasured(t *testing.T) {
	src := threeAcross + "\n- A plain list bullet that runs well past fifty characters of prose\n" +
		"    - and a nested one that runs well past fifty characters too\n"
	findings, checked, out := check(t, src)
	if findings != 0 {
		t.Errorf("want 0 findings outside the grid, got %d\n%s", findings, out)
	}
	if checked != 2 {
		t.Errorf("want 2 bullets measured, got %d", checked)
	}
}

// The budget is spent on what a reader sees. A destination long enough to blow
// the column on its own must not count, or every linked bullet fails.
func TestLinkDestinationIsNotBilled(t *testing.T) {
	src := strings.Replace(threeAcross, "(somewhere.md)",
		"(operations/security-operations.md#sharing-an-egress-proxy-across-namespaces)", 1)
	findings, _, out := check(t, src)
	if findings != 0 {
		t.Errorf("the link destination was billed to the bullet: %d findings\n%s", findings, out)
	}
}

// Likewise the backticks of a code span: the reader sees the contents.
func TestCodeSpanBacktricksAreNotBilled(t *testing.T) {
	// 50 rendered characters exactly, of which 4 would be backticks.
	src := strings.Replace(threeAcross, "    - Short enough",
		"    - `kubectl apply -k` then `kubectl rollout status` ok", 1)
	if findings, _, out := check(t, src); findings != 0 {
		t.Errorf("backticks were billed: %d findings\n%s", findings, out)
	}
}

// A page with no card at all cannot be reported green: main exits 2 on it, and
// that decision rests on checked being 0 here.
func TestPageWithNoCardMeasuresNothing(t *testing.T) {
	_, checked, _ := check(t, "# Page\n\n- An ordinary bullet.\n")
	if checked != 0 {
		t.Errorf("want 0 bullets measured, got %d", checked)
	}
}

// The grid closes at its own `</div>`, so a bullet after the card is outside
// the span. Counting the first `</div>` instead of matching the nesting would
// end the span one line early and drop the last card's bullets.
func TestGridSpanEndsAtItsMatchingClose(t *testing.T) {
	src := threeAcross + "\n<div class=\"other\" markdown>\n\n- Outside, and written well past fifty characters of prose\n\n</div>\n"
	findings, checked, out := check(t, src)
	if findings != 0 {
		t.Errorf("a bullet after the grid was measured: %d findings\n%s", findings, out)
	}
	if checked != 2 {
		t.Errorf("want 2 bullets measured, got %d", checked)
	}
}
