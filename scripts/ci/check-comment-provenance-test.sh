#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-comment-provenance.sh.
#
# The gate reads what a branch added, so each case builds a throwaway repo with
# a base commit and a branch change on top. Every passing case also asserts how
# many added comment lines the gate read: a lexer that stopped finding comments
# would otherwise pass all of them.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/ci/check-comment-provenance.sh"

fails=0
workdirs=()
WORK=""

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`
cleanup() {
    local d
    for d in "${workdirs[@]}"; do
        rm -rf "$d"
    done
}
trap cleanup EXIT

# new_repo — a repo whose origin/main is the base commit. The base already
# holds a comment that breaks the rule, which a branch may leave alone.
new_repo() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q -b main
    git -C "$WORK" config user.email t@example.com
    git -C "$WORK" config user.name Test
    # A detached `git maintenance run --auto` outlives the commit and prunes
    # the fixture under the next command.
    git -C "$WORK" config maintenance.auto false
    printf 'package a\n\n// Old explains Q100.\nvar Old = 1\n' >"$WORK/old.go"
    git -C "$WORK" add -A
    git -C "$WORK" commit -qm base
    git -C "$WORK" update-ref refs/remotes/origin/main HEAD
}

# put PATH — write stdin to PATH inside the fixture.
put() {
    mkdir -p "$WORK/$(dirname "$1")"
    cat >"$WORK/$1"
}

branch_commit() {
    git -C "$WORK" add -A
    git -C "$WORK" commit -qm change
}

# run_gate NAME WANT [ARGS...] — run the gate in the fixture and compare its
# exit status.
run_gate() {
    local name="$1" want="$2" got=0
    shift 2
    (cd "$WORK" && "$GATE" "$@") >"$WORK/gate.out" 2>&1 || got=$?
    die_if_killed "$name" "$got" "$want"
    if [[ "$got" == "$want" ]]; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

# expect_out NAME PATTERN — assert the last run's output matched PATTERN.
expect_out() {
    local name="$1" pattern="$2"
    if grep -q -- "$pattern" "$WORK/gate.out"; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: no match for %s in:\n' "$name" "$pattern"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

# fails_with NAME PATH RULE — commit stdin as PATH and require RULE to fire.
fails_with() {
    local name="$1" path="$2" rule="$3"
    new_repo
    put "$path"
    branch_commit
    run_gate "$name" 1
    expect_out "$name: names the rule" "\[$rule\]"
}

# passes_with NAME PATH LINES — commit stdin as PATH and require a pass that
# read LINES added comment lines.
passes_with() {
    local name="$1" path="$2" lines="$3"
    new_repo
    put "$path"
    branch_commit
    run_gate "$name" 0
    expect_out "$name: read $lines comment line(s)" " $lines added comment line(s)"
}

# --- each rule, in each language --------------------------------------------

fails_with "go: a backlog ID in a line comment" a.go backlog-id <<'EOF'
package a

// Limit caps retries, which Q123 raised.
var Limit = 3
EOF

fails_with "go: a PR number in a trailing comment" a.go pr-number <<'EOF'
package a

var Limit = 3 // raised in #1234
EOF

fails_with "go: a PR number in a block comment" a.go pr-number <<'EOF'
package a

/*
Limit was settled in PR 77.
*/
var Limit = 3
EOF

fails_with "shell: a date" a.sh date <<'EOF'
#!/usr/bin/env bash
# Retries three times after the 2026-11-01 outage.
retry 3
EOF

fails_with "shell: change narration" a.sh narration <<'EOF'
#!/usr/bin/env bash
# The loop used to run twice.
retry 3
EOF

fails_with "python: a backlog ID in a comment" a.py backlog-id <<'EOF'
LIMIT = 3  # Q99 raised this
EOF

fails_with "python: a backlog ID in a docstring" a.py backlog-id <<'EOF'
def limit():
    """Return the retry limit Q99 settled."""
    return 3
EOF

fails_with "yaml: a backlog ID" .github/workflows/a.yml backlog-id <<'EOF'
jobs:
  a:
    # Q456 split this job out.
    runs-on: ubuntu-latest
EOF

fails_with "yaml: a shell comment inside a run block" .github/workflows/a.yml backlog-id <<'EOF'
jobs:
  a:
    steps:
      - run: |
          # Q456 needs the fetch first.
          git fetch
EOF

fails_with "makefile: a backlog ID in help text" Makefile backlog-id <<'EOF'
check: ## Run the gates (Q12)
	true
EOF

# --- the forms that must stay legal -----------------------------------------

passes_with "go: TODO(QNNN) names open work" a.go 1 <<'EOF'
package a

// TODO(Q123): drop the fallback once the row closes.
var Limit = 3
EOF

passes_with "go: an upstream version citation" a.go 2 <<'EOF'
package a

// Neither project sets EventTime today (measured live: CA v1.36.1,
// Karpenter v1.14.0), so the window reads LastTimestamp.
var Window = 5
EOF

passes_with "shell: a measurement dated on the wrapped line" a.sh 3 <<'EOF'
#!/usr/bin/env bash
# helm exits 0 on an out-of-range index (measured against helm v3.16.2,
# 2026-11-01), so the render is checked for the key instead.
helm template .
EOF

passes_with "shell: a date beside the word measured" a.sh 2 <<'EOF'
#!/usr/bin/env bash
# GitHub lists a job that never ran as cancelled (measured 2026-11-23).
true
EOF

passes_with "shell: a date beside a version" a.sh 2 <<'EOF'
#!/usr/bin/env bash
# cert-manager v1.20.2 (2026-11-01) still ships the flag.
true
EOF

passes_with "go: an upstream issue reference and a URL" a.go 2 <<'EOF'
package a

// Works around kubernetes/kubernetes#12345, which is still open.
// See https://github.com/moby/moby/pull/52193 for the fix.
var Limit = 3
EOF

passes_with "shell: a date inside a path" a.sh 3 <<'EOF'
#!/usr/bin/env bash
# See docs/postmortems/2026-08-15-rc2-tagged-a-stale-commit.md
# and the capture in fixtures/2026-08-15.json.
true
EOF

passes_with "go: a date inside a timestamp value" a.go 1 <<'EOF'
package a

// Epoch is the fixture clock, 2024-01-01T00:00:00Z.
var Epoch = 1704067200
EOF

passes_with "go: the purpose sense of used to" a.go 2 <<'EOF'
package a

// Label is used to scope the pool.
// Used to key the image cache as well.
var Label = "pool"
EOF

passes_with "go: the escape marker silences its own line" a.go 1 <<'EOF'
package a

// Named for Q123's fixture. comment-provenance: the fixture file is q123.json
var Fixture = "q123.json"
EOF

# --- comment syntax that is not a comment -----------------------------------

passes_with "go: comment syntax inside strings" a.go 1 <<'EOF'
package a

// Samples holds comment-shaped text.
var Samples = []string{
	"// Q123 is not a comment",
	`// neither is Q124, from #1234 on 2026-11-01`,
	"a /* Q125 */ b",
}
EOF

passes_with "shell: comment syntax inside a string and a heredoc" a.sh 2 <<'EOF'
#!/usr/bin/env bash
echo "# Q123 is not a comment" 'nor # Q124'
cat <<EOT
# Q125 landed in #1234 on 2026-11-01
EOT
echo "${#BASH_SOURCE[@]} $#" # one real comment
EOF

passes_with "yaml: a hash inside a quoted scalar" a.yml 1 <<'EOF'
# One real comment.
name: "build # Q123"
title: 'release # Q124'
EOF

# A quote opened inside a command substitution must not swallow the rest of the
# file: the comment after it is real, and has to fail.
fails_with "shell: a comment after a nested quote is still read" a.sh backlog-id <<'EOF'
#!/usr/bin/env bash
x="$(echo "it's")"
# Q123 raised this.
EOF

fails_with "shell: a comment after a heredoc is still read" a.sh backlog-id <<'EOF'
#!/usr/bin/env bash
cat <<-'EOT'
	# Q125 inside the body
	EOT
# Q123 after it.
EOF

# --- what a branch touched, and what it did not -----------------------------

new_repo
printf '\n// New has nothing to cite.\nvar New = 2\n' >>"$WORK/old.go"
branch_commit
run_gate "an existing violation the branch did not touch passes" 0
expect_out "only the added line was read" ' 1 added comment line(s)'

new_repo
printf 'package a\n\n// Old explains Q100 and Q101.\nvar Old = 1\n' >"$WORK/old.go"
branch_commit
run_gate "an existing violation the branch rewrote fails" 1

new_repo
put b.go <<'EOF'
package a

// B exists because of Q123.
var B = 1
EOF
run_gate "an untracked file is read before its first commit" 1

new_repo
printf '\n// Staged cites Q123.\nvar Staged = 2\n' >>"$WORK/old.go"
run_gate "an uncommitted edit is read" 1

# --- files out of scope -----------------------------------------------------

new_repo
put vendor/x/x.go <<'EOF'
package x

// X cites Q123.
var X = 1
EOF
put testdata/y.sh <<'EOF'
# Q123
EOF
put zz_generated.deepcopy.go <<'EOF'
// Code generated by controller-gen. DO NOT EDIT.

package a

// Q123
EOF
put notes.md <<'EOF'
# Q123 in prose is not this gate's subject
EOF
branch_commit
run_gate "vendored, testdata, generated and prose files are skipped" 0
expect_out "no source file was read" 'ok (0 changed source file(s)'

# --- the base ---------------------------------------------------------------

new_repo
printf '\n// New cites Q123.\nvar New = 2\n' >>"$WORK/old.go"
branch_commit
git -C "$WORK" update-ref -d refs/remotes/origin/main
run_gate "no resolvable base skips" 0
expect_out "the skip says so" 'skipping'
run_gate "an explicit base is used in its place" 1 --base HEAD~1

got=0
(cd "$WORK" && COMMENT_PROVENANCE_REQUIRE_BASE=1 "$GATE") >"$WORK/gate.out" 2>&1 || got=$?
if [[ "$got" == 2 ]]; then
    printf 'ok   %s\n' "a caller that required a base gets a hard error"
else
    printf 'FAIL %s: want exit 2, got %s\n' "a caller that required a base gets a hard error" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
fi

# --- the reporting modes ----------------------------------------------------

new_repo
run_gate "--all --report reads the untouched base and exits 0" 0 --all --report
expect_out "the base's violation is reported" 'backlog-id.*old.go:3'

if ((fails > 0)); then
    printf '\n%d test(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\nall tests passed\n'
