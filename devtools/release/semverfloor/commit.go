package main

import (
	"regexp"
	"strings"
)

// Commit is one commit in the window: its Conventional Commit subject parsed
// into fields, and the paths it changed.
type Commit struct {
	SHA      string
	Subject  string
	Type     string   // feat, fix, …; empty when the subject is not conventional
	Scopes   []string // the scope list, split on commas; nil when absent
	Breaking bool     // `!` before the colon, or a BREAKING CHANGE footer
	Files    []string
}

// subjectRE matches a Conventional Commits 1.0.0 prefix: `type(scope)!: `.
// The scope is captured raw because a compound scope — `fix(agc,gmc)` — is one
// parenthesised group holding a comma-separated list.
var subjectRE = regexp.MustCompile(`^([a-z]+)(?:\(([^)]*)\))?(!)?: `)

// breakingFooterRE matches the footer form, which the spec allows as an
// alternative to `!`. Both spellings are normative; `-` is the variant a commit
// template produces when the space would break a trailer parser.
var breakingFooterRE = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE:`)

// parseSubject splits a Conventional Commit subject. ok is false for a subject
// that does not carry the prefix at all — a merge commit, or a hand-written
// subject — which is reported rather than guessed at, since a commit whose type
// cannot be read is a commit whose semver weight cannot be read either.
func parseSubject(subject string) (typ string, scopes []string, breaking, ok bool) {
	m := subjectRE.FindStringSubmatch(subject)
	if m == nil {
		return "", nil, false, false
	}
	if m[2] != "" {
		for _, s := range strings.Split(m[2], ",") {
			if s = strings.TrimSpace(s); s != "" {
				scopes = append(scopes, s)
			}
		}
	}
	return m[1], scopes, m[3] == "!", true
}

// hasBreakingFooter reports whether a commit body carries the footer form.
func hasBreakingFooter(body string) bool {
	return breakingFooterRE.MatchString(body)
}
