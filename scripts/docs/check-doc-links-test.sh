#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-doc-links.sh — the Markdown link and anchor
# gate. What the parser sees is asserted in Go (devtools/docs/markdown,
# devtools/docs/doclinks); this suite covers the half that lives in shell —
# which files are selected, and what the gate does with what it finds.
#
# The first case is the defect the rewrite closed (Q612): a badge-wrapped link
# `[![x](img)](target)`, whose outer target the old awk collector never saw
# because its regex matched the inner image first. The plain-link control
# beside it points at the same missing target, so a red badge case cannot be
# credited to the target rather than the shape.
#
# Each case builds a throwaway git repo and asserts the gate's exit status,
# which is what `make check` and the doc-links workflow consume.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
GATE="$REPO_ROOT/scripts/docs/check-doc-links.sh"

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
}

# run_gate NAME WANT — run the gate inside WORK and compare its exit status.
run_gate() {
    local name="$1" want="$2" got=0
    ( cd "$WORK" && "$GATE" ) >"$WORK/gate.out" 2>&1 || got=$?
    if [[ "$got" == "$want" ]]; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$(( fails + 1 ))
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
    fails=$(( fails + 1 ))
}

# --- the defect the rewrite closed, and its control ------------------------

new_repo
printf '[![License](https://img.example/b.svg)](LICENSE)\n' >"$WORK/README.md"
git -C "$WORK" add -A
run_gate 'badge-wrapped link at a missing target is caught' 1
expect_out 'the badge finding names the outer target' 'README.md:1: dead link: LICENSE'

new_repo
printf '[license](LICENSE)\n' >"$WORK/README.md"
git -C "$WORK" add -A
run_gate 'control: the same missing target as a plain link is caught' 1

new_repo
printf '[![License](https://img.example/b.svg)](LICENSE)\n' >"$WORK/README.md"
printf 'Apache 2.0\n' >"$WORK/LICENSE"
git -C "$WORK" add -A
run_gate 'control: the badge target present is green' 0

# --- anchors ---------------------------------------------------------------

new_repo
printf '# The check gate\n\n[jump](#the-check-gate)\n' >"$WORK/a.md"
git -C "$WORK" add -A
run_gate 'anchor matching a heading slug is green' 0

new_repo
printf '# Overview\n\n[jump](#overview-typo)\n' >"$WORK/a.md"
git -C "$WORK" add -A
run_gate 'anchor matching no heading is caught' 1

# --- file selection --------------------------------------------------------

new_repo
printf '# Tracked\n' >"$WORK/tracked.md"
git -C "$WORK" add -A
printf '[gone](missing.md)\n' >"$WORK/untracked.md"
run_gate 'an untracked, non-ignored file is scanned (Q619)' 1

new_repo
printf '# Tracked\n' >"$WORK/tracked.md"
printf 'ignored.md\n' >"$WORK/.gitignore"
git -C "$WORK" add -A
printf '[gone](missing.md)\n' >"$WORK/ignored.md"
run_gate 'a gitignored file is not scanned' 0

new_repo
mkdir -p "$WORK/vendor/dep"
printf '[gone](missing.md)\n' >"$WORK/vendor/dep/README.md"
printf '# Root\n' >"$WORK/root.md"
git -C "$WORK" add -A
run_gate 'a vendored tree is not scanned' 0

new_repo
printf '# Target\n' >"$WORK/CLAUDE.md"
ln -s CLAUDE.md "$WORK/AGENTS.md"
git -C "$WORK" add -A
run_gate 'a symlinked doc is skipped, not scanned twice' 0

# --- CI annotations --------------------------------------------------------

new_repo
printf '[gone](missing.md)\n' >"$WORK/a.md"
git -C "$WORK" add -A
GITHUB_ACTIONS=true run_gate 'a finding under CI still fails the gate' 1
expect_out 'the finding is a GitHub error annotation' '::error file=a.md,line=1::dead link'

if (( fails > 0 )); then
    printf '\ncheck-doc-links-test: FAILED — %d assertion(s)\n' "$fails"
    exit 1
fi
printf '\ncheck-doc-links-test: ok\n'
