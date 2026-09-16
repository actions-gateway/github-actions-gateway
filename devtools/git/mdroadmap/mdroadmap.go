// Package mdroadmap carves docs/roadmap.md into the prose and annotated-bullet
// segments its merge driver merges separately, and reads the backlog binding
// those bullets are merged by.
//
// A bullet spans several source lines, which the shared record merge does not
// model, so each one is encoded onto a single line with SOH standing in for the
// newline and decoded again afterwards. The blank lines between bullets ride
// beside the records rather than inside them: a bullet does not own the spacing
// around it, and folding the trailing blank into the record turns deleting a
// list's last bullet into an edit of its neighbour — the merge the driver
// exists to resolve.
//
// It is the Markdown half of the driver behind scripts/docs/git-merge-roadmap.sh;
// the set merge itself is in devtools/git/keyedrecords.
package mdroadmap

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Sep stands in for the newline inside an encoded multi-line record. A source
// line that already carries one is refused rather than encoded, since the
// decode could not tell the two apart.
const Sep = "\x01"

var (
	// The annotation as it appears inside an encoded record. The payload class
	// excludes Sep as well as `-`, so a truncated `<!--` can never reach across
	// an encoded line break into the next bullet's annotation.
	recordAnnotRE = regexp.MustCompile("<!--[ \t]*q:([^-\x01]*)-->")

	// The same annotation on a decoded line, where no record separator exists.
	lineAnnotRE = regexp.MustCompile("<!--[ \t]*q:([^-]*)-->")

	qIDRE = regexp.MustCompile(`^Q[0-9]+$`)
)

// markerIDs lists the backlog IDs annotated on line, in source order. Every
// annotation on the bullet contributes, because devtools/docs/roadmapcheck
// reads them all.
func markerIDs(line string, re *regexp.Regexp) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(line, -1) {
		payload := strings.NewReplacer(" ", "", "\t", "").Replace(m[1])
		for _, id := range strings.Split(payload, ",") {
			if id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// MarkerKey reads the `<!-- q:QN[,QM…] -->` binding a bullet carries, comma
// joined in source order, which is what keys the page: one binding, one bullet.
// It mirrors devtools/docs/roadmapcheck's annotation reader, so a bullet whose
// binding the checker cannot parse is one this cannot key either.
//
// An ID the backlog itself would not recognize unkeys the whole bullet, which
// is what makes a malformed annotation a fallback rather than a guess.
func MarkerKey(line string) string {
	if !isBullet(line) {
		return ""
	}
	ids := markerIDs(line, recordAnnotRE)
	for _, id := range ids {
		if !qIDRE.MatchString(id) {
			return ""
		}
	}
	return strings.Join(ids, ",")
}

// DupeKey reads the same binding off a decoded line, for the whole-page
// uniqueness check. It does not require the IDs to be well formed: by the time
// that check runs, an unkeyable bullet is one the driver left in prose, and two
// prose bullets naming one row are still two bullets for one row.
func DupeKey(line string) string {
	if !isBullet(line) {
		return ""
	}
	return strings.Join(markerIDs(line, lineAnnotRE), ",")
}

// List is one run of annotated bullets: the prose that precedes it, one encoded
// record per bullet, the blank-line count that follows each of them, and the
// count that trails the list as a whole.
type List struct {
	Pre     []string
	Records []string
	Blanks  []int
	Tail    int
}

// Doc is a roadmap page carved into alternating prose and bullet-list segments.
// Post holds the tail after the last list.
type Doc struct {
	Lists []List
	Post  []string
}

// ErrNoLists reports a page with no annotated bullet list on it, which is not a
// roadmap this driver can merge.
var ErrNoLists = errors.New("no annotated bullet lists found")

func isBullet(s string) bool { return strings.HasPrefix(s, "- ") }

func isBlank(s string) bool { return strings.TrimRight(s, " \t") == "" }

func isIndented(s string) bool { return strings.HasPrefix(s, " ") || strings.HasPrefix(s, "\t") }

// Split carves lines into alternating prose and bullet-list segments.
//
// A list is a maximal run of top-level `- ` bullets, and it owns the blank lines
// that trail it — so the run reaches all the way to the next prose line, and the
// last bullet has a separator like every other. A run is a list only when every
// bullet in it is annotated; anything else is prose, which is how an ordinary
// bulleted paragraph elsewhere on the page keeps git's own merge.
func Split(lines []string) (*Doc, error) {
	doc := &Doc{}
	var pre []string

	for i := 0; i < len(lines); {
		if !isBullet(lines[i]) {
			pre = append(pre, lines[i])
			i++
			continue
		}
		// The run extends over every bullet, continuation and blank line that
		// follows, and stops at the first column-0 line that is neither.
		j := i + 1
		for j < len(lines) && (isBlank(lines[j]) || isIndented(lines[j]) || isBullet(lines[j])) {
			j++
		}

		list, keyed, err := encode(lines[i:j], i)
		if err != nil {
			return nil, err
		}
		if keyed {
			list.Pre = pre
			pre = nil
			doc.Lists = append(doc.Lists, *list)
		} else {
			pre = append(pre, lines[i:j]...)
		}
		i = j
	}

	if len(doc.Lists) == 0 {
		return nil, ErrNoLists
	}
	doc.Post = pre
	return doc, nil
}

// encode folds one run of lines into records, reporting whether the run is a
// mergeable list at all.
//
// A blank line inside the run is undecided when it is read: absorbed into the
// record if a continuation follows, counted as a separator if a bullet does. The
// annotation is checked on the assembled record rather than the first line,
// because a bullet whose title wraps can carry its binding further down. A
// separator that is not an empty line would not survive being rebuilt from a
// count, so it disqualifies the run the same way an unannotated bullet does.
//
// Keying here is the annotation matching, while MarkerKey additionally requires
// every ID to be well formed. The two deliberately disagree: a run whose
// bindings are malformed is encoded, reaches the merge with empty keys, and is
// refused there as unparseable — one uncertain merge taking the fallback,
// rather than every malformed bullet colliding on a single empty key. Tightening
// this to match MarkerKey would turn that refusal into a silent collision.
func encode(run []string, offset int) (*List, bool, error) {
	list := &List{}
	pend, pendN := "", 0
	plain := true

	for k, line := range run {
		if strings.Contains(line, Sep) {
			return nil, false, fmt.Errorf("a source line already contains the record separator (line %d)", offset+k+1)
		}
		switch {
		case isBullet(line):
			if len(list.Records) > 0 {
				list.Blanks[len(list.Blanks)-1] = pendN
			}
			pend, pendN = "", 0
			list.Records = append(list.Records, line)
			list.Blanks = append(list.Blanks, 0)
		case isBlank(line):
			pend += Sep + line
			pendN++
			if line != "" {
				plain = false
			}
		default:
			last := len(list.Records) - 1
			list.Records[last] += pend + Sep + line
			pend, pendN = "", 0
		}
	}
	list.Blanks[len(list.Blanks)-1] = pendN
	list.Tail = pendN

	keyed := plain
	for _, rec := range list.Records {
		if !recordAnnotRE.MatchString(rec) {
			keyed = false
		}
	}
	return list, keyed, nil
}

// Decode turns one merged block of encoded records back into Markdown lines,
// restoring the blank lines around them.
//
// A record is looked up by its own text, which is exactly what survived the set
// merge, so no key has to be recomputed here. The spacing after a record follows
// the same three-way rule the records themselves do, and a side that respaced a
// bullet the other side respaced differently is uncertain like anything else.
//
// The last surviving record takes the list's trailing count rather than its own
// recorded separator, because that separator described the bullet that used to
// follow it. Without the override, deleting a tight list's final bullet would
// leave its predecessor welded to the next heading.
func Decode(base, ours, theirs *List, merged []string) ([]string, error) {
	hb, ho, ht := base.spacing(), ours.spacing(), theirs.spacing()

	var tail int
	switch {
	case ours.Tail == theirs.Tail:
		tail = ours.Tail
	case ours.Tail == base.Tail:
		tail = theirs.Tail
	case theirs.Tail == base.Tail:
		tail = ours.Tail
	default:
		return nil, errors.New("the blank lines after the list were changed on both sides")
	}

	var out []string
	for k, rec := range merged {
		vb, inBase := hb[rec]
		vo, inOurs := ho[rec]
		vt, inTheirs := ht[rec]

		var v int
		switch {
		case inOurs && inTheirs:
			switch {
			case vo == vt:
				v = vo
			case inBase && vo == vb:
				v = vt
			case inBase && vt == vb:
				v = vo
			default:
				return nil, errors.New("a bullet was respaced differently on both sides")
			}
		case inOurs:
			v = vo
		case inTheirs:
			v = vt
		case inBase:
			v = vb
		default:
			// Unreachable while every side records a separator for every record
			// it holds, and a blank line if it ever is not.
			v = 1
		}
		if k == len(merged)-1 {
			v = tail
		}

		out = append(out, strings.Split(rec, Sep)...)
		for b := 0; b < v; b++ {
			out = append(out, "")
		}
	}
	return out, nil
}

// spacing indexes a side's blank-line counts by record text, which is what the
// set merge preserved.
func (l *List) spacing() map[string]int {
	m := make(map[string]int, len(l.Records))
	for i, rec := range l.Records {
		m[rec] = l.Blanks[i]
	}
	return m
}
