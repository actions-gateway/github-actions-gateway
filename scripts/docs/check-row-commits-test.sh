#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-row-commits.sh — the row-deletion commit
# rule (Q1100).
#
# The gate reads a branch's commits rather than its tree, so each case builds a
# throwaway repo with a base commit, an origin/main pointing at it, and one or
# more commits on top. The assertion is about which commit the deletion landed
# in, which is the one thing no tree check can see.
#
# The red case and its controls are the point. A row deleted inside a feature
# commit must fail; the same deletion in its own `docs(queue): close QNNN`
# commit must pass; and a branch that deletes no row at all must pass, or the
# gate would have degenerated into "this branch touched docs/queue/".
#
# The remaining cases pin the shapes that would otherwise pass by checking
# nothing: a row commit naming *other* rows must not count for this one (a groom
# must not spend its verb on whatever else the diff holds), an unloadable
# queue.py is a refusal rather than a clean run, and the fail-open skip on a
# missing base is opt-out via ROW_COMMITS_REQUIRE_BASE.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/docs/check-row-commits.sh"

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

# new_repo — a repo carrying the real queue.py (the gate imports its verb test),
# two backlog rows, and an origin/main at the base commit.
new_repo() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q -b main
    git -C "$WORK" config user.email t@example.com
    git -C "$WORK" config user.name Test
    # git spawns a detached `git maintenance run --auto` from commit, which
    # outlives it and prunes the fixture while the next command writes to it
    # (Q820). check-fixture-maintenance.sh holds every suite to this.
    git -C "$WORK" config maintenance.auto false
    mkdir -p "$WORK/docs/queue" "$WORK/scripts/docs" "$WORK/docs/development"
    cp "$REPO_ROOT/scripts/docs/queue.py" "$WORK/scripts/docs/queue.py"
    printf -- '---\nid: Q1\nrank: a\nlabels:\n    - docs\nstatus: ready\nsize: S\n---\n\n# One\n\nBody.\n' \
        >"$WORK/docs/queue/Q1.md"
    printf -- '---\nid: Q2\nrank: b\nlabels:\n    - docs\nstatus: ready\nsize: S\n---\n\n# Two\n\nBody.\n' \
        >"$WORK/docs/queue/Q2.md"
    printf '# Doc\n\nBase.\n' >"$WORK/docs/development/page.md"
    git -C "$WORK" add -A
    git -C "$WORK" commit -qm base
    git -C "$WORK" update-ref refs/remotes/origin/main HEAD
}

# commit_msg SUBJECT — commit everything currently in the tree under SUBJECT.
commit_msg() {
    git -C "$WORK" add -A
    git -C "$WORK" commit -qm "$1"
}

# run_gate NAME WANT — run the gate inside the fixture and compare its exit
# status, which is what `make check` and the docs workflow consume.
run_gate() {
    local name="$1" want="$2" got=0
    (cd "$WORK" && "$GATE") >"$WORK/gate.out" 2>&1 || got=$?
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

# --- the red case, and the two controls that give it meaning ----------------

new_repo
rm "$WORK/docs/queue/Q1.md"
printf 'The fix.\n' >>"$WORK/docs/development/page.md"
commit_msg "docs: fix the thing (Q1)"
run_gate "a row deleted inside a feature commit fails" 1
expect_out "the finding names the row" 'Q1'

new_repo
printf 'The fix.\n' >>"$WORK/docs/development/page.md"
commit_msg "docs: fix the thing (Q1)"
rm "$WORK/docs/queue/Q1.md"
commit_msg "docs(queue): close Q1 now the fix has shipped"
run_gate "the same deletion in its own row commit passes" 0

# Without this control a gate that had degenerated into "this branch touched
# docs/queue/" would pass every case above.
new_repo
printf 'The fix.\n' >>"$WORK/docs/development/page.md"
commit_msg "docs: fix the thing"
run_gate "a branch deleting no row passes" 0

# --- a row commit speaks only for the rows it names --------------------------

new_repo
rm "$WORK/docs/queue/Q1.md"
commit_msg "docs(queue): file Q2 for the follow-on work"
run_gate "a row commit naming other rows does not count" 1

new_repo
rm "$WORK/docs/queue/Q1.md" "$WORK/docs/queue/Q2.md"
commit_msg "docs(queue): close Q1 and close Q2 together"
run_gate "one row commit naming both deleted rows passes" 0

# --- the base, and its fail-open posture ------------------------------------

new_repo
rm "$WORK/docs/queue/Q1.md"
commit_msg "docs: fix the thing (Q1)"
git -C "$WORK" update-ref -d refs/remotes/origin/main
run_gate "no resolvable base skips rather than reddening every PR" 0

new_repo
rm "$WORK/docs/queue/Q1.md"
commit_msg "docs: fix the thing (Q1)"
git -C "$WORK" update-ref -d refs/remotes/origin/main
got=0
(cd "$WORK" && ROW_COMMITS_REQUIRE_BASE=1 "$GATE") >"$WORK/gate.out" 2>&1 || got=$?
if [[ "$got" == 2 ]]; then
    printf 'ok   %s\n' "a caller that required a base gets a hard error"
else
    printf 'FAIL %s: want exit 2, got %s\n' "a caller that required a base gets a hard error" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
fi

# --- the verb test is queue.py's, so losing it is a refusal ------------------

new_repo
rm "$WORK/docs/queue/Q1.md"
commit_msg "docs(queue): close Q1 now the fix has shipped"
rm "$WORK/scripts/docs/queue.py"
run_gate "an unloadable queue.py is a refusal, not a clean run" 2

if ((fails > 0)); then
    printf '\n%d test(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\nall tests passed\n'
