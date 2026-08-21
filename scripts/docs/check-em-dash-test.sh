#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-em-dash.sh — the em-dash density gate
# (Q654). What the counter excludes is asserted in Go
# (devtools/docs/emdash/main_test.go); this suite covers the half that lives in
# shell — which files are selected, and how the baseline of per-file ceilings
# changes the verdict.
#
# The first pair is the injected defect and its controls: an em-dash added to
# prose must go red, and the same character added inside a code fence or a
# heading's title separator must stay green. Reading the counter predicts
# coverage; running it measures it.
#
# Each case builds a throwaway git repo and asserts the gate's exit status,
# which is what `make check` and the doc-links workflow consume.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
GATE="$REPO_ROOT/scripts/docs/check-em-dash.sh"

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

# new_repo — start a throwaway git repo and point WORK at it. The gate resolves
# its root with `git rev-parse --show-toplevel`, so running it from inside this
# directory scopes it to the fixture rather than the real tree.
new_repo() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q
    : >"$WORK/baseline.txt"
}

# commit_all MSG — commit the fixture's worktree. `git init` alone leaves no
# commit and no remote ref, which is the gate's fail-open path; a fixture that
# needs a base to ratchet against calls this and then set_base.
commit_all() {
    git -C "$WORK" add -A
    git -C "$WORK" -c user.email=t@example.invalid -c user.name=t commit -q -m "$1"
}

# set_base — point refs/remotes/origin/main at HEAD. resolve_base looks for
# exactly this ref, so a fixture that omits it exercises the skip.
set_base() {
    git -C "$WORK" update-ref refs/remotes/origin/main HEAD
}

# prose_page FILE [EXTRA] — 200 dash-free words, so a fixture's density is set
# by what EXTRA adds rather than by the file being too short to have one.
prose_page() {
    local file="$1" extra="${2:-}" i
    for ((i = 0; i < 25; i++)); do
        printf 'alpha beta gamma delta epsilon zeta eta theta\n' >>"$file"
    done
    [[ -n "$extra" ]] && printf '%s\n' "$extra" >>"$file"
    return 0
}

# run_gate NAME WANT [gha] — run the gate inside WORK against WORK/baseline.txt
# and compare its exit status. Every environment variable the gate reads is
# pinned rather than inherited, because each of them makes an assertion pass or
# fail on where the suite runs rather than on what the gate did:
#   GITHUB_ACTIONS      switches findings between `file:` and `::error`
#   EM_DASH_REQUIRE_BASE turns the no-base skip into a hard error, and every
#                       fixture repo here is deliberately without a base
# The second cost PR #1681 a red runner: the gate keyed on `CI` instead, which
# no caller here sets and the runner sets for everything, so 12 assertions went
# red on CI and green locally. Pin what the gate reads; a gate that reads an
# ambient variable cannot be tested where that variable is ambient.
run_gate() {
    local name="$1" want="$2" mode="${3:-plain}" got=0
    if [[ "$mode" == "gha" ]]; then
        (cd "$WORK" && env -u EM_DASH_REQUIRE_BASE GITHUB_ACTIONS=true "$GATE" --baseline "$WORK/baseline.txt") >"$WORK/gate.out" 2>&1 || got=$?
    else
        (cd "$WORK" && env -u GITHUB_ACTIONS -u EM_DASH_REQUIRE_BASE "$GATE" --baseline "$WORK/baseline.txt") >"$WORK/gate.out" 2>&1 || got=$?
    fi
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
    if grep -q "$pattern" "$WORK/gate.out"; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: no match for %s in:\n' "$name" "$pattern"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

# --- the injected defect, and the two exclusions that must stay green -------

new_repo
prose_page "$WORK/a.md" 'One thing — and another — and a third — and a fourth.'
git -C "$WORK" add -A
run_gate 'em-dashes injected into prose fail the gate' 1
expect_out 'the finding names the density and the rule' 'em-dash density'

new_repo
prose_page "$WORK/a.md"
cat >>"$WORK/a.md" <<'FENCE'
```sh
grep -o "—" f | wc -l   # — — — — —
```
FENCE
git -C "$WORK" add -A
run_gate 'control: the same dashes inside a code fence stay green' 0

new_repo
prose_page "$WORK/a.md"
printf '## Appendix A — Capacity Targets\n\n## 2.1. Tier 1 — GMC\n\n## Step 2 — the storage keys\n\n## Result — PASS\n' >>"$WORK/a.md"
git -C "$WORK" add -A
run_gate 'control: the same dashes as heading title separators stay green' 0

# --- the baseline ratchet --------------------------------------------------

new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '3 a.md\n' >"$WORK/baseline.txt"
git -C "$WORK" add -A
run_gate 'a file at its baseline ceiling is green' 0

new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '2 a.md\n' >"$WORK/baseline.txt"
git -C "$WORK" add -A
run_gate 'a file that gained an em-dash above its ceiling is caught' 1
expect_out 'the finding names the ceiling' 'baseline ceiling of 2'

new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '# only a comment\n\n9 other.md\n' >"$WORK/baseline.txt"
git -C "$WORK" add -A
run_gate 'a file with no ceiling is held to the rule' 1

new_repo
prose_page "$WORK/a.md" 'One — two.'
git -C "$WORK" add -A
run_gate 'two em-dashes on a page are punctuation, not a signature' 0

# --- the diff ratchet (Q742) -----------------------------------------------
#
# A ceiling is a whole-file total, so a file sitting under its entry carries
# slack, and two PRs can each spend the same slack on the same base. Every
# fixture here stays inside its ceiling, so the ceiling check cannot produce the
# verdict — which is what the no-base control measures rather than assumes.

new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '4 a.md\n' >"$WORK/baseline.txt"
commit_all base
set_base
printf 'And — five.\n' >>"$WORK/a.md"
run_gate 'a file over the rule may not spend its ceiling headroom' 1
expect_out 'the finding names the count at the base' 'up from 3 at the base revision'

new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '4 a.md\n' >"$WORK/baseline.txt"
commit_all base
printf 'And — five.\n' >>"$WORK/a.md"
run_gate 'control: with no base the ceiling alone passes the same tree' 0
expect_out 'the skipped ratchet is announced, not silent' 'diff ratchet skipped'

# Same tree, same missing base, with the caller opting in: the skip is the shape
# of the defect Q742 is about, so a job that arranged a base refuses rather than
# reporting the ceilings as the whole verdict.
new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '4 a.md\n' >"$WORK/baseline.txt"
commit_all base
printf 'And — five.\n' >>"$WORK/a.md"
req_rc=0
(cd "$WORK" && env -u GITHUB_ACTIONS EM_DASH_REQUIRE_BASE=1 "$GATE" --baseline "$WORK/baseline.txt") >"$WORK/gate.out" 2>&1 || req_rc=$?
if [[ "$req_rc" == 1 ]]; then
    printf 'ok   %s\n' 'EM_DASH_REQUIRE_BASE turns an unmeasurable ratchet into a failure'
else
    printf 'FAIL %s: want exit 1, got %s\n' 'EM_DASH_REQUIRE_BASE turns an unmeasurable ratchet into a failure' "$req_rc"
    fails=$((fails + 1))
fi
expect_out 'the failure names the checkout fault, not the prose' 'checkout fault, not a prose finding'
expect_out 'the failure points at the job rather than the docs' 'Fix the job'

# The regression that cost PR #1681 a red runner. A runner sets CI=true for
# every step, so a gate keyed on it refuses inside these fixtures, whose repos
# have no origin/main by construction. Nothing ambient may reach that branch.
new_repo
prose_page "$WORK/a.md" 'One — two.'
commit_all base
amb_rc=0
(cd "$WORK" && env -u GITHUB_ACTIONS -u EM_DASH_REQUIRE_BASE CI=true "$GATE" --baseline "$WORK/baseline.txt") >"$WORK/gate.out" 2>&1 || amb_rc=$?
if [[ "$amb_rc" == 0 ]]; then
    printf 'ok   %s\n' 'an ambient CI=true does not turn the skip into a failure'
else
    printf 'FAIL %s: want exit 0, got %s\n' 'an ambient CI=true does not turn the skip into a failure' "$amb_rc"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
fi
expect_out 'it degrades to the ceilings instead' 'diff ratchet skipped'

new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '4 a.md\n' >"$WORK/baseline.txt"
commit_all base
set_base
: >"$WORK/a.md"
prose_page "$WORK/a.md" 'One — two — three.'
run_gate 'a reduction is not a gain' 0

new_repo
prose_page "$WORK/a.md"
commit_all base
set_base
printf 'One — two — three.\n' >>"$WORK/a.md"
run_gate 'a file inside the rule may still gain em-dashes' 0

# The base is the branch point, not HEAD~1: a gain made in an earlier commit is
# still a gain when a later one lands on top of it.
new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '4 a.md\n' >"$WORK/baseline.txt"
commit_all base
set_base
printf 'And — five.\n' >>"$WORK/a.md"
commit_all 'the gain'
printf 'A dash-free line.\n' >>"$WORK/a.md"
commit_all 'a later commit that adds nothing'
run_gate 'the base is the merge-base, not the previous commit' 1

# The merge queue's candidate commit. Two branches each gain one em-dash inside
# the same ceiling; the candidate merging both is up two from the base it was
# built on. The ratchet reaches that base through the merge, which is the view
# Q743 put this workflow's gates in front of.
new_repo
printf 'Head — line.\n' >"$WORK/a.md"
prose_page "$WORK/a.md"
printf 'Tail — line.\n' >>"$WORK/a.md"
printf '4 a.md\n' >"$WORK/baseline.txt"
commit_all base
set_base
git -C "$WORK" switch -q -c pr1
awk 'NR == 1 { print "Head — line — one."; next } { print }' "$WORK/a.md" >"$WORK/a.new"
mv "$WORK/a.new" "$WORK/a.md"
commit_all 'pr1'
git -C "$WORK" switch -q -
git -C "$WORK" switch -q -c pr2
awk '/^Tail/ { print "Tail — line — two."; next } { print }' "$WORK/a.md" >"$WORK/a.new"
mv "$WORK/a.new" "$WORK/a.md"
commit_all 'pr2'
git -C "$WORK" switch -q -c candidate
git -C "$WORK" -c user.email=t@example.invalid -c user.name=t merge -q --no-edit pr1
run_gate "the merge queue's candidate is measured against the base it was built on" 1
expect_out 'the candidate finding names the joint gain' 'up from 2 at the base revision'

# --base names the revision directly, which is how a maintainer points the
# ratchet somewhere other than the merge-base. A revision that does not resolve
# is a caller error, not a finding, so it dies rather than degrading.
new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
printf '4 a.md\n' >"$WORK/baseline.txt"
commit_all base
set_base
printf 'And — five.\n' >>"$WORK/a.md"
commit_all 'the gain'
set_base
base_rc=0
(cd "$WORK" && env -u GITHUB_ACTIONS -u EM_DASH_REQUIRE_BASE "$GATE" --baseline "$WORK/baseline.txt" --base HEAD~1) >"$WORK/gate.out" 2>&1 || base_rc=$?
if [[ "$base_rc" == 1 ]]; then
    printf 'ok   %s\n' '--base overrides a merge-base that has moved past the gain'
else
    printf 'FAIL %s: want exit 1, got %s\n' '--base overrides a merge-base that has moved past the gain' "$base_rc"
    fails=$((fails + 1))
fi
expect_out 'the overridden base is the one reported against' 'up from 3 at the base revision'

bogus_rc=0
(cd "$WORK" && env -u GITHUB_ACTIONS -u EM_DASH_REQUIRE_BASE "$GATE" --baseline "$WORK/baseline.txt" --base no-such-rev) >"$WORK/gate.out" 2>&1 || bogus_rc=$?
if [[ "$bogus_rc" != 0 ]]; then
    printf 'ok   %s\n' 'a --base that does not resolve fails rather than degrading'
else
    printf 'FAIL %s: want a non-zero exit, got 0\n' 'a --base that does not resolve fails rather than degrading'
    fails=$((fails + 1))
fi
expect_out 'the failure names the unresolvable revision' 'no-such-rev'

# --- file selection --------------------------------------------------------

new_repo
prose_page "$WORK/tracked.md"
git -C "$WORK" add -A
prose_page "$WORK/untracked.md" 'One — two — three — four.'
run_gate 'an untracked, non-ignored file is counted (Q619)' 1

new_repo
prose_page "$WORK/tracked.md"
printf 'ignored.md\n' >"$WORK/.gitignore"
git -C "$WORK" add -A
prose_page "$WORK/ignored.md" 'One — two — three — four.'
run_gate 'a gitignored file is not counted' 0

new_repo
mkdir -p "$WORK/vendor/dep"
prose_page "$WORK/vendor/dep/README.md" 'One — two — three — four.'
prose_page "$WORK/root.md"
git -C "$WORK" add -A
run_gate 'a vendored tree is not counted' 0

new_repo
prose_page "$WORK/CLAUDE.md" 'One — two — three — four.'
ln -s CLAUDE.md "$WORK/AGENTS.md"
printf '4 CLAUDE.md\n' >"$WORK/baseline.txt"
git -C "$WORK" add -A
run_gate 'a symlinked doc is skipped, not counted twice' 0

# --- CI annotations --------------------------------------------------------

new_repo
prose_page "$WORK/a.md" 'One — two — three — four.'
git -C "$WORK" add -A
run_gate 'a finding under CI still fails the gate' 1 gha
expect_out 'the finding is a GitHub error annotation' '::error file=a.md::em-dash density'

if ((fails > 0)); then
    printf '\ncheck-em-dash-test: FAILED - %d assertion(s)\n' "$fails"
    exit 1
fi
printf '\ncheck-em-dash-test: ok\n'
