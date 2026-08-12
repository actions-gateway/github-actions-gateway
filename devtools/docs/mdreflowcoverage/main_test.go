package main

import "testing"

func TestEndsSentence(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"plain period", "One sentence.", true},
		{"question", "Is it?", true},
		{"colon lead-in", "Run this:", true},
		{"closing quote after period", `He said "no."`, true},
		{"paren after period", "(That is the reason.)", true},
		{"code span after period", "See `make check`.", true},
		{"period inside trailing code span", "See `make check.`", true},
		{"mid-sentence break", "One sentence that runs on", false},
		{"trailing whitespace tolerated", "Done.   ", true},
		{"comma", "First clause,", false},
		{"empty", "", false},
		{"only closers", `")]`, false},
	}
	for _, c := range cases {
		if got := endsSentence(c.line); got != c.want {
			t.Errorf("%s: endsSentence(%q) = %v, want %v", c.name, c.line, got, c.want)
		}
	}
}

// The denominator is the claim worth pinning: a line classifier over-counts by
// reading headings, tables, fences and list markers as prose. Each fixture
// states the interior-break count a parser must reach.
func TestMeasureDenominator(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		wantBreaks int
		wantRes    int
	}{
		{
			name:       "one-line paragraph has no interior break",
			src:        "Just the one line.\n",
			wantBreaks: 0,
		},
		{
			name:       "sentence-per-line paragraph is all boundary",
			src:        "First sentence.\nSecond sentence.\nThird.\n",
			wantBreaks: 2,
			wantRes:    0,
		},
		{
			name:       "hand-wrapped paragraph is all residue",
			src:        "A sentence that has been hand wrapped across\nthree source lines for no good\nreason at all.\n",
			wantBreaks: 2,
			wantRes:    2,
		},
		{
			name:       "headings and blank lines are not paragraphs",
			src:        "# Title\n\n## Section\n\nOne line.\n",
			wantBreaks: 0,
		},
		{
			name:       "fenced code is not prose",
			src:        "```bash\nmake check\nmake test\nmake lint\n```\n",
			wantBreaks: 0,
		},
		{
			name:       "table rows are not paragraphs",
			src:        "| A | B |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |\n",
			wantBreaks: 0,
		},
		{
			name:       "list items count their own interior breaks",
			src:        "- First bullet.\n  Second sentence.\n- Another bullet.\n",
			wantBreaks: 1,
			wantRes:    0,
		},
		{
			name:       "two paragraphs do not break across the blank line",
			src:        "One.\nTwo.\n\nThree.\nFour.\n",
			wantBreaks: 2,
			wantRes:    0,
		},
	}
	for _, c := range cases {
		breaks, residue := measure("fixture.md", []byte(c.src))
		if breaks != c.wantBreaks {
			t.Errorf("%s: breaks = %d, want %d", c.name, breaks, c.wantBreaks)
		}
		if len(residue) != c.wantRes {
			t.Errorf("%s: residue = %d, want %d", c.name, len(residue), c.wantRes)
		}
	}
}

// Reading the walker predicts which line it blames; running it measures that.
// A residue report naming the wrong line sends an author to the wrong place.
func TestMeasureReportsTheOffendingLine(t *testing.T) {
	src := "# Heading\n\nFine sentence.\nA line that runs\nover a break.\n"
	breaks, residue := measure("fixture.md", []byte(src))
	if breaks != 2 {
		t.Fatalf("breaks = %d, want 2", breaks)
	}
	if len(residue) != 1 {
		t.Fatalf("residue = %d, want 1", len(residue))
	}
	if residue[0].Line != 4 {
		t.Errorf("residue line = %d, want 4", residue[0].Line)
	}
	if residue[0].Text != "A line that runs" {
		t.Errorf("residue text = %q, want %q", residue[0].Text, "A line that runs")
	}
}
