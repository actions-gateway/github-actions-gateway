package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	queueHead = "## Queue\n\n| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n"
	deferHead = "\n## Deferred\n\n| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n"
	labelsDec = "**Labels:** `ci` `debt`\n\n"
)

// lint writes src to a temp file and returns the findings, one message per
// entry. A temp dir outside any repository keeps the git-baseline rules
// no-ops, so a case asserts only the content rule it names.
func lint(t *testing.T, src string) []string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "STATUS.md")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	cfg := config{file: file, maxChars: 250, linkChars: 200}
	n, err := run(cfg, &out, &errOut)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if errOut.Len() == 0 {
		if n != 0 {
			t.Fatalf("%d findings but nothing printed", n)
		}
		return nil
	}
	var msgs []string
	for _, line := range strings.Split(strings.TrimRight(errOut.String(), "\n"), "\n") {
		if i := strings.Index(line, "STATUS.md:"); i >= 0 {
			line = line[i+len("STATUS.md:"):]
			// Drop the line number prefix so cases assert the message.
			if j := strings.Index(line, ": "); j >= 0 {
				line = line[j+2:]
			}
		}
		msgs = append(msgs, line)
	}
	if len(msgs) != n {
		t.Fatalf("printed %d lines for %d findings", len(msgs), n)
	}
	return msgs
}

func repeat(s string, n int) string { return strings.Repeat(s, n) }

func TestQueueRules(t *testing.T) {
	tests := []struct {
		name string
		row  string
		want string // substring of the single expected finding; "" means clean
	}{{
		// The Q613 defect. Splitting on a literal `|` put everything after the
		// escape in a later field, so the cap measured the stub before it.
		name: "escaped pipe does not hide the rest of the Notes cell",
		row:  qrow("Q1", "[Item](plan/x.md)", "🔲", repeat("a", 40)+` \| `+repeat("b", 215)),
		want: "Q1 Notes is 259 chars (max 250)",
	}, {
		name: "control: the same length without an escape",
		row:  qrow("Q1", "[Item](plan/x.md)", "🔲", repeat("a", 259)),
		want: "Q1 Notes is 259 chars (max 250)",
	}, {
		// The other direction: the escape sits before St, which a positional
		// split then read from the Labels cell.
		name: "escaped pipe in Item leaves St readable",
		row:  qrow("Q1", `Item with a \| pipe`, "🔲", "short"),
		want: "",
	}, {
		name: "control: the same Item without an escape",
		row:  qrow("Q1", "Item with a pipe", "🔲", "short"),
		want: "",
	}, {
		// The second Q613 defect: awk's length() was bytes under BWK awk and
		// mawk, runes under gawk. 250 em dashes are 750 bytes.
		name: "cap counts runes, not bytes",
		row:  qrow("Q1", "[Item](plan/x.md)", "🔲", repeat("—", 250)),
		want: "",
	}, {
		name: "cap still fires one rune over",
		row:  qrow("Q1", "[Item](plan/x.md)", "🔲", repeat("—", 251)),
		want: "Q1 Notes is 251 chars (max 250)",
	}, {
		name: "an escape costs the two characters it is written as",
		row:  qrow("Q1", "[Item](plan/x.md)", "🔲", repeat("a", 249)+`\|`),
		want: "Q1 Notes is 251 chars (max 250)",
	}, {
		name: "over the link threshold with no document link",
		row:  qrow("Q1", "Item", "🔲", repeat("a", 201)),
		want: "links no document",
	}, {
		name: "old-format state marker",
		row:  qrow("Q1", "Item", "▶", "short"),
		want: "Q1 St is ▶",
	}, {
		name: "unknown state",
		row:  qrow("Q1", "Item", "?", "short"),
		want: "Q1 St must be 🔲 or 🚫; got: ?",
	}, {
		name: "Blocked by without the blocked state",
		row:  qrow("Q1", "Item", "🔲", "Blocked by [Q1](#Q1). Waits."),
		want: "Q1 Notes say Blocked by but St is not 🚫",
	}, {
		name: "undeclared label",
		row:  qrow("Q1", "Item", "🔲", "short", "`nope`"),
		want: "Q1 uses undeclared label `nope`",
	}, {
		name: "dangling sibling reference",
		row:  qrow("Q1", "Item", "🔲", "see [Q9](#Q9)"),
		want: "Q1 links (#Q9) but no row Q9 exists",
	}, {
		name: "visible ID with no anchor",
		row:  `| Q1 | Item | ` + "`ci`" + ` | 🔲 | S | short |`,
		want: `Q1 has no <a id="Q1"></a> anchor`,
	}, {
		name: "anchor that disagrees with the visible ID",
		row:  `| <a id="Q2"></a>Q1 | Item | ` + "`ci`" + ` | 🔲 | S | short |`,
		want: `anchor id="Q2" does not match visible ID Q1`,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lint(t, labelsDec+queueHead+tc.row+"\n"+deferHead)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want clean, got %q", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 finding, got %d: %q", len(got), got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("finding = %q, want it to contain %q", got[0], tc.want)
			}
		})
	}
}

func TestDeferredRules(t *testing.T) {
	tests := []struct {
		name string
		row  string
		want string
	}{{
		name: "trigger with no source tag",
		row:  drow("Q1", "Item", "when someone asks"),
		want: "Deferred trigger must open with",
	}, {
		name: "tagged trigger",
		row:  drow("Q1", "Item", "**Event:** upstream ships the fix."),
		want: "",
	}, {
		name: "trigger over the hard cap",
		row:  drow("Q1", "[Item](plan/x.md)", "**Demand:** "+repeat("a", 250)),
		want: "Q1 trigger cell is 262 chars (max 250)",
	}, {
		name: "an escaped pipe does not truncate the trigger either",
		row:  drow("Q1", "[Item](plan/x.md)", "**Demand:** "+repeat("a", 40)+` \| `+repeat("b", 210)),
		want: "Q1 trigger cell is 266 chars (max 250)",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lint(t, labelsDec+queueHead+deferHead+tc.row+"\n")
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want clean, got %q", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 finding, got %d: %q", len(got), got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("finding = %q, want it to contain %q", got[0], tc.want)
			}
		})
	}
}

func TestFileLevelRules(t *testing.T) {
	row := qrow("Q1", "Item", "🔲", "short")
	tests := []struct {
		name string
		src  string
		want string
	}{{
		name: "Next ID counter",
		src:  "**Next ID:** Q9\n\n" + labelsDec + queueHead + row + "\n" + deferHead,
		want: "drop the Next ID counter",
	}, {
		name: "Last touched line",
		src:  labelsDec + queueHead + row + "\n" + deferHead + "\nLast touched: 2026-08-03\n",
		want: "drop the Last touched line",
	}, {
		// Prose *about* the old format is not the old format. The awk matched
		// the line wherever it appeared, including inside a fenced block.
		name: "a counter inside a code block is prose",
		src:  labelsDec + "```\n**Next ID:** Q9\n```\n\n" + queueHead + row + "\n" + deferHead,
		want: "",
	}, {
		name: "no Queue section",
		src:  labelsDec + "## Deferred\n",
		want: "no ## Queue section found",
	}, {
		name: "labels with no declaring line",
		src:  queueHead + row + "\n" + deferHead,
		want: "no **Labels:** line declares the vocabulary",
	}, {
		// A -gate label's gloss carries backticked link text naming a release,
		// which must not enter the vocabulary.
		name: "gloss link text is not a label",
		src: "**Labels:** `ci` `2.0-gate` (blocks the [`v2.0.0`](plan/v2.md) tag)\n\n" +
			queueHead + qrow("Q1", "Item", "🔲", "short", "`v2.0.0`") + "\n" + deferHead,
		want: "uses undeclared label `v2.0.0`",
	}, {
		name: "duplicate ID across the two tables",
		src: labelsDec + queueHead + row + "\n" + deferHead +
			drow("Q1", "Item", "**Event:** it recurs.") + "\n",
		want: "duplicate ID Q1 (in Queue and Deferred)",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lint(t, tc.src)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want clean, got %q", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 finding, got %d: %q", len(got), got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("finding = %q, want it to contain %q", got[0], tc.want)
			}
		})
	}
}

// Rule 13. A row wider than its header is an unescaped pipe, and GFM drops the
// overflow from the rendered table, so the finding must name the width rather
// than leave the shift to be noticed by whichever rule happens to read a moved
// cell.
func TestUnescapedPipeFailsLoudly(t *testing.T) {
	row := `| <a id="Q1"></a>Q1 | Item | pipe | ` + "`ci`" + ` | 🔲 | S | short |`
	got := lint(t, labelsDec+queueHead+row+"\n"+deferHead)
	if len(got) == 0 {
		t.Fatal("an unescaped pipe passed silently")
	}
	if !strings.Contains(got[0], "Q1 has 7 cells but the header declares 6") {
		t.Errorf("finding = %q, want the row width to be rejected", got[0])
	}
}

// The control differs from the case above only by the escape: `\|` is a literal
// pipe to GFM even inside a code span, so the row stays six cells wide. Without
// it the rule would pass by rejecting every row that mentions a pipe at all.
func TestEscapedPipeIsOneCell(t *testing.T) {
	row := qrow("Q1", "Item", "🔲", "scans for `\\|`-prefixed lines")
	if got := lint(t, labelsDec+queueHead+row+"\n"+deferHead); len(got) != 0 {
		t.Errorf("escaped pipe rejected: %q", got)
	}
}

// A Deferred row is five cells wide, so the rule reads each table's own header
// rather than one hard-coded width.
func TestUnescapedPipeInDeferredRow(t *testing.T) {
	row := `| <a id="Q1"></a>Q1 | Item | pipe | ` + "`ci`" + ` | S | **Demand:** soon |`
	got := lint(t, labelsDec+queueHead+deferHead+row+"\n")
	if len(got) == 0 || !strings.Contains(got[0], "Q1 has 6 cells but the header declares 5") {
		t.Errorf("findings = %q, want the Deferred row width to be rejected", got)
	}
}

func qrow(id, item, st, notes string, labels ...string) string {
	label := "`ci`"
	if len(labels) > 0 {
		label = labels[0]
	}
	return "| <a id=\"" + id + "\"></a>" + id + " | " + item + " | " + label +
		" | " + st + " | S | " + notes + " |"
}

func drow(id, item, trigger string) string {
	return "| <a id=\"" + id + "\"></a>" + id + " | " + item + " | `ci` | S | " + trigger + " |"
}
