package main

import (
	"fmt"
	"strings"
)

// digits is the rank alphabet for the fractional part, ordered so that byte
// comparison of two ranks is their order.
const digits = "0123456789abcdefghijklmnopqrstuvwxyz"

// A rank is an order key: a magnitude head, an integer part whose length the
// head fixes, and an optional fraction. Plain string comparison orders two
// ranks, so placing an item only has to name a string between its neighbours,
// which is a write to that item's own file and to nothing else.
//
// The head is what keeps keys short. Midpointing alone is unbounded but
// degrades where it is used most: repeatedly inserting below the smallest key
// prepends a digit every few insertions, and flakes-first sends every new flake
// to the top. A head instead lets the integer part step down whole magnitudes —
// "a0" to "Zz" to "Zy" — so head and tail insertion cost no length at all until
// a magnitude is exhausted.
//
// Heads 'a' to 'z' carry integer lengths 2 to 27 upward; 'Z' down to 'A' carry
// 2 to 27 downward. A fraction may not end in the lowest digit, since "x0" and
// "x" denote the same value and midpointing toward a trailing zero would not
// terminate.

// smallestInteger is the bottom of the space: head 'A' takes 26 digits.
var smallestInteger = "A" + strings.Repeat(string(digits[0]), 26)

// RankBetween returns a rank strictly between lo and hi. An empty lo means
// "before every item", an empty hi means "after every item"; both empty yields
// the rank of a first item.
func RankBetween(lo, hi string) (string, error) {
	if lo != "" {
		if err := CheckRank(lo); err != nil {
			return "", fmt.Errorf("lo: %w", err)
		}
	}
	if hi != "" {
		if err := CheckRank(hi); err != nil {
			return "", fmt.Errorf("hi: %w", err)
		}
	}
	if lo != "" && hi != "" && lo >= hi {
		return "", fmt.Errorf("rank %q is not below %q", lo, hi)
	}

	if lo == "" {
		if hi == "" {
			return "a" + string(digits[0]), nil
		}
		ih, err := integerPart(hi)
		if err != nil {
			return "", err
		}
		fh := hi[len(ih):]
		if ih == smallestInteger {
			return ih + midpoint("", fh), nil
		}
		// The integer part alone already sits below hi when hi carries a
		// fraction, which costs no length.
		if ih < hi {
			return ih, nil
		}
		prev, err := decrementInteger(ih)
		if err != nil {
			return "", err
		}
		return prev, nil
	}

	il, err := integerPart(lo)
	if err != nil {
		return "", err
	}
	fl := lo[len(il):]

	if hi == "" {
		next, err := incrementInteger(il)
		if err != nil {
			// The top magnitude is exhausted, so the room left is fractional.
			return il + midpoint(fl, ""), nil
		}
		return next, nil
	}

	ih, err := integerPart(hi)
	if err != nil {
		return "", err
	}
	fh := hi[len(ih):]
	if il == ih {
		return il + midpoint(fl, fh), nil
	}
	next, err := incrementInteger(il)
	if err != nil {
		return "", err
	}
	if next < hi {
		return next, nil
	}
	return il + midpoint(fl, ""), nil
}

// CheckRank reports whether r is a well-formed order key.
func CheckRank(r string) error {
	if r == "" {
		return fmt.Errorf("rank is empty")
	}
	if r == smallestInteger {
		return fmt.Errorf("rank %q is the reserved bottom of the space", r)
	}
	i, err := integerPart(r)
	if err != nil {
		return err
	}
	frac := r[len(i):]
	if strings.TrimLeft(frac, digits) != "" {
		return fmt.Errorf("rank %q holds a character outside base-36 after its integer part", r)
	}
	if frac != "" && frac[len(frac)-1] == digits[0] {
		return fmt.Errorf("rank %q ends in %q, which denotes the same value as the rank without it", r, string(digits[0]))
	}
	return nil
}

// integerLength returns how many characters, head included, the integer part
// takes for a given head.
func integerLength(head byte) (int, error) {
	switch {
	case head >= 'a' && head <= 'z':
		return int(head-'a') + 2, nil
	case head >= 'A' && head <= 'Z':
		return int('Z'-head) + 2, nil
	}
	return 0, fmt.Errorf("rank head %q is not a magnitude character", string(head))
}

// integerPart returns the leading integer part of a rank.
func integerPart(r string) (string, error) {
	n, err := integerLength(r[0])
	if err != nil {
		return "", err
	}
	if n > len(r) {
		return "", fmt.Errorf("rank %q is shorter than the %d characters its head requires", r, n)
	}
	return r[:n], nil
}

// incrementInteger returns the next integer part, erroring when the top
// magnitude is exhausted.
func incrementInteger(x string) (string, error) {
	head, digs := x[0], []byte(x[1:])
	carry := true
	for i := len(digs) - 1; carry && i >= 0; i-- {
		d := strings.IndexByte(digits, digs[i]) + 1
		if d == len(digits) {
			digs[i] = digits[0]
			continue
		}
		digs[i] = digits[d]
		carry = false
	}
	if !carry {
		return string(head) + string(digs), nil
	}
	if head == 'Z' {
		return "a" + string(digits[0]), nil
	}
	if head == 'z' {
		return "", fmt.Errorf("rank %q is at the top of the space", x)
	}
	next := head + 1
	if next > 'a' {
		digs = append(digs, digits[0])
	} else {
		digs = digs[:len(digs)-1]
	}
	return string(next) + string(digs), nil
}

// decrementInteger returns the previous integer part, erroring when the bottom
// magnitude is exhausted.
func decrementInteger(x string) (string, error) {
	head, digs := x[0], []byte(x[1:])
	borrow := true
	for i := len(digs) - 1; borrow && i >= 0; i-- {
		d := strings.IndexByte(digits, digs[i]) - 1
		if d == -1 {
			digs[i] = digits[len(digits)-1]
			continue
		}
		digs[i] = digits[d]
		borrow = false
	}
	if !borrow {
		return string(head) + string(digs), nil
	}
	if head == 'a' {
		return "Z" + string(digits[len(digits)-1]), nil
	}
	if head == 'A' {
		return "", fmt.Errorf("rank %q is at the bottom of the space", x)
	}
	prev := head - 1
	if prev < 'Z' {
		digs = append(digs, digits[len(digits)-1])
	} else {
		digs = digs[:len(digs)-1]
	}
	return string(prev) + string(digs), nil
}

// midpoint returns a fraction strictly between lo and hi, treating an empty hi
// as the top. It assumes well-formed, ordered inputs.
func midpoint(lo, hi string) string {
	if hi != "" {
		// Descend through the shared prefix: it constrains nothing, and
		// dropping it is what keeps the result minimal-length.
		n := 0
		for n < len(hi) && digitAt(lo, n) == hi[n] {
			n++
		}
		if n > 0 {
			return hi[:n] + midpoint(clip(lo, n), hi[n:])
		}
	}

	lead := 0
	if lo != "" {
		lead = strings.IndexByte(digits, lo[0])
	}
	limit := len(digits)
	if hi != "" {
		limit = strings.IndexByte(digits, hi[0])
	}

	// A gap in the leading digit is the common case and ends it in one digit.
	if limit-lead > 1 {
		return string(digits[(lead+limit)/2])
	}
	// Leading digits are adjacent. When hi has more to say, its own leading
	// digit alone already sits above lo and below hi.
	if len(hi) > 1 {
		return hi[:1]
	}
	// hi is a single digit or absent, so the room is below it: keep lo's
	// leading digit and place the rest above lo's tail.
	return string(digits[lead]) + midpoint(clip(lo, 1), "")
}

// digitAt returns s[i], or the lowest digit when s has ended, which is the
// value an unwritten fractional digit denotes.
func digitAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return digits[0]
}

// clip returns s[n:], or "" when s is shorter than n.
func clip(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}
