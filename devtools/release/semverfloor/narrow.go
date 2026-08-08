package main

import (
	"go/scanner"
	"go/token"
	"os/exec"
	"regexp"
	"strings"
)

// Narrower re-reads the files a commit put on the released surface and returns
// the subset whose change can alter a built artifact. Returning the input
// unchanged narrows nothing, which is what Classify does with a nil Narrower.
//
// The path is where attribution starts, not where it ends: a commit that edits
// only a godoc line inside a released package directory ships a byte-identical
// binary. Reading the diff is what separates the two.
type Narrower interface {
	Substantive(c Commit, shipped []string) []string
}

// gitNarrower answers Narrower from the repository, comparing each shipped
// file's Go token stream on both sides of the commit.
//
// Every case it cannot read for certain is kept. The floor is a floor, and only
// one direction of error is dangerous: a dropped commit under-reports, so a
// minor ships as a patch and breaks a consumer's constraint, while a kept one
// costs a wrong name in a report a human is reading anyway. So a non-Go file, a
// file that does not scan, an added or deleted file, and a failed git read all
// count as substantive.
type gitNarrower struct{ root string }

// Substantive returns the shipped files whose change is not confined to
// comments and whitespace.
func (g gitNarrower) Substantive(c Commit, shipped []string) []string {
	var out []string
	for _, f := range shipped {
		if !commentOnly(g.root, c.SHA, f) {
			out = append(out, f)
		}
	}
	return out
}

// commentOnly reports whether this commit's change to one file cannot reach the
// built artifact. False whenever the answer is not certain.
func commentOnly(root, sha, file string) bool {
	if !strings.HasSuffix(file, ".go") {
		// Charts are the other shipped kind, and their `#` comments are not
		// separable this cheaply: a `#` line inside a template block scalar or a
		// Go-template action is content, not a comment.
		return false
	}
	before, ok := gitShow(root, sha+"^:"+file)
	if !ok {
		return false // added, renamed, or a root commit
	}
	after, ok := gitShow(root, sha+":"+file)
	if !ok {
		return false // deleted
	}
	bs, ok := goShape(before)
	if !ok {
		return false
	}
	as, ok := goShape(after)
	if !ok {
		return false
	}
	return bs == as
}

func gitShow(root, spec string) ([]byte, bool) {
	//nolint:gosec // G204: a fixed git verb plus a rev:path this program read out of git itself.
	cmd := exec.Command("git", "show", spec)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

// directiveRE matches a comment the toolchain reads as an instruction. These are
// comments to the scanner and code to the compiler — `//go:build` decides
// whether the file compiles at all, `//go:embed` puts a file in the binary — so
// they stay in the shape a file is compared by.
var directiveRE = regexp.MustCompile(`^//(?:go:|line |extern |export |sys |cgo_)|^// \+build `)

// goShape renders the part of a Go file that can reach a built binary: the token
// stream the compiler sees, with directive comments left in place, since where a
// directive sits is as load-bearing as what it says. Ordinary comments and all
// whitespace are absent, so two versions of a file with the same shape differ
// only in what cannot change behaviour.
//
// It reports false for source that does not scan cleanly. A shape read through
// scan errors is not evidence of anything, and the caller treats that as a
// change.
func goShape(src []byte) (string, bool) {
	fset := token.NewFileSet()
	f := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	clean := true
	s.Init(f, src, func(token.Position, string) { clean = false }, scanner.ScanComments)

	var b strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT && !directiveRE.MatchString(lit) {
			continue
		}
		b.WriteString(tok.String())
		b.WriteByte(0x1f)
		b.WriteString(lit)
		b.WriteByte(0x1e)
	}
	if !clean {
		return "", false
	}
	return b.String(), true
}
