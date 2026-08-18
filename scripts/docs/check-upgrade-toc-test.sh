#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-upgrade-toc.sh — the upgrade.md index gate
# (Q865).
#
# The first pair is the injected defect and its control: a TOC that has lost an
# entry must go red, and the same page with the entry restored must go green. A
# gate asserted only against a corrected document has demonstrated nothing,
# because a checker that reads no headings at all passes it too — which is why
# the three shape cases at the end require exit 2 rather than 0.
#
# The fence case is the one the gate is easiest to get wrong: a `# comment`
# inside a shell block is a heading to any line-oriented reading, and upgrade.md
# is most of a thousand lines of `kubectl` blocks. Counting those inflates the
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
GATE="$REPO_ROOT/scripts/docs/check-upgrade-toc.sh"

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
    cat >"$WORK/upgrade.md"
}

# run_gate NAME WANT [gha] — run the gate against the fixture page and compare
# its exit status. GITHUB_ACTIONS is pinned rather than inherited: it switches
# the findings between `file:` and `::error` annotations, and CI sets it, so an
# assertion on either format would otherwise pass or fail on where the suite
# runs rather than on what the gate did.
run_gate() {
    local name="$1" want="$2" mode="${3:-plain}" got=0
    if [[ "$mode" == "gha" ]]; then
        GITHUB_ACTIONS=true "$GATE" "$WORK/upgrade.md" >"$WORK/gate.out" 2>&1 || got=$?
    else
        env -u GITHUB_ACTIONS "$GATE" "$WORK/upgrade.md" >"$WORK/gate.out" 2>&1 || got=$?
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
expect_out 'the green line counts what it checked' 'headings indexed by'

new_page <<'MD'
# Upgrade

## Table of Contents

- [Alpha](#alpha)
- [Gamma](#gamma)

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
expect_out 'the finding names the dangling anchor' 'names no level-2 or level-3 heading'

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
    printf '\ncheck-upgrade-toc-test: FAILED - %d assertion(s)\n' "$fails"
    exit 1
fi
printf '\ncheck-upgrade-toc-test: ok\n'
