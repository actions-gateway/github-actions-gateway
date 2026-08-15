package main

import (
	"fmt"
	"sort"
	"strings"
)

// Status values. The Queue's St column carries the first two as glyphs; a
// Deferred row has no St column at all, which is what deferred means.
const (
	StatusReady    = "ready"
	StatusBlocked  = "blocked"
	StatusDeferred = "deferred"
)

// glyphs maps the Queue St cell to a status. Only these two are legal in the
// Queue; ✅/▶/💤 are old-format markers that backloglint rule 3 already rejects.
var glyphs = map[string]string{
	"🔲": StatusReady,
	"🚫": StatusBlocked,
}

// Item is one backlog item: the storage record, one file under docs/queue/.
//
// Every field is owned by this item alone, which is the point — ordering lives
// in Rank rather than in a position, so placing an item writes only its own
// file and two items edited concurrently cannot conflict.
type Item struct {
	ID     string   `yaml:"id"`
	Rank   string   `yaml:"rank"`
	Labels []string `yaml:"labels,omitempty"`
	Status string   `yaml:"status"`
	Size   string   `yaml:"size"`

	// Target is the Item cell's link destination, empty when the cell is bare
	// text. Two of the live Deferred rows are bare text, so this is optional
	// rather than required.
	Target string `yaml:"target,omitempty"`

	// Title is the Item cell's text, and Notes the Queue's Notes cell or the
	// Deferred table's revive trigger. Both are Markdown source.
	Title string `yaml:"-"`
	Notes string `yaml:"-"`
}

// SortItems orders items by rank, breaking ties on ID.
//
// The tiebreak is what makes concurrent insertion safe: two sessions that never
// saw each other can choose the same rank, and without a tiebreak the resulting
// order would depend on which side merged first.
func SortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Rank != items[j].Rank {
			return items[i].Rank < items[j].Rank
		}
		return items[i].ID < items[j].ID
	})
}

// AssignRanks seeds items with ascending ranks in their current slice order.
// It is the import path: the table's existing line order becomes the store's
// initial ranks, once, after which ordering is a per-item value.
//
// Deterministic, so re-importing the same table yields the same keys and a
// rebase that re-runs the import produces no spurious diff.
func AssignRanks(items []Item) error {
	prev := ""
	for i := range items {
		r, err := RankBetween(prev, "")
		if err != nil {
			return fmt.Errorf("assigning a rank after %q: %w", prev, err)
		}
		items[i].Rank = r
		prev = r
	}
	return nil
}

// Validate reports the first problem with an item, so a malformed file fails
// where it is read rather than where it is rendered.
func (it Item) Validate() error {
	if it.ID == "" {
		return fmt.Errorf("item has no id")
	}
	if err := CheckRank(it.Rank); err != nil {
		return fmt.Errorf("%s: %w", it.ID, err)
	}
	switch it.Status {
	case StatusReady, StatusBlocked, StatusDeferred:
	default:
		return fmt.Errorf("%s: status %q is not one of ready, blocked, deferred", it.ID, it.Status)
	}
	if it.Title == "" {
		return fmt.Errorf("%s: item has no title", it.ID)
	}
	return nil
}

// Deferred reports whether the item renders into the Deferred table rather than
// the Queue.
func (it Item) Deferred() bool { return it.Status == StatusDeferred }

// itemCell renders the Item column: a link when the item has a target, and the
// bare title when it does not.
func (it Item) itemCell() string {
	if it.Target == "" {
		return it.Title
	}
	return "[" + it.Title + "](" + it.Target + ")"
}

// labelCell renders the Labels column as the backtick-delimited list the tables
// carry.
func (it Item) labelCell() string {
	quoted := make([]string, 0, len(it.Labels))
	for _, l := range it.Labels {
		quoted = append(quoted, "`"+l+"`")
	}
	return strings.Join(quoted, " ")
}

// statusGlyph renders the Queue St column. Deferred items have no St column, so
// this is only reached for Queue rows.
func (it Item) statusGlyph() string {
	for g, s := range glyphs {
		if s == it.Status {
			return g
		}
	}
	return ""
}

// Row renders the item as one GFM table row, in the column order of whichever
// table it belongs to.
func (it Item) Row() string {
	cells := []string{anchor(it.ID), it.itemCell(), it.labelCell()}
	if !it.Deferred() {
		cells = append(cells, it.statusGlyph())
	}
	cells = append(cells, it.Size, it.Notes)
	return "| " + strings.Join(cells, " | ") + " |"
}

// anchor renders the ID cell. The inline <a id> is what makes `#QNNN` links
// resolve on github.com, which no generated table can supply.
func anchor(id string) string { return `<a id="` + id + `"></a>` + id }
