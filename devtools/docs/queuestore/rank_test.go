package main

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestRankBetweenAnchorKeys(t *testing.T) {
	for _, c := range []struct{ lo, hi, want string }{
		{"", "", "a0"},   // the first item
		{"", "a0", "Zz"}, // below the first magnitude, at no extra length
		{"a0", "", "a1"}, // above it, likewise
		{"Zz", "", "a0"},
	} {
		got, err := RankBetween(c.lo, c.hi)
		if err != nil {
			t.Fatalf("RankBetween(%q, %q): %v", c.lo, c.hi, err)
		}
		if got != c.want {
			t.Errorf("RankBetween(%q, %q) = %q, want %q", c.lo, c.hi, got, c.want)
		}
	}
}

func TestRankBetweenPlacesStrictlyBetween(t *testing.T) {
	cases := []struct{ lo, hi string }{
		{"", ""},
		{"", "a0"},
		{"a0", ""},
		{"a0", "a1"},  // adjacent integers, so the room is fractional
		{"a0", "a0i"}, // hi extends lo
		{"Zz", "a0"},  // across the magnitude boundary
		{"a0i", "a1"},
		{"az", "b00"}, // where the head rolls over
		{"Z0", "Zz"},
	}
	for _, c := range cases {
		got, err := RankBetween(c.lo, c.hi)
		if err != nil {
			t.Fatalf("RankBetween(%q, %q): %v", c.lo, c.hi, err)
		}
		if err := CheckRank(got); err != nil {
			t.Errorf("RankBetween(%q, %q) = %q, which is malformed: %v", c.lo, c.hi, got, err)
		}
		if c.lo != "" && got <= c.lo {
			t.Errorf("RankBetween(%q, %q) = %q, not above lo", c.lo, c.hi, got)
		}
		if c.hi != "" && got >= c.hi {
			t.Errorf("RankBetween(%q, %q) = %q, not below hi", c.lo, c.hi, got)
		}
	}
}

func TestRankBetweenRejectsUnorderedBounds(t *testing.T) {
	for _, c := range []struct{ lo, hi string }{
		{"a1", "a0"},
		{"a0", "a0"},
		{"a0i", "a0"},
	} {
		if _, err := RankBetween(c.lo, c.hi); err == nil {
			t.Errorf("RankBetween(%q, %q) succeeded; want an error", c.lo, c.hi)
		}
	}
}

func TestCheckRankRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"a",                           // shorter than its head requires
		"0a",                          // no magnitude head
		"a00",                         // fraction ending in the lowest digit
		"A" + strings.Repeat("0", 26), // the reserved bottom
		"a0!",                         // outside base-36
	} {
		if err := CheckRank(bad); err == nil {
			t.Errorf("CheckRank(%q) succeeded; want an error", bad)
		}
	}
	for _, good := range []string{"a0", "Zz", "a0i", "b00i", "z" + strings.Repeat("0", 25) + "1"} {
		if err := CheckRank(good); err != nil {
			t.Errorf("CheckRank(%q) failed: %v", good, err)
		}
	}
}

// Head insertion is the case the process actually generates: flakes-first sends
// every new flake above the current top. A bare fractional key degrades here,
// growing a digit every few insertions — 500 of them reached 100 characters,
// which is why ranks carry a magnitude head at all. The bound is the assertion.
func TestRepeatedHeadInsertionStaysOrderedAndShort(t *testing.T) {
	ranks := []string{}
	top := ""
	for i := 0; i < 500; i++ {
		r, err := RankBetween("", top)
		if err != nil {
			t.Fatalf("insert %d above %q: %v", i, top, err)
		}
		if top != "" && r >= top {
			t.Fatalf("insert %d produced %q, not below %q", i, r, top)
		}
		if err := CheckRank(r); err != nil {
			t.Fatalf("insert %d produced malformed %q: %v", i, r, err)
		}
		ranks = append(ranks, r)
		top = r
	}
	if longest := maxLen(ranks); longest > 4 {
		t.Errorf("500 head insertions grew a rank to %d characters; want a short key", longest)
	}
}

func TestRepeatedTailInsertionStaysOrderedAndShort(t *testing.T) {
	ranks := []string{}
	last := ""
	for i := 0; i < 500; i++ {
		r, err := RankBetween(last, "")
		if err != nil {
			t.Fatalf("insert %d after %q: %v", i, last, err)
		}
		if r <= last {
			t.Fatalf("insert %d produced %q, not above %q", i, r, last)
		}
		ranks = append(ranks, r)
		last = r
	}
	if longest := maxLen(ranks); longest > 4 {
		t.Errorf("500 tail insertions grew a rank to %d characters; want a short key", longest)
	}
}

// The property that matters: whatever sequence of insertions runs, sorting the
// ranks reproduces the intended order. Insertion points are chosen at random so
// the walk covers interior splits, which head and tail insertion do not reach.
func TestRandomInsertionsPreserveIntendedOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	order := []int{}
	ranks := []string{}

	for n := 0; n < 400; n++ {
		at := 0
		if len(order) > 0 {
			at = rng.Intn(len(order) + 1)
		}
		lo, hi := "", ""
		if at > 0 {
			lo = ranks[at-1]
		}
		if at < len(ranks) {
			hi = ranks[at]
		}
		r, err := RankBetween(lo, hi)
		if err != nil {
			t.Fatalf("insert %d between %q and %q: %v", n, lo, hi, err)
		}
		if err := CheckRank(r); err != nil {
			t.Fatalf("insert %d produced malformed %q: %v", n, r, err)
		}
		order = append(order, 0)
		copy(order[at+1:], order[at:])
		order[at] = n
		ranks = append(ranks, "")
		copy(ranks[at+1:], ranks[at:])
		ranks[at] = r
	}

	sorted := append([]string(nil), ranks...)
	sort.Strings(sorted)
	for i := range ranks {
		if ranks[i] != sorted[i] {
			t.Fatalf("sorting the ranks does not reproduce the intended order at %d: %q vs %q", i, ranks[i], sorted[i])
		}
	}
	if len(uniq(ranks)) != len(ranks) {
		t.Errorf("insertions produced a duplicate rank")
	}
}

// A tie is the concurrency case: two sessions independently pick the same rank
// because neither saw the other. It must not be an error, and the ID has to
// settle it, or the order would depend on which side merged first.
func TestEqualRanksAreOrderedByID(t *testing.T) {
	items := []Item{
		{ID: "Q900", Rank: "a2"},
		{ID: "Q100", Rank: "a2"},
		{ID: "Q500", Rank: "a2"},
		{ID: "Q050", Rank: "a1"},
	}
	SortItems(items)
	got := []string{}
	for _, it := range items {
		got = append(got, it.ID)
	}
	want := []string{"Q050", "Q100", "Q500", "Q900"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("SortItems = %v, want %v", got, want)
	}
}

// AssignRanks seeds the store at import: existing line order becomes the
// initial ranks, and re-importing must produce the same keys.
func TestAssignRanksIsOrderedAndDeterministic(t *testing.T) {
	mk := func() []Item {
		return []Item{{ID: "Q1"}, {ID: "Q2"}, {ID: "Q3"}, {ID: "Q4"}}
	}
	a, b := mk(), mk()
	if err := AssignRanks(a); err != nil {
		t.Fatalf("AssignRanks: %v", err)
	}
	if err := AssignRanks(b); err != nil {
		t.Fatalf("AssignRanks: %v", err)
	}
	for i := range a {
		if a[i].Rank != b[i].Rank {
			t.Errorf("AssignRanks is not deterministic at %d: %q vs %q", i, a[i].Rank, b[i].Rank)
		}
		if i > 0 && a[i-1].Rank >= a[i].Rank {
			t.Errorf("AssignRanks produced %q then %q, which is not ascending", a[i-1].Rank, a[i].Rank)
		}
	}
}

func maxLen(ss []string) int {
	n := 0
	for _, s := range ss {
		if len(s) > n {
			n = len(s)
		}
	}
	return n
}

func uniq(ss []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range ss {
		m[s] = true
	}
	return m
}
