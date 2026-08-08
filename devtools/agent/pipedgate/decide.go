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
	// BaseRef is the branch the repo-state checks compare against; defaults to
	// defaultBaseRef.
	BaseRef string `json:"base_ref"`
	// OverlapIgnore are paths a file-overlap check discounts, because a
	// concurrent edit to them is expected and mechanically resolved.
	OverlapIgnore []string `json:"overlap_ignore"`
}

const defaultBaseRef = "origin/main"

// refPattern is what a base_ref may look like. The registry is repo-local and
// as trusted as the hook itself, but the value reaches `git diff` as an
// argument: a leading `-` would be read as an option rather than a ref, and an
// empty one as the working tree. Anything else falls back to defaultBaseRef.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// compiled holds a Registry with its patterns compiled once.
type compiled struct {
	gates         []*regexp.Regexp
	exempt        []*regexp.Regexp
	baseRef       string
	overlapIgnore map[string]bool
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
	base := r.BaseRef
	if !refPattern.MatchString(base) {
		base = defaultBaseRef
	}
	ignore := make(map[string]bool, len(r.OverlapIgnore))
	for _, p := range r.OverlapIgnore {
		ignore[p] = true
	}
	return &compiled{
		gates:         compileAll(r.Gates),
		exempt:        compileAll(r.Exempt),
		baseRef:       base,
		overlapIgnore: ignore,
	}, errs
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// isGate reports whether c is a registered gate whose exit status is the
// answer: registered, not exempted, and not a capability probe. head is the
// call's head text, which every caller already has.
func (reg *compiled) isGate(c *syntax.CallExpr, head string) bool {
	return head != "" && !matchAny(reg.exempt, head) && matchAny(reg.gates, head) && !isProbe(c)
}

// probeFlags make an invocation a capability probe rather than a run: the tool
// prints and exits without producing a result, so there is no exit status to
// lose and nothing for the pipe to swallow (Q730). Structural rather than a
// registry entry, because the shape holds for every gate — a per-tool exempt
// pattern would fix the tool it was reported against and leave the class.
//
// `-v` is deliberately absent: it is --version to make but verbose to
// `go test`, so exempting it would exempt `go test -v ./... | tail`, the bug
// this tool exists to catch. `make -v | head` stays denied, and `--version` is
// the way out of it.
var probeFlags = map[string]bool{
	"--version": true,
	"--help":    true,
	"-V":        true,
	"-h":        true,
}

// isProbe reports whether any argument is a probe flag. Read from the parsed
// words rather than the joined head, so a flag spelled inside a quoted
// argument stays one word: `git commit -m "bump --version output"` is a commit
// and still a gate.
func isProbe(c *syntax.CallExpr) bool {
	if c == nil || len(c.Args) == 0 {
		return false
	}
	for _, a := range c.Args[1:] {
		if probeFlags[literal(a)] {
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

// overrideVar is the break-glass, spelled as a command-position assignment
// because that is the form a PreToolUse hook can see: it reads the command
// string, and the session cannot set a variable in the hook's own environment.
// Mirrors prod-guard's PROD_GUARD_OVERRIDE.
const overrideVar = "PIPED_GATE_OVERRIDE"

const (
	// overrideHint closes every reason. The deny reaches the model rather than
	// the user, so it must carry both ways out: the prefix for a case the rule
	// reads wrong, and the Queue for a rule that is wrong often enough to fix.
	overrideHint = " If the status genuinely does not matter here, re-run prefixed with " +
		overrideVar + "=<reason>. If this call is not the mistake the rule describes, that is a " +
		"defect in the rule: file a Queue row rather than overriding it every time. "

	pipestatusReason = "This reads $PIPESTATUS, which does not exist in zsh — the shell the Bash tool runs. " +
		"It expands to empty, so the test against it reads as success whatever the pipeline did. " +
		"zsh spells it $pipestatus (lowercase, 1-indexed); better still, redirect and read the status " +
		`directly: cmd > tmp/out.log 2>&1; echo "EXIT=$?".` + overrideHint +
		"See docs/development/testing.md#the-status-you-report-is-a-claim-too."

	pipedReasonSuffix = " is piped into a filter, so this call's exit status is the filter's, not the gate's — " +
		"a failure reads exactly like a pass, and zsh (the shell the Bash tool runs) has no PIPESTATUS to " +
		"recover it. Redirect instead, then reconcile status against output: " +
		`cmd > tmp/out.log 2>&1; echo "EXIT=$?"; grep -E 'FAILED|Error [0-9]|^make:' tmp/out.log.` + overrideHint +
		"See docs/development/testing.md#the-status-you-report-is-a-claim-too."

	lostStatusReasonSuffix = " runs in the background, but this call's exit status is its LAST statement's — " +
		"an echo exits 0 whatever the gate did, so the task notification reports success for a failed gate. " +
		"Capture the status and re-raise it: " +
		`cmd > tmp/out.log 2>&1; rc=$?; echo "EXIT=$rc"; exit $rc.` + overrideHint +
		"See docs/development/testing.md#the-status-you-report-is-a-claim-too."
)

// Decide returns the permissionDecisionReason for a Bash command, or "" to stay
// silent. background is the payload's run_in_background, and repo answers the
// repo-state checks (nil disables them). Every failure path returns "": a hook
// that cannot parse a command has nothing to say about it.
func Decide(cmd string, background bool, reg *compiled, repo Repo) string {
	if defersToThrottleHook(cmd) {
		return ""
	}

	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		// A zsh-ism bash cannot parse, or a truncated string. Staying silent is
		// the contract; the alternative is guessing from a half-parse.
		return ""
	}

	if hasOverride(f) {
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
	// each stage's status. Neither mitigates a lost background status, so the
	// suppression is scoped to the pipe verdict.
	if !pipefail && !readsLower {
		for _, st := range lhs {
			src := statusSource(st)
			head := peelWrappers(headText(src))
			if reg.isGate(src, head) {
				return "`" + truncate(head, 70) + "`" + pipedReasonSuffix
			}
		}
	}
	if reason := lostBackgroundStatus(f, background, reg); reason != "" {
		return reason
	}
	// Last, because it is the only check that costs a subprocess: the status
	// verdicts are decided from the parse tree alone.
	return repoStateWarning(f, reg, repo)
}

// hasOverride reports whether the command carries the break-glass assignment
// with a reason attached. An empty value does not count: the point of the
// prefix is that the caller has to say why, and a bare
// `PIPED_GATE_OVERRIDE= make check | tail` would be the switch-it-off form.
//
// Read from the parse tree, so the same Q624 asymmetry as everything else here:
// an assignment is a real assignment, while the name quoted in a commit message
// or a grep pattern is a word and disables nothing.
func hasOverride(f *syntax.File) bool {
	var found bool
	syntax.Walk(f, func(node syntax.Node) bool {
		if found {
			return false
		}
		a, ok := node.(*syntax.Assign)
		if !ok {
			return true
		}
		if a.Name != nil && a.Name.Value == overrideVar && a.Value != nil && literal(a.Value) != "" {
			found = true
		}
		return true
	})
	return found
}

// lostBackgroundStatus returns the warning for a gate whose status never
// reaches the caller because the call is backgrounded and something else runs
// last (Q681). A `;`-list yields its last statement's status, so a backgrounded
// `make check > log 2>&1; echo "EXIT=$?"` notifies exit 0 for a failed gate —
// the same false green as the pipe, arriving by a different route. `&` on the
// last statement is the other spelling of the same mistake.
//
// Foreground calls are silent by construction: there the trailing echo prints
// the real status where it can be read, which is the documented form.
func lostBackgroundStatus(f *syntax.File, background bool, reg *compiled) string {
	if len(f.Stmts) == 0 {
		return ""
	}
	last := f.Stmts[len(f.Stmts)-1]
	if !background && !last.Background {
		return ""
	}
	if carriesStatus(last, reg) {
		return ""
	}
	gate := firstGate(f, reg)
	if gate == "" {
		return ""
	}
	return "`" + truncate(gate, 70) + "`" + lostStatusReasonSuffix
}

// carriesStatus reports whether a failing gate could still surface as st's own
// exit status. Anything it cannot reason about counts as carrying, so an
// unfamiliar shape gets silence rather than a guess.
func carriesStatus(st *syntax.Stmt, reg *compiled) bool {
	if st == nil {
		return true
	}
	if st.Background {
		return false // the status is the fork's, never the job's
	}
	switch c := st.Cmd.(type) {
	case *syntax.CallExpr:
		if len(c.Args) == 0 {
			return true // a bare assignment yields its own substitution's status
		}
		// `exit $rc` is the fix, and a literal `exit 0` is a deliberate discard.
		if literal(c.Args[0]) == "exit" {
			return true
		}
		return reg.isGate(c, peelWrappers(headText(c)))
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.AndStmt:
			// Either side can be the last to run, and a failing left side ends
			// the chain with its own status.
			return carriesStatus(c.X, reg) || carriesStatus(c.Y, reg)
		case syntax.OrStmt, syntax.Pipe, syntax.PipeAll:
			// `a || b` yields 0 whenever a succeeds, and a pipeline yields its
			// last stage's: only the right side can carry a failure out.
			return carriesStatus(c.Y, reg)
		}
	case *syntax.Subshell:
		return lastCarries(c.Stmts, reg)
	case *syntax.Block:
		return lastCarries(c.Stmts, reg)
	}
	return true
}

func lastCarries(stmts []*syntax.Stmt, reg *compiled) bool {
	if len(stmts) == 0 {
		return true
	}
	return carriesStatus(stmts[len(stmts)-1], reg)
}

// firstGate returns the head of the first registered gate at command position
// anywhere in the tree, or "".
func firstGate(f *syntax.File, reg *compiled) string {
	return findCall(f, reg.isGate)
}

// findCall returns the head of the first call at command position anywhere in
// the tree that satisfies match, or "". Walking the tree rather than the raw
// string is what keeps a command merely NAMED in a commit message or a grep
// pattern from counting: quoted text parses as a word, never a call.
func findCall(f *syntax.File, match func(c *syntax.CallExpr, head string) bool) string {
	var found string
	syntax.Walk(f, func(node syntax.Node) bool {
		if found != "" {
			return false
		}
		c, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		head := peelWrappers(headText(c))
		if head != "" && match(c, head) {
			found = head
		}
		return true
	})
	return found
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
