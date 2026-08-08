package main

import (
	"strings"
	"testing"
)

// testPrefix stands in for whatever local-throttle.sh resolves on the host, so
// the assertions read the same on every platform and cost no subprocess.
const testPrefix = "TP -d throttle"

func decide(cmd string) *Decision {
	return Decide(cmd, func() string { return testPrefix })
}

// Both directions are asserted, because both fail silently (Q624). A rule that
// matches too much turns every `git show`, `grep`, and commit message that
// merely NAMES a go command into a rewrite — and this runs on every Bash call,
// so a message quoting `go test -race` was once denied outright. A rule that
// stops matching is the more expensive error: an unthrottled `-race` run
// saturates the machine and freezes the GUI (Q92). Every must-not-match case
// below is paired with a must-match one built from the same text.
func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		// want is the permission decision, or "" for silence.
		want string
		// wantCmd, when set, is the exact rewritten command.
		wantCmd string
		// wantReason, when set, must appear in the reason.
		wantReason string
	}{
		// --- The bare form: rewritten and auto-allowed ------------------------
		{name: "bare go test", cmd: "go test ./...", want: allow, wantCmd: testPrefix + " go test ./..."},
		{name: "bare go build", cmd: "go build ./...", want: allow, wantCmd: testPrefix + " go build ./..."},
		{
			name: "leading assignment stays in front", cmd: "GOFLAGS=-mod=mod go test -race ./...",
			want: allow, wantCmd: "GOFLAGS=-mod=mod " + testPrefix + " go test -race ./...",
		},
		{name: "quoted head is one command", cmd: `"go" test ./...`, want: allow},

		// --- Compound / redirected -race: rewritten and asked -----------------
		{
			name: "subshell with cd", cmd: "(cd cmd/agc && go test -race ./...)",
			want: ask, wantCmd: "(cd cmd/agc && " + testPrefix + " go test -race ./...)",
		},
		{
			name: "redirect to a file", cmd: "go test -race ./... > out.log 2>&1",
			want: ask, wantCmd: testPrefix + " go test -race ./... > out.log 2>&1",
		},
		{
			name: "piped to tee", cmd: "go test -race ./... | tee out.log",
			want: ask, wantCmd: testPrefix + " go test -race ./... | tee out.log",
		},
		{
			name: "chained after cd", cmd: "cd cmd/agc && go test -race ./...",
			want: ask, wantCmd: "cd cmd/agc && " + testPrefix + " go test -race ./...",
		},
		{
			name: "newline separated", cmd: "cd cmd/agc\ngo test -race ./...",
			want: ask, wantCmd: "cd cmd/agc\n" + testPrefix + " go test -race ./...",
		},
		{
			name: "backgrounded", cmd: "go test -race ./... &",
			want: ask, wantCmd: testPrefix + " go test -race ./... &",
		},
		{
			// bash parses `time` as a keyword, so the call it times is walked on
			// its own and needs no wrapper entry.
			name: "time keyword", cmd: "time go test -race ./...",
			want: ask, wantCmd: "time " + testPrefix + " go test -race ./...",
		},
		{
			name: "inside a command substitution", cmd: "out=$(go test -race ./...)",
			want: ask, wantCmd: "out=$(" + testPrefix + " go test -race ./...)",
		},

		// --- More invocations than prefixes to place: denied ------------------
		{
			name: "two invocations", cmd: "go build ./... && go test -race ./...",
			want: deny, wantReason: "more than one go build/test",
		},
		{
			name: "two invocations, one wrapped", cmd: "timeout 900 go test -race ./... && go build ./...",
			want: deny, wantReason: "more than one go build/test",
		},

		// --- Q696: a -race behind a wrapper is throttled, never auto-allowed ---
		//
		// `go` is an argument here, not a command, so the scanner this replaces
		// reported nothing and the run escaped the throttle entirely.
		{
			name: "timeout", cmd: "timeout 900 go test -race ./...",
			want: ask, wantCmd: "timeout 900 " + testPrefix + " go test -race ./...",
		},
		{
			name: "timeout with a signal flag", cmd: "timeout -s KILL 900 go test -race ./...",
			want: ask, wantCmd: "timeout -s KILL 900 " + testPrefix + " go test -race ./...",
		},
		{
			name: "timeout after --", cmd: "timeout -- 900 go test -race ./...",
			want: ask, wantCmd: "timeout -- 900 " + testPrefix + " go test -race ./...",
		},
		{
			name: "env with an assignment", cmd: "env GOFLAGS=-mod=mod go test -race ./...",
			want: ask, wantCmd: "env GOFLAGS=-mod=mod " + testPrefix + " go test -race ./...",
		},
		{
			name: "nested wrappers", cmd: "nohup timeout 900 go test -race ./...",
			want: ask, wantCmd: "nohup timeout 900 " + testPrefix + " go test -race ./...",
		},
		{
			name: "wrapper inside a subshell", cmd: "(cd cmd/agc && timeout 900 go test -race ./...)",
			want: ask, wantCmd: "(cd cmd/agc && timeout 900 " + testPrefix + " go test -race ./...)",
		},

		// The peel is an allowlist, and everything it cannot describe is silence
		// rather than a guessed offset: a misread peel would emit a rewrite
		// nobody wrote.
		{name: "unknown wrapper", cmd: "xargs -n1 go test -race ./..."},
		{name: "unknown option to a known wrapper", cmd: "timeout --frobnicate 900 go test -race ./..."},
		{name: "attached option form", cmd: "timeout --signal=KILL 900 go test -race ./..."},
		{name: "wrapper with no command", cmd: "timeout 900"},
		// A wrapped invocation is never auto-allowed, so a non-race one stays on
		// the normal permission flow — widening it would add a prompt where
		// there is none today.
		{name: "wrapped without -race", cmd: "timeout 900 go test ./..."},

		// --- Q624: text that only NAMES a go command is not an invocation -----
		{name: "commit message quotes -race", cmd: `git commit -m "docs: note that go test -race needs the prefix"`},
		{name: "quoted, then chained", cmd: `git commit -m "docs: go test -race notes" && git push`},
		{name: "grep pattern", cmd: `grep -rn 'go test -race' scripts/agent`},
		{name: "quoted heredoc body", cmd: "git commit -F - <<'MSG'\nfix: go test -race notes\nMSG"},
		{
			// The flag is read from the invocation's own words, so a -race in a
			// message cannot upgrade a plain `go test` to the -race path.
			name: "-race in a message, plain go test alongside",
			cmd:  `go test ./... ; git commit -m "docs: go test -race notes"`,
		},
		// The paired must-match direction: the same message text with a real
		// invocation after it is still seen and still throttled.
		{
			name:    "heredoc message then a real -race",
			cmd:     "git commit -F - <<'MSG'\nfix: go test -race notes\nMSG\ngo test -race ./...",
			want:    ask,
			wantCmd: "git commit -F - <<'MSG'\nfix: go test -race notes\nMSG\n" + testPrefix + " go test -race ./...",
		},

		// The insertion point is a byte offset into the command, and Go slices
		// bytes — so a multi-byte rune ahead of the `go` token must not shift it.
		{
			name:    "multi-byte text before the invocation",
			cmd:     `git commit -m "docs: café — notes" && go test -race ./...`,
			want:    ask,
			wantCmd: `git commit -m "docs: café — notes" && ` + testPrefix + " go test -race ./...",
		},

		// --- Already throttled, and not ours ----------------------------------
		{name: "carries the current prefix", cmd: "(cd cmd/agc && nice -n 10 taskpolicy -d throttle go test -race ./...)"},
		// The pre-Q441 prefix still reads as throttled: a stale worktree emits
		// it, and re-wrapping would stack two prefixes.
		{name: "carries the legacy prefix", cmd: "(cd cmd/agc && taskpolicy -c utility go test -race ./...)"},
		{name: "computed via local-throttle.sh", cmd: "$(scripts/agent/local-throttle.sh prefix) go test -race ./..."},
		{name: "a different toolchain", cmd: "(cd rust && cargo test)"},
		{name: "go, but not build or test", cmd: "go vet ./..."},
		{name: "a command that merely starts with go", cmd: "gofmt -l ."},
		{name: "no go at all", cmd: "make check"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decide(tc.cmd)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want silence, got %s: %q", got.Permission, got.Command)
				}
				return
			}
			if got == nil {
				t.Fatalf("want %s, got silence", tc.want)
			}
			if got.Permission != tc.want {
				t.Fatalf("want %s, got %s", tc.want, got.Permission)
			}
			if tc.wantCmd != "" && got.Command != tc.wantCmd {
				t.Fatalf("want command %q, got %q", tc.wantCmd, got.Command)
			}
			if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("want reason containing %q, got %q", tc.wantReason, got.Reason)
			}
		})
	}
}

// The rewrite is the whole point, so assert its invariant directly rather than
// only through equality: whatever the shape, the prefix ends up immediately in
// front of the throttled `go`, and `-race` is still there to be throttled.
func TestRewritePlacesPrefixBeforeGo(t *testing.T) {
	for _, cmd := range []string{
		"go test -race ./...",
		"(cd cmd/agc && go test -race ./...)",
		"timeout 900 go test -race ./...",
		"env GOFLAGS=-mod=mod go test -race ./...",
		"GOFLAGS=-mod=mod go test -race ./...",
		"go test -race ./... > out.log 2>&1",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := decide(cmd)
			if got == nil || got.Command == "" {
				t.Fatal("want a rewrite, got none")
			}
			if !strings.Contains(got.Command, testPrefix+" go test -race") {
				t.Fatalf("prefix does not precede the go invocation: %q", got.Command)
			}
		})
	}
}

// An empty prefix means throttling is off (CI, headless, SSH, an unsupported
// OS). There is nothing to apply, so the hook must have no opinion rather than
// emit a rewrite that prepends nothing.
func TestNoPrefixIsSilence(t *testing.T) {
	for _, cmd := range []string{"go test ./...", "(cd x && go test -race ./...)", "timeout 900 go test -race ./..."} {
		if got := Decide(cmd, func() string { return "" }); got != nil {
			t.Fatalf("%q: want silence with no prefix, got %s", cmd, got.Permission)
		}
	}
}

// The prefix costs a subprocess and this hook fires on every Bash call, so it
// must not be resolved for a command with nothing to throttle.
func TestPrefixNotResolvedWithoutAnInvocation(t *testing.T) {
	var calls int
	for _, cmd := range []string{"make check", `git commit -m "go test -race"`, "ls -la"} {
		Decide(cmd, func() string { calls++; return testPrefix })
	}
	if calls != 0 {
		t.Fatalf("prefix resolved %d times for commands with no invocation", calls)
	}
}
