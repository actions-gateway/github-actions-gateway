package mdregistry

import (
	"errors"
	"regexp"
	"strings"
)

// sepRE matches a GFM column-separator row, the line that turns the row above
// it into a table header.
var sepRE = regexp.MustCompile(`^\|[-:| \t]+\|[ \t]*$`)

// Table is one table's contiguous data rows, together with everything from the
// previous table's rows up to and including this table's header and separator.
type Table struct {
	Pre  []string
	Rows []string
}

// Doc is a registry page carved into alternating prose and table-row segments.
// Post holds the tail after the last table.
type Doc struct {
	Tables []Table
	Post   []string
}

// ErrNoTables reports a page with no Markdown table in it, which is not a
// registry page this driver can merge.
var ErrNoTables = errors.New("no Markdown tables found")

// Split carves lines into alternating prose and table-row segments.
//
// Whole-file rather than one named section: these pages carry a table per
// group, and a row can move between groups when its subject is regrouped.
func Split(lines []string) (*Doc, error) {
	doc := &Doc{}
	var pre []string

	for i := 0; i < len(lines); {
		// A table starts at a header row whose next line is the column
		// separator. Anything else is prose, including a stray leading `|`.
		if strings.HasPrefix(lines[i], "|") && i+1 < len(lines) && sepRE.MatchString(lines[i+1]) {
			t := Table{Pre: append(pre, lines[i], lines[i+1])}
			pre = nil
			i += 2
			for i < len(lines) && strings.HasPrefix(lines[i], "|") {
				t.Rows = append(t.Rows, lines[i])
				i++
			}
			doc.Tables = append(doc.Tables, t)
			continue
		}
		pre = append(pre, lines[i])
		i++
	}

	if len(doc.Tables) == 0 {
		return nil, ErrNoTables
	}
	doc.Post = pre
	return doc, nil
}
