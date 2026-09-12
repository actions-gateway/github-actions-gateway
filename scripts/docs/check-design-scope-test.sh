#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-design-scope.sh — the design-doc scope rule
# (Q774).
#
# The gate reads a branch rather than a tree, so each case builds a throwaway
# repo with a base commit and a branch commit on top, and the assertion is about
# what the branch ADDED. That is the whole design: keying on "a design file
# changed" would fire on every typo and be waived into meaninglessness, so the
# trigger is a scope sentence appearing where there was none.
#
# The red case and its two controls are the point. A scope statement added under
# docs/design/ with nothing under docs/operations/ must fail; the same statement
# alongside an operations edit must pass; and an edit to the same design file
# that states no scope must pass. Without the third, a gate that had degenerated
# into "design changed, operations did not" would pass this suite.
#
# The remaining cases pin the escape marker, both calibrated vocabularies, and
# the fail-open posture with its DESIGN_SCOPE_REQUIRE_BASE override — a gate that
# skips silently is the shape the em-dash ratchet was fixed for.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/docs/check-design-scope.sh"

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

# new_repo — a repo with an origin/main to diff against, holding one design doc
# and one operations doc, each with a line the branch can edit.
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
    mkdir -p "$WORK/docs/design" "$WORK/docs/operations"
    printf '# Design\n\nThe reconciler owns the pod template.\n' >"$WORK/docs/design/arch.md"
    printf '# Runbook\n\nWhat to do when a pod will not start.\n' >"$WORK/docs/operations/runbook.md"
    git -C "$WORK" add -A
    git -C "$WORK" commit -qm base
    # The gate resolves its base as merge-base(HEAD, origin/main), so give the
    # fixture an origin/main pointing at the base commit.
    git -C "$WORK" update-ref refs/remotes/origin/main HEAD
}

# branch_commit — commit whatever the case has written, as the branch's change.
branch_commit() {
    git -C "$WORK" add -A
    git -C "$WORK" commit -qm change
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
printf 'A RunnerSet naming an unknown profile is rejected at admission.\n' \
    >>"$WORK/docs/design/arch.md"
branch_commit
run_gate "a scope statement with no operator doc fails" 1
expect_out "the finding names the category" 'admission-reject'

new_repo
printf 'A RunnerSet naming an unknown profile is rejected at admission.\n' \
    >>"$WORK/docs/design/arch.md"
printf 'The rejection reads: profile not in the allowlist.\n' \
    >>"$WORK/docs/operations/runbook.md"
branch_commit
run_gate "the same statement alongside an operations edit passes" 0

# Without this control a gate that had degenerated into "a design file changed
# and an operations file did not" would pass every case above.
new_repo
printf 'The reconciler rebuilds the template on every pass.\n' \
    >>"$WORK/docs/design/arch.md"
branch_commit
run_gate "a design edit stating no scope passes" 0

# --- both calibrated vocabularies -------------------------------------------

new_repo
printf 'The worker pod TTL defaults to five minutes.\n' >>"$WORK/docs/design/arch.md"
branch_commit
run_gate "a changed default with no operator doc fails" 1
expect_out "the finding names the default category" 'default-change'

# --- the escape marker ------------------------------------------------------

new_repo
printf 'A RunnerSet naming an unknown profile is rejected at admission. <!-- operator-surface: already in runbook.md § Admission -->\n' \
    >>"$WORK/docs/design/arch.md"
branch_commit
run_gate "a line carrying the escape marker is silenced" 0

# The marker silences its own line only, never the file.
new_repo
{
    printf 'A RunnerSet naming an unknown profile is rejected at admission. <!-- operator-surface: already documented -->\n'
    printf 'A second request is rejected for a different reason entirely.\n'
} >>"$WORK/docs/design/arch.md"
branch_commit
run_gate "the marker does not silence the rest of the file" 1

# --- the base, and its fail-open posture ------------------------------------

new_repo
printf 'A RunnerSet naming an unknown profile is rejected at admission.\n' \
    >>"$WORK/docs/design/arch.md"
branch_commit
git -C "$WORK" update-ref -d refs/remotes/origin/main
run_gate "no resolvable base skips rather than reddening every PR" 0

new_repo
printf 'A RunnerSet naming an unknown profile is rejected at admission.\n' \
    >>"$WORK/docs/design/arch.md"
branch_commit
git -C "$WORK" update-ref -d refs/remotes/origin/main
got=0
(cd "$WORK" && DESIGN_SCOPE_REQUIRE_BASE=1 "$GATE") >"$WORK/gate.out" 2>&1 || got=$?
if [[ "$got" == 2 ]]; then
    printf 'ok   %s\n' "a caller that required a base gets a hard error"
else
    printf 'FAIL %s: want exit 2, got %s\n' "a caller that required a base gets a hard error" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
fi

if ((fails > 0)); then
    printf '\n%d test(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\nall tests passed\n'
