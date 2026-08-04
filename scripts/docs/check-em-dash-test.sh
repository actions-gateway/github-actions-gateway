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
# and compare its exit status. GITHUB_ACTIONS is pinned rather than inherited:
# it switches the findings between `file:` and `::error` annotations, and CI
# sets it, so an assertion on either format would otherwise pass or fail on
# where the suite runs rather than on what the gate did.
run_gate() {
    local name="$1" want="$2" mode="${3:-plain}" got=0
    if [[ "$mode" == "gha" ]]; then
        (cd "$WORK" && GITHUB_ACTIONS=true "$GATE" --baseline "$WORK/baseline.txt") >"$WORK/gate.out" 2>&1 || got=$?
    else
        (cd "$WORK" && env -u GITHUB_ACTIONS "$GATE" --baseline "$WORK/baseline.txt") >"$WORK/gate.out" 2>&1 || got=$?
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
