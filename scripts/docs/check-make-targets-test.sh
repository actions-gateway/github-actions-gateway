#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-make-targets.sh — the named-but-absent make
# target gate (Q1012).
#
# The first pair is the injected defect and its control: a page naming a target
# no Makefile declares must go red, and the same page naming a real one must go
# green. A gate asserted only against a correct page has demonstrated nothing,
# because a checker that reads no Makefile at all passes it too — which is why
# the last case requires exit 2 when the target set comes back empty.
#
# The two cases the gate is easiest to get wrong are the ones that decide
# whether anybody keeps it. Prose is full of "make sure" and "make room", so the
# English cases must stay green; and `deploy` lives in api/Makefile rather than
# the root one, so the union case must stay green too. Getting either wrong
# turns the gate into a permanent unfixable failure.
#
# Each case builds a throwaway root holding its own Makefiles and page, and
# asserts the gate's exit status, which is what `make check` consumes.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/docs/check-make-targets.sh"

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

# new_root — start a throwaway root with a root Makefile declaring `check` and
# an api/Makefile declaring `deploy`, then read the page body from stdin. The
# split is the point: `deploy` is reachable only by unioning the two.
new_root() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    mkdir -p "$WORK/api"
    printf '.PHONY: check\ncheck:\n\t@true\n' >"$WORK/Makefile"
    printf '.PHONY: deploy\ndeploy:\n\t@true\n' >"$WORK/api/Makefile"
    cat >"$WORK/page.md"
}

# run_gate NAME WANT — run the gate over the fixture page and compare its exit
# status.
run_gate() {
    local name="$1" want="$2" got=0
    "$GATE" --root "$WORK" page.md >"$WORK/gate.out" 2>&1 || got=$?
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

new_root <<'MD'
Run `make check` before opening a PR.
MD
run_gate 'control: a target the root Makefile declares is green' 0
expect_out 'the green line counts what it checked' 'invocation(s) in'

new_root <<'MD'
Run `make next-task` to pick up the next item.
MD
run_gate 'a target no Makefile declares is caught' 1
expect_out 'the finding names the target' 'names no target in any Makefile'

# --- the union rule: a sub-Makefile's target is a real target --------------

new_root <<'MD'
Apply the CRDs with `make deploy`.
MD
run_gate 'a target declared only in api/Makefile is green' 0

new_root <<'MD'
Regenerate with `make -C api deploy`.
MD
run_gate 'a -C flag consumes its directory rather than reading it as the target' 0

# --- prose must not be a finding -------------------------------------------

new_root <<'MD'
Please make sure the worker is drained, and make room for the next one.
A misconfigured allowlist would make the worker and infra sets intersect.
MD
run_gate 'prose using "make" as an English verb is green' 0

new_root <<'MD'
The reconciler will `make the decision` on its own.
MD
run_gate 'a code span whose first word is make but names prose is still scanned' 1

# --- fenced blocks ----------------------------------------------------------

new_root <<'MD'
```bash
make check
```
MD
run_gate 'a fenced invocation of a real target is green' 0

new_root <<'MD'
```bash
make next-task
```
MD
run_gate 'a fenced invocation of an absent target is caught' 1

new_root <<'MD'
```bash
# so make that first, then run the real one
make check
```
MD
run_gate 'a comment inside a fence is not an invocation' 0

# --- the deliberate negative mention ---------------------------------------

new_root <<'MD'
There is no `make next-task`. <!-- make-targets-check: ignore -->
MD
run_gate 'a line carrying the ignore marker is skipped' 0

# --- the shape guard: an empty target set must not read as a pass ----------

WORK="$(mktemp -d)"
workdirs+=("$WORK")
cat >"$WORK/page.md" <<'MD'
Run `make next-task`.
MD
run_gate 'a root with no Makefile exits 2 rather than passing by checking nothing' 2
expect_out 'the shape failure names itself' 'would pass by checking nothing'

if ((fails > 0)); then
    printf '\n%d failure(s)\n' "$fails"
    exit 1
fi
printf '\nall checks passed\n'
