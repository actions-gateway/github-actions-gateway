#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-doc-toc.sh — the hand-kept-index gate
# (Q865, widened to the tree by Q911).
#
# The first pair is the injected defect and its control: a TOC that has lost an
# entry must go red, and the same page with the entry restored must go green. A
# gate asserted only against a corrected document has demonstrated nothing,
# because a checker that reads no headings at all passes it too — which is why
# the three shape cases at the end require exit 2 rather than 0.
#
# The fence case is the one the gate is easiest to get wrong: a `# comment`
# inside a shell block is a heading to any line-oriented reading, and the
# operator pages are most of a thousand lines of `kubectl` blocks each. Counting those inflates the
# heading total by nine on the real page, so a naive gate reports a permanent,
# unfixable failure and gets switched off.
#
# Each case builds a throwaway page and asserts the gate's exit status, which is
# what `make check` and the doc-links workflow consume.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/docs/check-doc-toc.sh"

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

# run_gate NAME WANT [gha] — run the gate against the fixture page and compare
# its exit status. GITHUB_ACTIONS is pinned rather than inherited: it switches
# the findings between `file:` and `::error` annotations, and CI sets it, so an
# assertion on either format would otherwise pass or fail on where the suite
# runs rather than on what the gate did.
run_gate() {
    local name="$1" want="$2" mode="${3:-plain}" got=0
    if [[ "$mode" == "gha" ]]; then
        GITHUB_ACTIONS=true "$GATE" "$WORK/page.md" >"$WORK/gate.out" 2>&1 || got=$?
    else
        env -u GITHUB_ACTIONS "$GATE" "$WORK/page.md" >"$WORK/gate.out" 2>&1 || got=$?
    fi
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
# Upgrade

## Table of Contents

- [Alpha](#alpha)
  - [Beta](#beta)
- [Gamma](#gamma)

## Alpha

### Beta

## Gamma
MD
run_gate 'control: an index matching its headings is green' 0
expect_out 'the green line counts what it checked, and how deep' 'headings to level'

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha](#alpha)
  - [Beta](#beta)

## Alpha

### Beta

## Gamma
MD
run_gate 'a heading the index never mentions is caught' 1
expect_out 'the finding names the heading and the entry to add' 'is absent from the Table of Contents'

# --- order and nesting -----------------------------------------------------

new_page <<'MD'
# Upgrade

## Table of Contents

- [Gamma](#gamma)
- [Alpha](#alpha)

## Alpha

## Gamma
MD
run_gate 'entries that are all present but out of document order are caught' 1
expect_out 'the finding names the order' 'out of document order'

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha](#alpha)
- [Beta](#beta)

## Alpha

### Beta
MD
run_gate 'a level-3 entry left un-nested is caught' 1
expect_out 'the finding names the depth it wants' 'but its heading is level'

# --- entries with no heading -----------------------------------------------

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha](#alpha)
- [Ghost](#ghost)

## Alpha
MD
run_gate 'an entry naming no heading is caught' 1
expect_out 'the finding names the dangling anchor' 'names no heading this page indexes'

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha](#alpha)
- [Alpha again](#alpha)

## Alpha
MD
run_gate 'the same heading listed twice is caught' 1
expect_out 'the finding names the repeat' 'more than once'

# --- what the gate deliberately ignores ------------------------------------

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha](#alpha)

## Alpha

#### A procedure step

##### And a deeper one
MD
run_gate 'control: level-4 and deeper procedure steps are not indexed' 0

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha](#alpha)

## Alpha

```sh
# 1. Confirm both replicas are on the new image
kubectl get pods
# 2. Confirm the leader was re-elected
```
MD
run_gate 'control: a hash comment inside a code fence is not a heading' 0

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha with a `code span`](#alpha-with-a-code-span)

## Alpha with a `code span`
MD
run_gate 'control: entry text need not reproduce the heading verbatim' 0

# --- shapes that must refuse rather than pass by checking nothing ----------

new_page <<'MD'
# Upgrade

## Contents

- [Alpha](#alpha)

## Alpha
MD
run_gate 'a renamed index heading refuses instead of passing' 2
expect_out 'the refusal says there is no index' 'so there is no index to check'

new_page <<'MD'
# Upgrade

## Table of Contents

Nothing here yet.

## Alpha
MD
run_gate 'an index with no links refuses instead of passing' 2
expect_out 'the refusal says it would check nothing' 'would check nothing'

new_page <<'MD'
# Upgrade

## Table of Contents

- [Upgrade](#upgrade)
MD
run_gate 'a page with no indexable headings refuses instead of passing' 2

WORK="$(mktemp -d)"
workdirs+=("$WORK")
run_gate 'a page that is not there refuses instead of passing' 2
expect_out 'the refusal names the missing page' 'does not exist'

# --- how deep the page indexes is read off its own entries ------------------

new_page <<'MD'
# Page

## Table of Contents

- [Alpha](#alpha)
- [Gamma](#gamma)

## Alpha

### Beta

## Gamma
MD
run_gate 'control: a page whose index names only level-2 headings is held to level 2' 0
expect_out 'and the green line says so' 'headings to level 2'

new_page <<'MD'
# Page

## Table of Contents

- [Alpha](#alpha)
  - [Beta](#beta)
- [Gamma](#gamma)

## Alpha

### Beta

## Gamma

### Delta
MD
run_gate 'a page whose index already names a level-3 heading is held to level 3' 1
expect_out 'the level-3 heading it lost is named' 'heading "Delta" is absent'

new_page <<'MD'
# Page

## Table of Contents

- [Alpha](#alpha)

## Alpha

#### Deep
MD
run_gate 'control: a level-4 heading never raises the depth' 0

# --- several pages in one run ----------------------------------------------

# The gate checks a set, so a verdict has to survive the page it came from not
# being last. run_gate drives one fixture, so these two drive the gate directly.
WORK="$(mktemp -d)"
workdirs+=("$WORK")
cat >"$WORK/good.md" <<'MD'
# Good

## Table of Contents

- [Alpha](#alpha)

## Alpha
MD
cat >"$WORK/bad.md" <<'MD'
# Bad

## Table of Contents

- [Alpha](#alpha)

## Alpha

## Gamma
MD

multi_rc=0
env -u GITHUB_ACTIONS "$GATE" "$WORK/bad.md" "$WORK/good.md" >"$WORK/gate.out" 2>&1 || multi_rc=$?
die_if_killed 'a finding on the first of two pages still fails the set' "$multi_rc" 1
if [[ "$multi_rc" == 1 ]]; then
    printf 'ok   %s\n' 'a finding on the first of two pages still fails the set'
else
    printf 'FAIL %s: want exit 1, got %s\n' 'a finding on the first of two pages still fails the set' "$multi_rc"
    fails=$((fails + 1))
fi
expect_out 'the page that was clean is still reported' 'ok (good.md'

# A refusal outranks a finding: a page that was never checked must not be
# reported as a set that merely has findings in it.
refuse_rc=0
env -u GITHUB_ACTIONS "$GATE" "$WORK/bad.md" "$WORK/absent.md" >"$WORK/gate.out" 2>&1 || refuse_rc=$?
die_if_killed 'a page that is not there outranks a finding on one that is' "$refuse_rc" 2
if [[ "$refuse_rc" == 2 ]]; then
    printf 'ok   %s\n' 'a page that is not there outranks a finding on one that is'
else
    printf 'FAIL %s: want exit 2, got %s\n' 'a page that is not there outranks a finding on one that is' "$refuse_rc"
    fails=$((fails + 1))
fi

# --- the default selection --------------------------------------------------

# With no arguments the gate picks its own pages, which is the form `make check`
# runs and the one no assertion above reaches. Each case is a throwaway repo:
# the gate resolves its subject tree from `git rev-parse --show-toplevel`, so
# the working directory is what decides, while the checker still builds from
# this repo's devtools/.
new_repo() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q
    mkdir -p "$WORK/docs/operations"
}

run_default() {
    local name="$1" want="$2" got=0
    (cd "$WORK" && env -u GITHUB_ACTIONS "$GATE") >"$WORK/gate.out" 2>&1 || got=$?
    die_if_killed "$name" "$got" "$want"
    if [[ "$got" == "$want" ]]; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

new_repo
cat >"$WORK/docs/operations/indexed.md" <<'MD'
# Indexed

## Table of Contents

- [Alpha](#alpha)

## Alpha

## Gamma
MD
cat >"$WORK/docs/operations/plain.md" <<'MD'
# Plain

## Alpha
MD
run_default 'the default selection finds a page carrying an index' 1
expect_out 'and names the heading its index lost' 'heading "Gamma" is absent'
expect_out 'a page with no index is not reported at all' '^docs/operations/indexed.md'

new_repo
cat >"$WORK/docs/operations/plain.md" <<'MD'
# Plain

## Alpha
MD
run_default 'a tree where no page carries an index refuses instead of passing' 2
expect_out 'the refusal says it would check nothing' 'would check nothing'

# --- CI annotations --------------------------------------------------------

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha](#alpha)

## Alpha

## Gamma
MD
run_gate 'a finding under CI still fails the gate' 1 gha
expect_out 'the finding is a GitHub error annotation' '::error file=.*,line=[0-9]*::heading'

if ((fails > 0)); then
    printf '\ncheck-doc-toc-test: FAILED - %d assertion(s)\n' "$fails"
    exit 1
fi
printf '\ncheck-doc-toc-test: ok\n'
