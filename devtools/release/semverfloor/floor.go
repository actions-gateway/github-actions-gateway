package main

import (
	"path"
	"sort"
	"strings"
)

// Level is the semver bump a commit forces. Ordered, so the window's floor is
// the maximum over its commits.
//
// There is deliberately no major level. A breaking marker cannot be resolved
// mechanically in this repo: whether it broke anything depends on whether the
// FROM tag had published the surface it changed, which is a field-level
// question. Every breaking marker in the repo's history — the three in
// v1.2.0..v1.3.0, and there are no others — changed surface v1.2.0 never
// published, and v1.3.0 duly shipped as a minor. So a marker raises a question
// (Result.Unresolved) rather than a floor, and api-surface-since.sh answers it.
type Level int

const (
	LevelNone Level = iota
	LevelPatch
	LevelMinor
)

func (l Level) String() string {
	switch l {
	case LevelPatch:
		return "patch"
	case LevelMinor:
		return "minor"
	default:
		return "none"
	}
}

// Surface is the set of paths a release artifact is built from: the package
// directories the released binaries compile, and whole trees (charts) that are
// packaged as-is.
type Surface struct {
	PkgDirs map[string]bool // exact Go package directories
	Trees   []string        // path prefixes packaged wholesale
}

// Ships reports whether a path is one a released artifact is built from.
//
// Package directories match exactly: `go list -deps` reports the directory of
// every package that compiles in, so a file's own directory is the unit, and a
// sibling directory that nothing imports is correctly excluded.
func (s Surface) Ships(file string) bool {
	if s.PkgDirs[path.Dir(file)] {
		return true
	}
	for _, t := range s.Trees {
		if file == t || strings.HasPrefix(file, t+"/") {
			return true
		}
	}
	return false
}

// ShippedFiles returns the commit's files that land on the released surface.
func (s Surface) ShippedFiles(c Commit) []string {
	var out []string
	for _, f := range c.Files {
		if s.Ships(f) {
			out = append(out, f)
		}
	}
	return out
}

// levelFor is the semver weight of a commit that has already been shown to
// touch the released surface.
//
// `refactor` and `perf` differ here on purpose: a refactor is defined as making
// no observable change, so it carries no floor however much shipped code it
// moves, while a perf change is observable and rates a patch. A breaking marker
// is not consulted — see Level.
func levelFor(c Commit) Level {
	switch c.Type {
	case "feat":
		return LevelMinor
	case "fix", "perf":
		return LevelPatch
	default:
		return LevelNone
	}
}

// Verdict is one commit's classification, carrying the evidence for it.
type Verdict struct {
	Commit  Commit
	Level   Level
	Shipped []string // the files that put it on the released surface
}

// Result is the floor over a window, plus what a reader needs to check it.
type Result struct {
	Floor Level
	// Raising holds every commit that ships and carries a level, floor-first.
	Raising []Verdict
	// Withheld holds commits whose type would have raised the floor but whose
	// paths ship nothing. This is the difference between counting `feat`
	// subjects and reading what a release contains, so it is reported rather
	// than silently dropped.
	Withheld []Verdict
	// Unresolved holds shipping commits carrying a breaking marker. Each is a
	// major the tool cannot confirm or dismiss; a commit here may also appear
	// in Raising, since its type still carries a floor of its own.
	Unresolved []Verdict
	// NonConventional holds subjects with no readable type. Each is a commit
	// whose weight was not assessed.
	NonConventional []Commit
}

// Classify computes the floor over a window.
func Classify(commits []Commit, surface Surface) Result {
	var r Result
	for _, c := range commits {
		if c.Type == "" {
			r.NonConventional = append(r.NonConventional, c)
			continue
		}
		lvl := levelFor(c)
		shipped := surface.ShippedFiles(c)
		v := Verdict{Commit: c, Level: lvl, Shipped: shipped}

		// A breaking marker is assessed on shipping alone: a `refactor!` carries
		// no floor by type, and would otherwise never be looked at.
		if c.Breaking && len(shipped) > 0 {
			r.Unresolved = append(r.Unresolved, v)
		}
		if lvl == LevelNone {
			continue
		}
		if len(shipped) == 0 {
			r.Withheld = append(r.Withheld, v)
			continue
		}
		r.Raising = append(r.Raising, v)
		if lvl > r.Floor {
			r.Floor = lvl
		}
	}
	sort.SliceStable(r.Raising, func(i, j int) bool {
		return r.Raising[i].Level > r.Raising[j].Level
	})
	return r
}

// DivergentScopes returns the raising commits whose scope names something the
// derived surface does not agree with, in either direction.
//
// The scope string is the trap release.md records twice: a `feat(metrics)` that
// touched only the usage tooling was listed as a product feature in v1.3.0's
// notes, and a `feat(probe)` in the v1.3.0..v1.4.0 window edits shipped files.
// Neither is discoverable from the subject, so the surface decides and the
// disagreement is printed.
func DivergentScopes(r Result, shippingScopes map[string]bool) []Verdict {
	var out []Verdict
	for _, v := range r.Withheld {
		for _, s := range v.Commit.Scopes {
			if shippingScopes[s] {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

// ShippingScopes is the set of scopes carried by commits that do ship. It is
// derived from the window rather than declared, so it cannot rot; it exists
// only to name divergences, never to classify.
func ShippingScopes(r Result) map[string]bool {
	m := map[string]bool{}
	for _, v := range r.Raising {
		for _, s := range v.Commit.Scopes {
			m[s] = true
		}
	}
	return m
}
