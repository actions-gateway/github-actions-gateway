// Package mklists lifts a Makefile's whitespace-separated list assignments out
// of the file and renders them back, so a merge driver can merge the entries as
// a set while the rest of the Makefile merges as ordinary text.
//
// It is the file-specific half of the gate-lists driver; the set merge itself
// is devtools/git/keyedrecords, reached with an identity key, because an entry
// here is a bare word and the word is its own key.
//
// Confining the rewrite to a sentinel line is the safety argument: a conflict
// anywhere else in the Makefile never reaches the list logic at all.
package mklists

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// assign matches the head of a list assignment, and strips it when the
	// entries on that line are wanted.
	assign = regexp.MustCompile(`^[A-Z_]+[ \t]*[:+?]?=`)
	// name reduces an assignment line to the variable it assigns.
	name = regexp.MustCompile(`[ \t]*[:+?]?=.*$`)
	// cont matches the backslash that carries an assignment onto the next line.
	cont = regexp.MustCompile(`\\[ \t]*$`)
	// op reads the assignment operator, which a render reuses.
	op   = regexp.MustCompile(`[:+?]?=`)
	lead = regexp.MustCompile(`^[ \t]+`)
)

// Block is one managed variable's assignment as one side wrote it.
type Block struct {
	Name    string
	Lines   []string // verbatim, continuations included
	Entries []string
}

// Doc is one side of the merge: the Makefile with each managed assignment
// replaced by its sentinel, plus the assignments themselves.
type Doc struct {
	Body   []string
	Blocks map[string]*Block
}

// Slot is the one-line stand-in a lifted assignment leaves behind. It is a
// Makefile comment because the body is merged as ordinary text and, on a
// refusal, can be read by make with the sentinel still in it.
func Slot(varName string) string { return "#__GATE_LIST_SLOT_" + varName + "__" }

// Lift splits lines into a body and one Block per managed variable.
//
// A variable missing from a side, or assigned twice on one, breaks the pairing
// the merge depends on, so both are errors rather than something to work
// around: the caller turns them into ordinary conflict markers.
func Lift(lines []string, vars []string) (*Doc, error) {
	managed := make(map[string]bool, len(vars))
	for _, v := range vars {
		managed[v] = true
	}

	doc := &Doc{Blocks: make(map[string]*Block, len(vars))}
	var cur *Block
	for _, line := range lines {
		if cur != nil {
			cur.Lines = append(cur.Lines, line)
			cur.Entries = append(cur.Entries, fields(line)...)
			if !cont.MatchString(line) {
				cur = nil
			}
			continue
		}
		v := name.ReplaceAllString(line, "")
		if !managed[v] || !assign.MatchString(line) {
			doc.Body = append(doc.Body, line)
			continue
		}
		if _, dup := doc.Blocks[v]; dup {
			return nil, fmt.Errorf("%s is assigned more than once", v)
		}
		cur = &Block{Name: v, Lines: []string{line}, Entries: fields(assign.ReplaceAllString(line, ""))}
		doc.Blocks[v] = cur
		doc.Body = append(doc.Body, Slot(v))
		if !cont.MatchString(line) {
			cur = nil
		}
	}
	if cur != nil {
		return nil, fmt.Errorf("%s ends in an unterminated continuation", cur.Name)
	}
	for _, v := range vars {
		if _, ok := doc.Blocks[v]; !ok {
			return nil, fmt.Errorf("%s is not assigned in this file", v)
		}
	}
	return doc, nil
}

// BlockEntries reads the entries back out of a rendered block, which is how the
// driver checks its own render. Lift cannot do it: Lift requires every managed
// variable to be present, and this is one assignment on its own.
func BlockEntries(lines []string) []string {
	var out []string
	for i, line := range lines {
		if i == 0 {
			line = assign.ReplaceAllString(line, "")
		}
		out = append(out, fields(line)...)
	}
	return out
}

// Continued reports whether lines form one assignment: every line but the last
// carries a continuation, and the last does not.
//
// Reading the entries back cannot see this, which is why it is a separate
// check. BlockEntries finds every entry whether or not the lines are joined, so
// a render that drops a backslash round-trips perfectly — and make then assigns
// only the first line, expanding the gate list to a fraction of itself. That is
// the silent state loss the round-trip check exists to stop, and entry
// membership alone does not stop it.
func Continued(lines []string) error {
	if len(lines) == 0 {
		return fmt.Errorf("the block is empty")
	}
	for i, line := range lines[:len(lines)-1] {
		if !cont.MatchString(line) {
			return fmt.Errorf("line %d ends the assignment early: %s", i+1, line)
		}
	}
	if cont.MatchString(lines[len(lines)-1]) {
		return fmt.Errorf("the last line continues into nothing")
	}
	return nil
}

// fields splits one line's entries off, dropping the continuation backslash.
// Space and tab only: make's separators, and nothing else in a line is one.
func fields(text string) []string {
	return strings.FieldsFunc(cont.ReplaceAllString(text, ""), func(r rune) bool {
		return r == ' ' || r == '\t'
	})
}

// Style is the shape of an assignment as one side wrote it. A render reuses it
// so the merged file reads like the file it came from rather than like this
// package's idea of one.
type Style struct {
	Op     string
	Indent string
	Width  int
}

const (
	defaultIndent = "                 "
	defaultWidth  = 100
	minWidth      = 60
)

// StyleOf reads the style off a block, falling back to this repo's own shape
// for anything it cannot see. Width is the head line plus a little slack, which
// is what keeps a re-render wrapping at roughly the column the file already
// wraps at.
//
// The indent comes from the block's own continuation lines and nowhere else. A
// block whose continuations carry no indent gets the default, rather than
// whatever leading whitespace happens to appear further down the file.
func StyleOf(b *Block) Style {
	s := Style{Op: ":=", Indent: defaultIndent, Width: defaultWidth}
	if b == nil || len(b.Lines) == 0 {
		return s
	}
	if found := op.FindString(b.Lines[0]); found != "" {
		s.Op = found
	}
	if w := len(b.Lines[0]) + 4; w >= minWidth {
		s.Width = w
	}
	for _, line := range b.Lines[1:] {
		if in := lead.FindString(line); in != "" {
			s.Indent = in
			break
		}
	}
	return s
}

// Render rebuilds an assignment from scratch. Only a removal needs it, because
// a removal has to rewrite the wrapped lines anyway.
func Render(varName string, st Style, entries []string) []string {
	head := varName + " " + st.Op
	var out []string
	line := head
	for _, e := range entries {
		cand := line + " " + e
		if len(cand) > st.Width && line != head {
			out = append(out, line+" \\")
			line = st.Indent + e
			continue
		}
		line = cand
	}
	return append(out, line)
}

// Append is ours' block verbatim with adds tacked on as new continuation lines.
//
// Leaving ours' existing lines alone is what holds the merge diff down to the
// entries that actually arrived: re-rendering rewraps every line of a 30-entry
// list, which is noise in every future merge and buries the real change during
// review.
func Append(block []string, adds []string, st Style) []string {
	if len(block) == 0 {
		return nil
	}
	out := append([]string{}, block[:len(block)-1]...)
	last := block[len(block)-1]
	if len(adds) == 0 {
		return append(out, last)
	}

	// The line that ended the assignment now continues into the additions.
	out = append(out, strings.TrimRight(last, " \t")+" \\")
	line := st.Indent
	for _, a := range adds {
		cand := line + a
		if line != st.Indent {
			cand = line + " " + a
		}
		if len(cand) > st.Width && line != st.Indent {
			out = append(out, line+" \\")
			line = st.Indent + a
			continue
		}
		line = cand
	}
	return append(out, line)
}

// Substitute puts each rendered block back where its sentinel stands.
//
// A missing sentinel means the body merge did not keep the line this package
// put there, so the pairing is gone and the result would silently lose a whole
// list. That is reported, never worked around.
func Substitute(body []string, vars []string, blocks map[string][]string) ([]string, error) {
	out := body
	for _, v := range vars {
		slot := Slot(v)
		found := false
		next := make([]string, 0, len(out))
		for _, line := range out {
			if line == slot {
				found = true
				next = append(next, blocks[v]...)
				continue
			}
			next = append(next, line)
		}
		if !found {
			return nil, fmt.Errorf("the %s placeholder did not survive the merge", v)
		}
		out = next
	}
	return out, nil
}
