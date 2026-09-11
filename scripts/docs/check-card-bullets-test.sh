#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-card-bullets.sh — the card-bullet column
# gate (Q711).
#
# The checker's own rules are tested in Go, beside the code that holds them
# (devtools/docs/cardbullets/main_test.go). What only this layer can answer is
# the wrapper's: that the page arguments reach the checker, that the two pages
# the repo ships are green as they stand, and that a page which is not there is
# a refusal rather than a pass — the shape a rename would otherwise take the
# verdict green with.
#
# The first pair is the injected defect and its control: a bullet lengthened
# past its column must go red, and the same page with it restored must go
# green. A gate asserted only against a compliant document has demonstrated
# nothing, because a checker that measures no bullet at all passes it too.
#
# Each case asserts the gate's exit status, which is what `make check`, `make
# docs-gates` and the doc-links workflow consume.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/docs/check-card-bullets.sh"

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

# new_page — start a throwaway directory and read the page body from stdin.
new_page() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    cat >"$WORK/page.md"
}

# run_gate NAME WANT [args...] — run the gate and compare its exit status.
# GITHUB_ACTIONS is unset rather than inherited: it switches the findings
# between `file:` and `::error` annotations, so an assertion on either format
# would otherwise pass or fail on where the suite runs.
run_gate() {
    local name="$1" want="$2" got=0
    shift 2
    env -u GITHUB_ACTIONS "$GATE" "$@" >"$WORK/gate.out" 2>&1 || got=$?
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

# --- the injected defect, and its control ----------------------------------

new_page <<'MD'
# Page

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   __A card__

    ---

    Its lead-in:

    - This bullet is written well past the fifty character column budget

</div>
</div>
MD
run_gate "a bullet past its column budget fails" 1 "$WORK/page.md"
expect_out "the finding names the column it overran" "50-character column"

new_page <<'MD'
# Page

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   __A card__

    ---

    Its lead-in:

    - Short enough

</div>
</div>
MD
run_gate "the same page inside its budget passes" 0 "$WORK/page.md"

# --- the shapes that must refuse rather than pass by checking nothing ------

new_page <<'MD'
# Page

- An ordinary bullet, in no card grid at all.
MD
run_gate "a page with no card grid refuses" 2 "$WORK/page.md"
expect_out "the refusal says the gate would check nothing" "check nothing"

new_page </dev/null
rm -f "$WORK/page.md"
run_gate "a page that is not there refuses" 2 "$WORK/page.md"
expect_out "the refusal names the missing page" "does not exist"

# --- the pages the repo actually ships -------------------------------------

new_page </dev/null
run_gate "the shipped pages are green" 0

if ((fails > 0)); then
    printf '\ncheck-card-bullets-test: %d failure(s)\n' "$fails"
    exit 1
fi
printf '\ncheck-card-bullets-test: all cases passed\n'
