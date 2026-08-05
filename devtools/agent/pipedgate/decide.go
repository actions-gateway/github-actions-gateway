package main

import (
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Registry is the editable gate list, .claude/piped-gate-guard.json. Patterns
// are matched against the HEAD of a command — its name and arguments with any
// `VAR=val` assignments and wrapper words removed — never against the raw
// command string, which is what makes them fire on the invocation and not on a
// `git show` or a commit message that names it.
type Registry struct {
	Gates  []string `json:"gates"`
	Exempt []string `json:"exempt"`
}

// compiled holds a Registry with its patterns compiled once.
type compiled struct {
	gates  []*regexp.Regexp
	exempt []*regexp.Regexp
}

// compile builds the matchers. A pattern that does not compile is dropped
// rather than fatal: this runs on every Bash call, so a bad edit to the
// registry must degrade the warning, never break the tool. compileErrs reports
// the rejects so the test suite can fail on them.
func (r Registry) compile() (*compiled, []string) {
	var errs []string
	compileAll := func(pats []string) []*regexp.Regexp {
		out := make([]*regexp.Regexp, 0, len(pats))
		for _, p := range pats {
			re, err := regexp.Compile(p)
			if err != nil {
				errs = append(errs, p)
				continue
			}
			out = append(out, re)
		}
		return out
	}
	return &compiled{gates: compileAll(r.Gates), exempt: compileAll(r.Exempt)}, errs
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// heavyGo matches a `go build`/`go test` invocation anywhere in a command. The
// leading boundary keeps `cargo test` and `django test` from matching.
var heavyGo = regexp.MustCompile(`(^|[^[:alnum:]_-])go[[:space:]]+(build|test)([[:space:]]|$)`)

// defersToThrottleHook reports whether claude-go-throttle-hook.sh will answer
// this call by rewriting it. Only one hook may answer a Bash call, and losing
// that rewrite would leave an unthrottled `-race` run free to freeze the GUI
// (Q92) — worse than a missed warning, so this tool stands down.
//
// Deliberately a string test rather than an AST one: it mirrors the sibling
// hook's own predicate, and the two must agree about which calls it claims.
func defersToThrottleHook(cmd string) bool {
	if !strings.Contains(cmd, "-race") || !heavyGo.MatchString(cmd) {
		return false
	}
	switch {
	case strings.Contains(cmd, "local-throttle.sh"),
		strings.Contains(cmd, "taskpolicy "),
		strings.Contains(cmd, "nice -n"):
		return false // already throttled; the sibling hook leaves it alone
	}
	return true
}

// wrappers precede a real command word without changing whose status is at
// stake.
var wrappers = map[string]bool{
	"time": true, "sudo": true, "nohup": true, "command": true,
	"exec": true, "bash": true, "sh": true, "zsh": true,
}

const (
	pipestatusReason = "This reads $PIPESTATUS, which does not exist in zsh — the shell the Bash tool runs. " +
		"It expands to empty, so the test against it reads as success whatever the pipeline did. " +
		"zsh spells it $pipestatus (lowercase, 1-indexed); better still, redirect and read the status " +
		`directly: cmd > tmp/out.log 2>&1; echo "EXIT=$?". ` +
		"See docs/development/testing.md#the-status-you-report-is-a-claim-too."

	pipedReasonSuffix = " is piped into a filter, so this call's exit status is the filter's, not the gate's — " +
		"a failure reads exactly like a pass, and zsh (the shell the Bash tool runs) has no PIPESTATUS to " +
		"recover it. Redirect instead, then reconcile status against output: " +
		`cmd > tmp/out.log 2>&1; echo "EXIT=$?"; grep -E 'FAILED|Error [0-9]|^make:' tmp/out.log. ` +
		"Continue only if you want the output and not the status. " +
		"See docs/development/testing.md#the-status-you-report-is-a-claim-too."
)

// Decide returns the permissionDecisionReason for a Bash command, or "" to stay
// silent. Every failure path returns "": a hook that cannot parse a command has
// nothing to say about it.
func Decide(cmd string, reg *compiled) string {
	if defersToThrottleHook(cmd) {
		return ""
	}

	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		// A zsh-ism bash cannot parse, or a truncated string. Staying silent is
		// the contract; the alternative is guessing from a half-parse.
		return ""
	}

	var (
		readsUpper bool // $PIPESTATUS — bash-only, empty in zsh
		readsLower bool // $pipestatus — the correct zsh spelling
		pipefail   bool
		lhs        []*syntax.Stmt
	)
	syntax.Walk(f, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.ParamExp:
			if n.Param != nil {
				switch n.Param.Value {
				case "PIPESTATUS":
					readsUpper = true
				case "pipestatus":
					readsLower = true
				}
			}
		case *syntax.CallExpr:
			if setsPipefail(n) {
				pipefail = true
			}
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				lhs = append(lhs, n.X)
			}
		}
		return true
	})

	if readsUpper {
		return pipestatusReason
	}
	// Mitigated: pipefail propagates the failure, and zsh's $pipestatus recovers
	// each stage's status.
	if pipefail || readsLower {
		return ""
	}

	for _, st := range lhs {
		head := peelWrappers(headText(statusSource(st)))
		if head == "" || matchAny(reg.exempt, head) {
			continue
		}
		if matchAny(reg.gates, head) {
			return "`" + truncate(head, 70) + "`" + pipedReasonSuffix
		}
	}
	return ""
}

// setsPipefail reports whether a call is `set -o pipefail` (in any of its
// spellings, including a combined `set -euo pipefail`).
func setsPipefail(c *syntax.CallExpr) bool {
	if len(c.Args) < 2 || literal(c.Args[0]) != "set" {
		return false
	}
	for _, a := range c.Args[1:] {
		if literal(a) == "pipefail" {
			return true
		}
	}
	return false
}

// statusSource returns the call whose exit status a statement yields, seeing
// through a subshell, a brace group, and an `&&`/`||` chain: in
// `(cd x && go test ./...) | tail` the pipe consumes `go test`'s status.
func statusSource(st *syntax.Stmt) *syntax.CallExpr {
	if st == nil {
		return nil
	}
	switch c := st.Cmd.(type) {
	case *syntax.CallExpr:
		return c
	case *syntax.Subshell:
		return lastSource(c.Stmts)
	case *syntax.Block:
		return lastSource(c.Stmts)
	case *syntax.BinaryCmd:
		// `&&`/`||` yield the last command that ran; a nested pipeline yields
		// its right-hand side. Either way the status comes from the right.
		return statusSource(c.Y)
	}
	return nil
}

func lastSource(stmts []*syntax.Stmt) *syntax.CallExpr {
	if len(stmts) == 0 {
		return nil
	}
	return statusSource(stmts[len(stmts)-1])
}

// headText renders a call's name and arguments for registry matching.
// Assignments are excluded because the parser keeps them separate, so
// `GOFLAGS=-mod=mod go build ./...` matches a plain `^go build` pattern.
func headText(c *syntax.CallExpr) string {
	if c == nil || len(c.Args) == 0 {
		return ""
	}
	words := make([]string, 0, len(c.Args))
	for _, a := range c.Args {
		words = append(words, literal(a))
	}
	return strings.Join(words, " ")
}

// literal renders a word as its source text. Quotes are dropped, which is what
// registry matching wants: `"make" check` and `make check` are one command.
func literal(w *syntax.Word) string {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, dp := range p.Parts {
				if lit, ok := dp.(*syntax.Lit); ok {
					b.WriteString(lit.Value)
				}
			}
		}
	}
	return b.String()
}

func peelWrappers(head string) string {
	for {
		name, rest, found := strings.Cut(head, " ")
		if !found || !wrappers[name] {
			return head
		}
		head = strings.TrimLeft(rest, " ")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
