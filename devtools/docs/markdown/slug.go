package markdown

import (
	"strconv"
	"strings"
)

// Slugger assigns GitHub heading anchors within one document.
//
// The algorithm is github-slugger's, which is what GitHub's rendered headings
// use: lowercase, drop every character that is not [a-z0-9 _-], turn spaces
// into hyphens, and suffix a repeat with `-1`, `-2`, … Hyphen runs and
// leading/trailing hyphens are NOT collapsed — GitHub does not collapse them.
//
// goldmark's own WithAutoHeadingID is deliberately not used: it keeps Unicode
// letters GitHub drops, so its IDs and GitHub's diverge on any heading with a
// non-ASCII letter.
type Slugger struct {
	// occ maps a slug to the number of repeats issued for it. A key's presence
	// means the slug is taken, which is what makes `-1` land on the second
	// heading rather than the first.
	occ map[string]int
}

// NewSlugger returns a Slugger with no headings seen yet.
func NewSlugger() *Slugger {
	return &Slugger{occ: map[string]int{}}
}

// Slug returns the anchor for a heading's rendered text, de-duplicating
// against every heading already passed to this Slugger.
func (s *Slugger) Slug(text string) string {
	base := slugify(text)
	res := base
	for {
		if _, taken := s.occ[res]; !taken {
			break
		}
		s.occ[base]++
		res = base + "-" + strconv.Itoa(s.occ[base])
	}
	s.occ[res] = 0
	return res
}

func slugify(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}
