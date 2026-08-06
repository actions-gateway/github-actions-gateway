package main

import (
	"reflect"
	"testing"
)

func TestParseSubject(t *testing.T) {
	tests := []struct {
		name     string
		subject  string
		wantType string
		wantScop []string
		wantBrk  bool
		wantOK   bool
	}{
		{"plain type", "docs: tidy the README", "docs", nil, false, true},
		{"scoped", "feat(agc): export a gauge", "feat", []string{"agc"}, false, true},
		{"breaking scoped", "feat(agc)!: drop the old field", "feat", []string{"agc"}, true, true},
		{"breaking unscoped", "refactor!: rename everything", "refactor", nil, true, true},
		{"compound scope", "fix(agc,gmc): share the guard", "fix", []string{"agc", "gmc"}, false, true},
		{"compound spaced", "fix(agc, gmc): share the guard", "fix", []string{"agc", "gmc"}, false, true},
		{"breaking refactor is still breaking", "refactor(api)!: split the axes", "refactor", []string{"api"}, true, true},
		{"colon in the description", "feat(agc): note: this is fine", "feat", []string{"agc"}, false, true},
		{"trailing PR number", "fix(gmc): bound the retry (Q1) (#2)", "fix", []string{"gmc"}, false, true},

		// A subject whose type cannot be read is reported, never guessed at:
		// its semver weight is unknown, and main.go surfaces it as such.
		{"merge subject", "Merge pull request #1 from x", "", nil, false, false},
		{"sentence case", "Re-measure the eviction split (Q657)", "", nil, false, false},
		{"no space after colon", "feat(agc):export a gauge", "", nil, false, false},
		{"uppercase type", "Feat(agc): export a gauge", "", nil, false, false},
		{"bang before scope is not the spec form", "feat!(agc): nope", "", nil, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			typ, scopes, brk, ok := parseSubject(tc.subject)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if typ != tc.wantType {
				t.Errorf("type = %q, want %q", typ, tc.wantType)
			}
			if !reflect.DeepEqual(scopes, tc.wantScop) {
				t.Errorf("scopes = %v, want %v", scopes, tc.wantScop)
			}
			if brk != tc.wantBrk {
				t.Errorf("breaking = %v, want %v", brk, tc.wantBrk)
			}
		})
	}
}

func TestHasBreakingFooter(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"space form", "subject\n\nBREAKING CHANGE: the field moved\n", true},
		{"hyphen form", "subject\n\nBREAKING-CHANGE: the field moved\n", true},
		{"absent", "subject\n\nJust an ordinary body.\n", false},
		// Must be a footer at line start: a body that merely discusses one is
		// not a commit that declares one.
		{"mentioned mid-line", "subject\n\nThis is not a BREAKING CHANGE: really\n", false},
		{"lowercase is not the spec form", "subject\n\nbreaking change: nope\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasBreakingFooter(tc.body); got != tc.want {
				t.Errorf("hasBreakingFooter = %v, want %v", got, tc.want)
			}
		})
	}
}
