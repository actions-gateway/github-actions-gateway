// Package keyedrecords is the three-way merge the repo's registry merge drivers
// share: one block of keyed records, one per line, merged by key set-semantics
// with row order reconstructed.
//
// It replaces scripts/lib/merge-keyed-records.awk, which three drivers called
// and none could unit-test — every suite drove a whole driver end to end, so
// the order reconstruction below was only ever exercised through a file merge.
//
// The caller supplies the key reader, which is the only file-specific knowledge
// involved. A record whose key is empty is not well formed, and that is what
// makes an unparseable block a fallback rather than a guess.
package keyedrecords

import (
	"fmt"
	"strings"
)

// KeyFunc reads a record's stable key, returning "" when the line is not a
// well-formed record.
type KeyFunc func(line string) string

// Uncertain reports that the merge is not certain and the caller must fall back
// to ordinary conflict markers. Silence beats a guess: a wrongly resolved
// record loses registry state, whereas a conflict marker costs a minute.
type Uncertain struct {
	Reason string
}

func (e *Uncertain) Error() string { return e.Reason }

func uncertainf(format string, args ...any) error {
	return &Uncertain{Reason: fmt.Sprintf(format, args...)}
}

// side is one input to the merge, indexed as base, ours, theirs.
type side struct {
	name string
	text map[string]string
	seq  []string
	pos  map[string]int // 1-indexed position in seq
}

func readSide(name string, lines []string, key KeyFunc) (*side, error) {
	s := &side{
		name: name,
		text: make(map[string]string, len(lines)),
		pos:  make(map[string]int, len(lines)),
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		id := key(line)
		if id == "" {
			return nil, uncertainf("%s: not a well-formed record: %s", name, truncate(line, 60))
		}
		if _, dup := s.text[id]; dup {
			return nil, uncertainf("%s: %s appears twice in the block", name, id)
		}
		s.text[id] = line
		s.seq = append(s.seq, id)
		s.pos[id] = len(s.seq)
	}
	return s, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Merge resolves base/ours/theirs into one record block.
//
// The rules, per key:
//   - deleted on either side (and unchanged on the other) -> deleted
//   - added on either side                                -> present
//   - changed on one side only                            -> that change
//   - changed identically on both sides                   -> that change
//   - changed differently on both sides                   -> uncertain
//   - deleted on one side, changed on the other           -> uncertain
//   - same new key added on both sides with different text -> uncertain
//
// Record order is meaningful in these files, so it is reconstructed rather than
// assumed: the records both sides kept form a skeleton, and each side's
// additions are spliced back in at the position that side put them. When the
// two sides order the shared records differently, the side that still agrees
// with the base did not reorder, so the other side's order is the intended one.
// When both reordered, that is uncertain.
func Merge(base, ours, theirs []string, key KeyFunc) ([]string, error) {
	b, err := readSide("base", base, key)
	if err != nil {
		return nil, err
	}
	o, err := readSide("ours", ours, key)
	if err != nil {
		return nil, err
	}
	t, err := readSide("theirs", theirs, key)
	if err != nil {
		return nil, err
	}

	keep, err := resolve(b, o, t)
	if err != nil {
		return nil, err
	}

	skeleton, err := orderSkeleton(b, o, t, keep)
	if err != nil {
		return nil, err
	}

	return emit(o, t, keep, skeleton)
}

// allKeys lists every key any side holds, base order first, then the keys ours
// introduced, then theirs. awk iterated its hash, so which of several
// conflicting records got named in the refusal was unspecified; a stable walk
// makes the reason deterministic and therefore assertable.
func allKeys(b, o, t *side) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range []*side{b, o, t} {
		for _, id := range s.seq {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// resolve applies the per-key rules, returning the surviving record text.
func resolve(b, o, t *side) (map[string]string, error) {
	keep := make(map[string]string)
	for _, id := range allKeys(b, o, t) {
		bt, hb := b.text[id]
		ot, ho := o.text[id]
		tt, ht := t.text[id]

		switch {
		case hb && ho && ht:
			switch {
			case ot == tt:
				keep[id] = ot
			case ot == bt:
				keep[id] = tt
			case tt == bt:
				keep[id] = ot
			default:
				return nil, uncertainf("%s was changed differently on both sides", id)
			}
		case hb && ho:
			// theirs deleted it. Only an untouched record may go quietly: one
			// deleted on one side and edited on the other is the classic
			// delete/modify, and which intent wins is not ours to pick.
			if ot != bt {
				return nil, uncertainf("%s was deleted on one side and changed on the other", id)
			}
		case hb && ht:
			if tt != bt {
				return nil, uncertainf("%s was deleted on one side and changed on the other", id)
			}
		case hb:
			// Both deleted it: the agreed-on outcome.
		case ho && ht:
			if ot != tt {
				return nil, uncertainf("%s was filed on both sides with different content", id)
			}
			keep[id] = ot
		case ho:
			keep[id] = ot
		case ht:
			keep[id] = tt
		}
	}
	return keep, nil
}

// shared lists s's records that survived and that other also holds, in s's own
// order.
func shared(s, other *side, keep map[string]string) []string {
	var out []string
	for _, id := range s.seq {
		if _, live := keep[id]; !live {
			continue
		}
		if _, both := other.text[id]; both {
			out = append(out, id)
		}
	}
	return out
}

func seqEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// orderSkeleton picks the order the records both sides kept should appear in.
func orderSkeleton(b, o, t *side, keep map[string]string) ([]string, error) {
	sharedOurs := shared(o, t, keep)
	sharedTheirs := shared(t, o, keep)

	if seqEqual(sharedOurs, sharedTheirs) {
		// Both sides agree: nothing was reordered, or both reordered the same
		// way.
		return sharedOurs, nil
	}

	// They disagree, so at least one side reordered. Compare each side's order
	// of the shared-and-in-base records against the base's: the side that still
	// matches the base is the one that did not reorder.
	inBase := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if _, ok := b.text[id]; ok {
				out = append(out, id)
			}
		}
		return out
	}
	var baseOrder []string
	for _, id := range b.seq {
		if _, live := keep[id]; !live {
			continue
		}
		_, inOurs := o.text[id]
		_, inTheirs := t.text[id]
		if inOurs && inTheirs {
			baseOrder = append(baseOrder, id)
		}
	}

	oursKeptBase := seqEqual(inBase(sharedOurs), baseOrder)
	theirsKeptBase := seqEqual(inBase(sharedTheirs), baseOrder)
	switch {
	case oursKeptBase && !theirsKeptBase:
		return sharedTheirs, nil
	case theirsKeptBase && !oursKeptBase:
		return sharedOurs, nil
	default:
		return nil, uncertainf("rows were reordered on both sides")
	}
}

// emit lays the skeleton down in order, splicing each side's additions in at
// the position that side put them.
//
// Only additions are spliced ahead of a skeleton entry. Sweeping every earlier
// record of a side would drag skeleton records forward out of their agreed
// order, since the two sides' positions for them need not line up.
func emit(o, t *side, keep map[string]string, skeleton []string) ([]string, error) {
	inSkeleton := make(map[string]bool, len(skeleton))
	for _, id := range skeleton {
		inSkeleton[id] = true
	}

	var out []string
	emitted := make(map[string]bool, len(keep))
	push := func(id string) {
		text, live := keep[id]
		if !live || emitted[id] {
			return
		}
		emitted[id] = true
		out = append(out, text)
	}

	oi, ti := 0, 0
	for _, id := range skeleton {
		for oi < o.pos[id]-1 {
			add := o.seq[oi]
			oi++
			if !inSkeleton[add] {
				push(add)
			}
		}
		for ti < t.pos[id]-1 {
			add := t.seq[ti]
			ti++
			if !inSkeleton[add] {
				push(add)
			}
		}
		push(id)
		oi = o.pos[id]
		ti = t.pos[id]
	}

	// Additions past the last skeleton entry, in each side's own order, plus the
	// ones stepped over above, which happens when the skeleton follows one
	// side's order and the other side's positions are therefore not monotonic.
	for _, id := range o.seq {
		if !inSkeleton[id] {
			push(id)
		}
	}
	for _, id := range t.seq {
		if !inSkeleton[id] {
			push(id)
		}
	}

	// Completeness backstop. push is idempotent, so this cannot duplicate a
	// record; it exists so that no surviving record can be dropped even if the
	// ordering pass above ever misses one. Losing registry state is the one
	// outcome worse than a conflict marker.
	for _, id := range o.seq {
		push(id)
	}
	for _, id := range t.seq {
		push(id)
	}

	if len(out) != len(keep) {
		return nil, uncertainf("internal: emitted %d of %d surviving records", len(out), len(keep))
	}
	return out, nil
}
