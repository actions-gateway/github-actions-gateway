#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-status-isolation.sh — the gate that keeps a
# docs/STATUS.md commit from carrying anything else.
#
# Reading the range logic predicts coverage; building the commit measures it.
# Each case builds a throwaway git repo with a `main` and a `topic` branch,
# commits a shape, and asserts the gate's exit status against `--base main
# --head topic`. The red cases plant the defect three ways — authored directly,
# arrived at by `git commit --amend`, and buried mid-range behind an innocent
# tip. The green cases pin what must not trip: a lone backlog commit, several of
# them in one branch (the real shape — PR #1239 landed three), a branch that
# never touches the backlog, a mixed commit that is already on the base, a merge
# commit, the allow-list, and a base that does not resolve.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
GATE="$REPO_ROOT/scripts/docs/check-status-isolation.sh"

fails=0
workdirs=()
WORK=""

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() {
    local d
    for d in "${workdirs[@]}"; do
        rm -rf "$d"
    done
}
trap cleanup EXIT

# new_repo — a throwaway repo on `main` with one commit, and `topic` checked out
# from it. The gate resolves its root with `git rev-parse --show-toplevel`, so
# running it from inside WORK scopes it to the fixture rather than the real tree.
new_repo() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q -b main
    git -C "$WORK" config user.email test@example.com
    git -C "$WORK" config user.name test
    mkdir -p "$WORK/docs"
    printf 'baseline\n' >"$WORK/docs/STATUS.md"
    printf 'package p\n' >"$WORK/pkg.go"
    git -C "$WORK" add docs/STATUS.md pkg.go
    git -C "$WORK" commit -qm 'chore: baseline'
    git -C "$WORK" checkout -q -b topic
}

# commit_status MSG — one more line in the backlog, committed alone.
commit_status() {
    printf 'row\n' >>"$WORK/docs/STATUS.md"
    git -C "$WORK" add docs/STATUS.md
    git -C "$WORK" commit -qm "$1"
}

# commit_code MSG — a source-only commit.
commit_code() {
    printf '// %s\n' "$1" >>"$WORK/pkg.go"
    git -C "$WORK" add pkg.go
    git -C "$WORK" commit -qm "$1"
}

# commit_mixed MSG — the defect: both files in one commit.
commit_mixed() {
    printf 'row\n' >>"$WORK/docs/STATUS.md"
    printf '// %s\n' "$1" >>"$WORK/pkg.go"
    git -C "$WORK" add docs/STATUS.md pkg.go
    git -C "$WORK" commit -qm "$1"
}

# run_gate NAME WANT [ARGS...] — run the gate inside WORK and compare its exit
# status with WANT. Defaults to the branch range every case uses.
run_gate() {
    local name="$1" want="$2" got=0
    shift 2
    if (($# == 0)); then
        set -- --base main --head topic
    fi
    (cd "$WORK" && "$GATE" "$@") >"$WORK/gate.out" 2>&1 || got=$?
    if [[ "$got" == "$want" ]]; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

# assert_eq NAME WANT GOT — a plain value assertion, for the amend differential.
assert_eq() {
    if [[ "$2" == "$3" ]]; then
        printf 'ok   %s\n' "$1"
        return
    fi
    printf 'FAIL %s: want %q, got %q\n' "$1" "$2" "$3"
    fails=$((fails + 1))
}

# --- red: the defect the gate exists to catch -------------------------------

new_repo
commit_mixed 'docs(status): complete Q1'
run_gate 'a commit mixing the backlog with code is rejected' 1

# The case Q652 names, and the one the pre-commit hook structurally cannot see:
# only docs/STATUS.md is staged, so `lint-backlog.sh --staged` reads a clean
# index — but the amend rewrites a code commit, so the result is mixed. The
# assertion below measures that differential rather than asserting it.
new_repo
commit_code 'feat: a change'
printf 'row\n' >>"$WORK/docs/STATUS.md"
git -C "$WORK" add docs/STATUS.md
staged="$(git -C "$WORK" diff --cached --name-only)"
assert_eq 'the index an amend presents to the hook names only the backlog' \
    'docs/STATUS.md' "$staged"
git -C "$WORK" commit -q --amend --no-edit
run_gate 'a mixed commit produced by --amend is rejected' 1

new_repo
commit_mixed 'docs(status): complete Q1'
commit_code 'feat: later work'
run_gate 'a mixed commit behind an innocent tip is rejected' 1

# --- green: the shapes real branches actually have --------------------------

new_repo
commit_status 'docs(status): complete Q1'
run_gate 'a lone backlog commit stays green' 0

new_repo
commit_code 'feat: the work'
commit_status 'docs(status): complete Q1'
commit_status 'docs(status): file Q2'
commit_status 'docs(status): defer Q3'
run_gate 'several isolated backlog commits in one branch stay green' 0

new_repo
commit_code 'feat: the work'
commit_code 'test: cover it'
run_gate 'a branch that never touches the backlog stays green' 0

# The base's own history is out of scope by construction — which is what makes
# it safe never to run this over main, where every squash-merge is mixed.
new_repo
git -C "$WORK" checkout -q main
commit_mixed 'squash: a merged PR, mixed by construction'
git -C "$WORK" checkout -q topic
git -C "$WORK" merge -q main
commit_status 'docs(status): complete Q1'
run_gate 'a mixed commit already on the base is out of range' 0

new_repo
commit_code 'feat: the work'
git -C "$WORK" checkout -q main
commit_status 'docs(status): complete Q1'
git -C "$WORK" checkout -q topic
git -C "$WORK" merge -q --no-ff -m 'merge main' main
run_gate 'a merge commit is skipped' 0

new_repo
commit_mixed 'docs(status): complete Q1'
sha="$(git -C "$WORK" rev-parse --short HEAD)"
BACKLOG_ALLOW_MIXED_COMMITS="$sha" run_gate 'the allow-list admits a named commit' 0
BACKLOG_ALLOW_MIXED_COMMITS='deadbeef' run_gate 'the allow-list does not admit a different commit' 1

new_repo
commit_mixed 'docs(status): complete Q1'
run_gate 'an unresolvable base skips rather than fails' 0 --base origin/main --head topic

new_repo
run_gate 'an empty range stays green' 0

if ((fails > 0)); then
    printf '\ncheck-status-isolation-test: %d assertion(s) failed\n' "$fails" >&2
    exit 1
fi

printf '\ncheck-status-isolation-test: all assertions passed\n'
exit 0
