package main

import (
	"regexp"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Permission decisions, and the reasons shown with them. The text is addressed
// to whoever reads the prompt: an ask reaches the user, a deny reaches the
// model.
const (
	allow = "allow"
	ask   = "ask"
	deny  = "deny"

	allowReason = "Auto-throttled heavy go build/test (I/O and CPU demoted below the desktop) " +
		"to keep the local GUI responsive — see CLAUDE.md."

	askReason = "Auto-throttled a heavy `go ... -race` (I/O and CPU demoted below the desktop) — " +
		"confirm to run the throttled form. An unthrottled -race run can saturate the machine and " +
		"freeze the local GUI; see CLAUDE.md."

	denyReason = "Blocked: this `go ... -race` has more than one go build/test invocation, so the " +
		"hook cannot insert the throttle prefix unambiguously. Give each `go ... -race` its own " +
		"throttle prefix ($(scripts/agent/local-throttle.sh prefix)), run each go line on its own, " +
		"or use the matching `make` target (it throttles itself). See CLAUDE.md."
)

// Decision is the hook's verdict. Command is the rewritten command an allow or
// an ask carries; a deny leaves it empty. A nil *Decision is silence, which
// Claude Code reads as "no opinion".
type Decision struct {
	Permission string
	Reason     string
	Command    string
}

// invocation is one command-position `go build`/`go test` in the command.
type invocation struct {
	// offset is the byte offset of the `go` token — where the prefix goes.
	offset int
	// args is the invocation's own word text. -race is read from here rather
	// than from the whole command, so a -race in a commit message is not a race
	// run.
	args string
	// wrapped reports that `go` was reached by peeling a wrapper (Q696). Such a
	// call is throttled but never auto-allowed: `taskpolicy … timeout … go test`
	// is not the bare shape the repo's `Bash(go test *)` allowlist trusts.
	wrapped bool
}

// Decide returns the verdict for a Bash command, or nil to stay silent. prefix
// resolves the platform throttle prefix; it is called only once an invocation
// is found, because it costs a subprocess and this runs on every Bash call.
//
// Every failure path returns nil. A hook that cannot parse a command has
// nothing to say about it, and one that fires on every Bash call must never be
// the reason one fails.
func Decide(cmd string, prefix func() string) *Decision {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		// A zsh-ism bash cannot parse, or a truncated string. Silence is the
		// contract; the alternative is guessing from a half-parse.
		trace("silent: parsing a %d-byte command: %v", len(cmd), err)
		return nil
	}

	invs := findInvocations(f)
	if len(invs) == 0 {
		trace("silent: no command-position go build/test")
		return nil
	}
	first := invs[0]

	// Read from the text preceding the first invocation, so a commit message
	// naming taskpolicy cannot suppress a real throttle.
	if alreadyThrottled(cmd[:first.offset]) {
		trace("silent: already throttled")
		return nil
	}

	var race, wrapped bool
	for _, inv := range invs {
		race = race || strings.Contains(inv.args, "-race")
		wrapped = wrapped || inv.wrapped
	}

	// An empty prefix means throttling is off (CI, headless, SSH, unsupported
	// OS), so there is nothing to apply. It also means a probe that could not
	// run, which throttlePrefix traces apart: this is the one silent path a
	// contended machine can reach mid-suite, and so the first thing a Q703
	// occurrence has to rule in or out.
	p := prefix()
	if p == "" {
		trace("silent: no throttle prefix")
		return nil
	}

	if !simple(f) || wrapped {
		// Only a bare invocation can be auto-allowed: a compound command's other
		// segments, or a redirect to an outside-workspace path, would ride past
		// the permission system and the guard hooks on an allow. The throttle
		// itself must still reach the dangerous form, so a -race here is
		// rewritten and asked — the prompt keeps the user and the guards in the
		// loop. A non-race form stays on the normal flow untouched.
		if !race {
			trace("silent: compound or wrapped, and carries no -race")
			return nil
		}
		if len(invs) > 1 {
			// One prefix to place and more than one invocation to throttle.
			// Denying beats emitting a command that throttles one and leaves the
			// other running at full tilt.
			return &Decision{Permission: deny, Reason: denyReason}
		}
		return &Decision{Permission: ask, Reason: askReason, Command: rewriteAt(cmd, first.offset, p)}
	}

	return &Decision{Permission: allow, Reason: allowReason, Command: rewriteAt(cmd, first.offset, p)}
}

// findInvocations returns every command-position `go build`/`go test` in the
// tree, in source order.
//
// Walking the parse tree rather than scanning the string is what keeps a `go`
// that is merely NAMED from counting (Q624): text inside quotes or a heredoc
// body parses as a word, never a call, so a `git commit` message quoting
// `go test -race` reaches no CallExpr at all. The one place the parser is
// stricter than a scanner is an UNQUOTED heredoc, whose `$(go test …)` really
// does run and really is a call.
func findInvocations(f *syntax.File) []invocation {
	var out []invocation
	syntax.Walk(f, func(node syntax.Node) bool {
		c, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		if inv, ok := goInvocation(c); ok {
			out = append(out, inv)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].offset < out[j].offset })
	return out
}

// goInvocation reports whether a call is a `go build`/`go test`, peeling any
// wrapper words in front of it.
func goInvocation(c *syntax.CallExpr) (invocation, bool) {
	words, wrapped := peelWrappers(c.Args)
	if len(words) < 2 || literal(words[0]) != "go" {
		return invocation{}, false
	}
	switch literal(words[1]) {
	case "build", "test":
	default:
		return invocation{}, false
	}
	texts := make([]string, 0, len(words))
	for _, w := range words {
		texts = append(texts, literal(w))
	}
	return invocation{
		offset:  int(words[0].Pos().Offset()),
		args:    strings.Join(texts, " "),
		wrapped: wrapped,
	}, true
}

// wrapperSpec describes how to skip a wrapper's own words to reach the command
// it runs. flagValues are the options taking a separate argument; operands is
// how many non-option operands precede the command (timeout's DURATION is the
// only one); assigns marks a wrapper that takes leading NAME=VALUE words.
type wrapperSpec struct {
	flagValues map[string]bool
	operands   int
	assigns    bool
}

// wrappers exec their trailing command, so a `go … -race` behind one runs
// unthrottled unless it is peeled (Q696).
//
// An allowlist rather than a heuristic: a name absent here, or an option absent
// from its spec, stops the peel and leaves the call unclaimed — silence, the
// same outcome as not looking. That is the safe direction, because a misread
// peel would insert the prefix at the wrong offset and emit a command nobody
// wrote. Throttling wrappers (taskpolicy, nice) are deliberately absent;
// alreadyThrottled answers those. `time` needs no entry: bash parses it as a
// keyword, so the call it times is walked on its own.
var wrappers = map[string]wrapperSpec{
	"timeout": {
		flagValues: map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true},
		operands:   1,
	},
	"env":     {flagValues: map[string]bool{"-u": true, "--unset": true}, assigns: true},
	"stdbuf":  {flagValues: map[string]bool{"-i": true, "-o": true, "-e": true, "--input": true, "--output": true, "--error": true}},
	"nohup":   {},
	"command": {},
	"exec":    {},
}

// peelWrappers strips leading wrapper words, reporting whether it stripped any.
// Wrappers nest, so this loops.
func peelWrappers(args []*syntax.Word) ([]*syntax.Word, bool) {
	var wrapped bool
	for len(args) > 0 {
		spec, ok := wrappers[literal(args[0])]
		if !ok {
			return args, wrapped
		}
		rest, ok := spec.skip(args[1:])
		if !ok {
			return nil, false
		}
		args, wrapped = rest, true
	}
	return args, wrapped
}

// skip consumes a wrapper's own options and operands, reporting false on
// anything the spec does not describe: an unknown option, an attached `-oL` or
// `--signal=KILL` form, a truncated one.
func (w wrapperSpec) skip(args []*syntax.Word) ([]*syntax.Word, bool) {
	for len(args) > 0 {
		word := literal(args[0])
		switch {
		case word == "--":
			return w.skipOperands(args[1:])
		case word != "-" && strings.HasPrefix(word, "-"):
			if !w.flagValues[word] || len(args) < 2 {
				return nil, false
			}
			args = args[2:]
		case w.assigns && assignWord.MatchString(word):
			args = args[1:]
		default:
			return w.skipOperands(args)
		}
	}
	return nil, false
}

func (w wrapperSpec) skipOperands(args []*syntax.Word) ([]*syntax.Word, bool) {
	if len(args) < w.operands {
		return nil, false
	}
	return args[w.operands:], true
}

// assignWord is a NAME=VALUE word, which `env` takes before its command.
var assignWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// simple reports whether the command is a single unadorned call — the only
// shape that can be auto-allowed, because it holds nothing another hook would
// want to see: no second segment, no redirect whose target could sit outside
// the workspace, no substitution that runs a command of its own.
func simple(f *syntax.File) bool {
	if len(f.Stmts) != 1 {
		return false
	}
	st := f.Stmts[0]
	if st.Background || st.Negated || len(st.Redirs) > 0 {
		return false
	}
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok {
		return false
	}
	var expands bool
	syntax.Walk(call, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			expands = true
		}
		return !expands
	})
	return !expands
}

// alreadyThrottled reports whether the text before an invocation already
// carries a throttle prefix, or computes one via local-throttle.sh — the
// documented manual workaround, or a previous wrap. Re-wrapping an already
// demoted command would stack two prefixes.
func alreadyThrottled(pre string) bool {
	return strings.Contains(pre, "local-throttle.sh") ||
		strings.Contains(pre, "taskpolicy ") ||
		strings.Contains(pre, "nice -n")
}

// rewriteAt inserts the prefix immediately before the `go` token, preserving
// everything around it — a subshell wrapper, a leading `cd`, redirects,
// `VAR=val` assignments, and any wrapper words the peel stepped over.
func rewriteAt(cmd string, offset int, prefix string) string {
	return cmd[:offset] + prefix + " " + cmd[offset:]
}

// literal renders a word as its source text, dropping quotes: `"go" test` and
// `go test` are one command.
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
