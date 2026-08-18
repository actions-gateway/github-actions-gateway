package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shippedRegistry loads .claude/piped-gate-guard.json — the file the hook reads
// at runtime, not a copy. A registry edit that broke a pattern would otherwise
// pass a suite asserting its own fixture. testdata/piped-gate-guard.json is an
// in-module symlink to it: reached directly as ../../../.claude/… the read
// leaves the devtools module root, and go drops those from the test-cache key,
// so a broken registry replayed a cached green — the fixture problem this
// comment rejects, arriving by another route (Q895,
// testing.md § The out-of-module test read gate).
func shippedRegistry(t *testing.T) *compiled {
	t.Helper()
	path := filepath.Join("testdata", "piped-gate-guard.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped registry: %v", err)
	}
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("parse shipped registry: %v", err)
	}
	c, errs := reg.compile()
	if len(errs) > 0 {
		t.Fatalf("registry patterns do not compile: %v", errs)
	}
	if len(c.gates) == 0 {
		t.Fatal("registry lists no gates")
	}
	return c
}

// Both directions are asserted because both fail silently. A rule that stops
// matching lets the original bug back in: a failing gate piped into `tail`
// reports success and reads exactly like a real green. A rule that matches too
// much turns every `git show`, `grep`, and commit message that merely NAMES a
// gate into a permission prompt — and this runs on every Bash call.
func TestDecide(t *testing.T) {
	reg := shippedRegistry(t)

	cases := []struct {
		name string
		cmd  string
		// bg is the payload's run_in_background.
		bg   bool
		warn bool
		// substr, when set, must appear in the reason.
		substr string
	}{
		// --- A gate whose status the pipe swallows -----------------------------
		{name: "plain pipe to tail", cmd: "make check | tail -30", warn: true, substr: "exit status is the filter's"},
		{name: "the canonical false green", cmd: `make check 2>&1 | tail -30; echo "EXIT=$?"`, warn: true},
		// The recurrence that reopened Q625: a failed pull reported EXIT=0.
		{name: "git pull piped", cmd: `git pull --ff-only 2>&1 | tail -5; echo "EXIT=$?"`, warn: true},
		{name: "git push piped", cmd: "git push -u origin HEAD 2>&1 | tail -3", warn: true},
		{name: "make -C piped to grep", cmd: `make -C cmd/agc test-integration | grep -E "FAIL|ok"`, warn: true},
		{name: "go test piped", cmd: "go test ./... | tail -20", warn: true},
		{name: "scripts gate piped", cmd: "scripts/ci/check-tools.sh | head -20", warn: true},
		{name: "bash-wrapped scripts gate", cmd: `bash scripts/docs/lint-backlog.sh | grep -v "^ok"`, warn: true},
		{name: "tee loses the status too", cmd: "make check | tee tmp/check.log", warn: true},
		{name: "inside a command substitution", cmd: "out=$(make check | tail -1)", warn: true},
		{name: "subshell group piped", cmd: "(cd cmd/agc && go test ./...) | tail -5", warn: true},
		{name: "brace group piped", cmd: "{ cd cmd/agc && go test ./...; } | tail -5", warn: true},
		{name: "after an unrelated leading segment", cmd: "mkdir -p tmp; make check | grep FAIL", warn: true},
		{name: "env-prefixed gate", cmd: "GOFLAGS=-mod=mod go build ./... | tail -5", warn: true},
		{name: "second stage of a three-stage pipeline", cmd: "cat x | make check | tail", warn: true},
		{name: "|& pipes stderr too", cmd: "make check |& tail -5", warn: true},
		// `make test-race` carries "-race" but no `go build`/`go test` token, so
		// the throttle hook never claims it and this one must still warn.
		{name: "make test-race still warns", cmd: "make test-race 2>&1 | tail -40", warn: true},

		// --- PIPESTATUS does not exist in zsh ----------------------------------
		{name: "PIPESTATUS[0] after a gate", cmd: `make check 2>&1 | tail -5; echo "EXIT=${PIPESTATUS[0]}"`, warn: true, substr: "does not exist in zsh"},
		{name: "bare $PIPESTATUS, no gate", cmd: "ls -l | wc -l; echo $PIPESTATUS", warn: true, substr: "does not exist in zsh"},

		// --- The correct forms -------------------------------------------------
		{name: "redirect then echo $?", cmd: `make check > tmp/check.log 2>&1; echo "EXIT=$?"`},
		{name: "redirect then grep the FILE", cmd: `make check > tmp/check.log 2>&1; echo "EXIT=$?"; grep -E "FAILED" tmp/check.log`},
		{name: "pipefail propagates", cmd: "set -o pipefail; make check | tail -30"},
		{name: "set -euo pipefail counts", cmd: "set -euo pipefail; make check 2>&1 | tail -30"},
		{name: "zsh $pipestatus recovers it", cmd: `make check 2>&1 | tail -5; echo "EXIT=${pipestatus[1]}"`},
		{name: "no pipe at all", cmd: "make check"},
		{name: "gate on the RIGHT keeps its status", cmd: `printf "%s" "$msg" | git commit -F -`},

		// --- Commands that merely NAME a gate (the Q624 shape) -----------------
		{name: "git show of a file containing it", cmd: `git show origin/main:CLAUDE.md | grep -n "make check"`},
		{name: "commit message quoting the bug", cmd: `git commit -m "fix(ci): make check | tail was reporting EXIT=0"`},
		// A heredoc body is a word, never a command, so a piped gate quoted in
		// one is text however the delimiter is written. No special case in the
		// code does this; the parser does.
		{name: "commit message in a quoted heredoc body", cmd: "git commit -F - <<'EOF'\nfix(ci): stop doing make check | tail -30\nEOF"},
		{name: "commit message in an unquoted heredoc body", cmd: "git commit -F - <<EOF\nci: make check | tail lied\nEOF"},
		// A quoted delimiter makes the body literal, so $PIPESTATUS there is not
		// a read.
		{name: "PIPESTATUS inside a quoted heredoc is text", cmd: "git commit -F - <<'EOF'\nnote: ${PIPESTATUS[0]} is a bash-ism\nEOF"},
		// An UNquoted delimiter expands, so the same text really does read the
		// variable — and in zsh it expands to empty. Warning is correct here.
		{name: "PIPESTATUS inside an unquoted heredoc is a real read", cmd: "git commit -F - <<EOF\nnote: ${PIPESTATUS[0]} was empty\nEOF", warn: true, substr: "does not exist in zsh"},
		{name: "grep for the pattern in docs", cmd: `grep -rn "make check | tail" docs/`},
		{name: "single-quoted PIPESTATUS is text, not a read", cmd: `grep -rn '$PIPESTATUS' docs/`},
		{name: "echo of the offending form", cmd: `echo "never run: make check | tail"`},

		// --- The break-glass prefix (Q697) -------------------------------------
		{name: "override on a piped gate", cmd: "PIPED_GATE_OVERRIDE=want-the-output-only make check | tail -30"},
		{name: "override on a lost background status", cmd: `PIPED_GATE_OVERRIDE=log-only make check > tmp/c.log 2>&1; echo "EXIT=$?"`, bg: true},
		{name: "override on a PIPESTATUS read", cmd: "PIPED_GATE_OVERRIDE=demonstrating-the-bug echo $PIPESTATUS"},
		{name: "override as its own statement", cmd: "PIPED_GATE_OVERRIDE=scoped-to-this-call; make check | tail -5"},
		{name: "quoted override value", cmd: `PIPED_GATE_OVERRIDE="reading output, not status" make check | tail -5`},
		// An empty value is the switch-it-off form, so it buys nothing.
		{name: "empty override still denies", cmd: "PIPED_GATE_OVERRIDE= make check | tail -30", warn: true},
		// The Q624 shape: the name inside a string is a word, not an assignment.
		{name: "override named in a commit message", cmd: `git commit -m "docs: PIPED_GATE_OVERRIDE=x make check | tail is the escape"`},
		{name: "override quoted, gate really piped", cmd: `echo "PIPED_GATE_OVERRIDE=x" | make check | tail -5`, warn: true},
		{name: "a different variable is not the override", cmd: "PIPED_GATE=x make check | tail -30", warn: true},

		// --- Non-gate commands piped into filters ------------------------------
		{name: "git log", cmd: "git log --oneline | head -5"},
		{name: "git diff", cmd: "git diff origin/main | head -40"},
		{name: "gh pr list", cmd: "gh pr list | head -20"},
		{name: "cat a log", cmd: "cat tmp/check.log | tail -30"},
		{name: "kubectl get", cmd: "kubectl get pods -n gag | grep Running"},
		{name: "make help is informational", cmd: "make help | grep check"},
		{name: "make -n prints, not runs", cmd: "make -n check | head"},

		// --- Capability probes, not gate runs (Q730) ---------------------------
		// A --version/--help invocation prints and exits, so there is no gate
		// result for the pipe to swallow. Every registered gate is covered, not
		// just the shellcheck instance that was reported.
		{name: "shellcheck --version piped", cmd: "shellcheck --version | grep 0.11"},
		{name: "shellcheck -V piped", cmd: "shellcheck -V | head -1"},
		{name: "make --version piped", cmd: "make --version | head -1"},
		{name: "golangci-lint --version piped", cmd: "golangci-lint --version | cat"},
		{name: "go test --help piped", cmd: "go test --help | head"},
		{name: "go vet -h piped", cmd: "go vet -h | head"},
		{name: "git pull --help piped", cmd: "git pull --help | head"},
		{name: "scripts gate --help piped", cmd: "scripts/ci/check-tools.sh --help | head"},
		{name: "./scripts gate -h piped", cmd: "./scripts/ci/check-tools.sh -h | head"},
		{name: "backgrounded probe ending in echo", cmd: `shellcheck --version > tmp/v.log 2>&1; echo "EXIT=$?"`, bg: true},

		// The catch the guard exists for, kept beside the exemption: the same
		// tools doing real work still deny.
		{name: "shellcheck on a script still denies", cmd: "shellcheck scripts/agent/local-throttle.sh | tail", warn: true},
		{name: "go test -v is verbose, not a version probe", cmd: "go test -v ./... | tail -20", warn: true},
		// `-v` is --version to make and verbose to `go test`. Exempting it would
		// exempt the case above, so the short form stays denied.
		{name: "make -v stays denied", cmd: "make -v | head -1", warn: true},
		// A probe flag inside a quoted argument is one word, not a flag: matching
		// parsed words rather than the joined head is what keeps these gates.
		{name: "commit message naming --version still denies", cmd: `git commit -m "chore: bump --version output" | tee tmp/c.log`, warn: true},
		{name: "backgrounded commit naming --help still denies", cmd: `git commit -m "docs: --help text" > tmp/c.log 2>&1; echo "EXIT=$?"`, bg: true, warn: true},

		// --- Owned by the sibling throttle hook --------------------------------
		{name: "go test -race defers", cmd: "go test -race ./... | tee tmp/race.log"},
		{name: "already-throttled -race does not defer", cmd: "nice -n 10 taskpolicy -d throttle go test -race ./... | tail"},

		// --- A backgrounded gate whose status the last statement drops (Q681) --
		// Measured: a backgrounded `false; echo "EXIT=$?"` logs EXIT=1 and
		// notifies exit code 0.
		{name: "the canonical lost background status", cmd: `make check > tmp/check.log 2>&1; echo "EXIT=$?"`, bg: true, warn: true, substr: "task notification reports success"},
		{name: "background gate then an unrelated last statement", cmd: "make check > tmp/check.log 2>&1; grep -c FAILED tmp/check.log", bg: true, warn: true},
		{name: "background scripts gate", cmd: `bash scripts/docs/lint-backlog.sh > tmp/l.log 2>&1; echo "EXIT=$?"`, bg: true, warn: true},
		{name: "background git push", cmd: `git push -u origin HEAD > tmp/p.log 2>&1; echo "EXIT=$?"`, bg: true, warn: true},
		{name: "leading segment before the gate", cmd: `mkdir -p tmp; make check > tmp/c.log 2>&1; echo "EXIT=$?"`, bg: true, warn: true},
		// `||` swallows the failure it was written to report.
		{name: "|| fallback swallows it", cmd: `make check > tmp/c.log 2>&1 || echo "gate failed"`, bg: true, warn: true},
		// `&` is the other spelling, and loses the status even in the foreground.
		{name: "trailing & forks, foreground call", cmd: "make check > tmp/c.log 2>&1 &", warn: true},
		{name: "backgrounded subshell ending in echo", cmd: `(make check > tmp/c.log 2>&1; echo "EXIT=$?")`, bg: true, warn: true},
		// pipefail and $pipestatus are pipe mitigations; neither re-raises a
		// status the last statement already discarded.
		{name: "pipefail does not mitigate this", cmd: `set -o pipefail; make check > tmp/c.log 2>&1; echo "EXIT=$?"`, bg: true, warn: true},

		// --- Backgrounded forms that keep the status ---------------------------
		{name: "the documented fix re-raises it", cmd: `make check > tmp/check.log 2>&1; rc=$?; echo "EXIT=$rc"; exit $rc`, bg: true},
		{name: "gate is the last statement", cmd: "make check > tmp/check.log 2>&1", bg: true},
		{name: "&& chain ending in the gate", cmd: "mkdir -p tmp && make check > tmp/check.log 2>&1", bg: true},
		{name: "&& chain starting with the gate", cmd: `make check > tmp/c.log 2>&1 && echo "clean"`, bg: true},
		// An explicit `exit 0` is a deliberate discard, and the escape hatch for
		// a background call whose status genuinely does not matter.
		{name: "explicit exit 0 is deliberate", cmd: `make check > tmp/c.log 2>&1; echo "EXIT=$?"; exit 0`, bg: true},
		// The SAME command in the foreground is the documented correct form: the
		// echo prints the real status where it can be read.
		{name: "foreground redirect-then-echo is correct", cmd: `make check > tmp/check.log 2>&1; echo "EXIT=$?"`},
		{name: "background non-gate loses nothing worth warning about", cmd: `gh run list > tmp/r.log 2>&1; echo "EXIT=$?"`, bg: true},
		{name: "background pr-sentinel watcher", cmd: "bash /Users/x/.claude/plugins/pr-sentinel/watch.sh 1288", bg: true},
		// The Q624 shape, backgrounded: a gate named inside a string is a word.
		{name: "background echo naming the bug form", cmd: `echo "never background: make check; echo EXIT=$?"`, bg: true},
		{name: "background grep for the pattern", cmd: `grep -rn "make check" docs/ > tmp/o.log 2>&1; echo "EXIT=$?"`, bg: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.cmd, tc.bg, reg, nil)
			if tc.warn && got == "" {
				t.Fatalf("want a warning, got silence\ncommand: %s", tc.cmd)
			}
			if !tc.warn && got != "" {
				t.Fatalf("want silence, got a warning\ncommand: %s\nreason: %s", tc.cmd, got)
			}
			if tc.substr != "" && !strings.Contains(got, tc.substr) {
				t.Fatalf("reason missing %q\nreason: %s", tc.substr, got)
			}
		})
	}
}

// An unanchored pattern searches the whole head, which is how a rule starts
// matching text that merely mentions a command.
func TestShippedRegistryPatternsAreAnchored(t *testing.T) {
	path := filepath.Join("testdata", "piped-gate-guard.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped registry: %v", err)
	}
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("parse shipped registry: %v", err)
	}
	for _, p := range append(append([]string{}, reg.Gates...), reg.Exempt...) {
		if !strings.HasPrefix(p, "^") {
			t.Errorf("pattern not anchored to command position: %s", p)
		}
	}
}

// A command this tool cannot parse gets silence, not a guess.
func TestUnparseableCommandIsSilent(t *testing.T) {
	reg := shippedRegistry(t)
	for _, cmd := range []string{"make check | tail 'unterminated", "make check | | tail", "for do done"} {
		for _, bg := range []bool{false, true} {
			if got := Decide(cmd, bg, reg, nil); got != "" {
				t.Errorf("want silence for unparseable %q (bg=%v), got: %s", cmd, bg, got)
			}
		}
	}
}

// A registry with no gates cannot warn — detection is driven by the registry,
// not by an incidental match somewhere in the walk.
func TestEmptyRegistryNeverWarns(t *testing.T) {
	empty, _ := Registry{}.compile()
	if got := Decide(`make check 2>&1 | tail -30; echo "EXIT=$?"`, false, empty, nil); got != "" {
		t.Errorf("want silence with an empty registry, got: %s", got)
	}
	if got := Decide(`make check > tmp/c.log 2>&1; echo "EXIT=$?"`, true, empty, nil); got != "" {
		t.Errorf("want silence with an empty registry (background), got: %s", got)
	}
}

// A bad pattern degrades the warning; it never breaks the tool.
func TestBadPatternIsDroppedNotFatal(t *testing.T) {
	reg := Registry{Gates: []string{"^make([[:space:]]|$)", "*not a regexp"}}
	c, errs := reg.compile()
	if len(errs) != 1 {
		t.Fatalf("want 1 rejected pattern, got %d", len(errs))
	}
	if got := Decide("make check | tail", false, c, nil); got == "" {
		t.Error("the surviving pattern should still warn")
	}
}

// The pipe verdict wins when a backgrounded call is also piped: both routes
// lose the same status, and the pipe reason names the nearer cause.
func TestPipeVerdictWinsOverBackground(t *testing.T) {
	reg := shippedRegistry(t)
	got := Decide(`make check 2>&1 | tail -30; echo "EXIT=$?"`, true, reg, nil)
	if !strings.Contains(got, "exit status is the filter's") {
		t.Errorf("want the pipe reason, got: %s", got)
	}
}
