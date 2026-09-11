#!/usr/bin/env bash
#
# Unit tests for scripts/manifest/check-dashboard-tables.sh — the gate holding
# observability-dashboards.md's panel tables to the shipped dashboard JSON
# (Q1091).
#
# The first pair is the injected defect and its control: each of Q961's two real
# instances, reproduced as a fixture, must go red, and the same fixture repaired
# must go green. A gate asserted only against a reconciled page has demonstrated
# nothing, because a checker that reads no row at all passes it too — which is
# why the shape cases at the end require exit 2 rather than 0.
#
# The panel-title case is the control for what the gate ignores on purpose: the
# doc shortens seven labels, so a fixture whose panel titles disagree entirely
# must still pass. Without it the gate could silently tighten into a rewrite of
# the page.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/manifest/check-dashboard-tables.sh"

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

# new_case — start a throwaway directory holding a two-row tenant dashboard and
# read the doc body from stdin. Every case mutates one side of this pair, so a
# case that stops discriminating shows up as two fixtures that are equal.
new_case() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    cat >"$WORK/doc.md"
    cat >"$WORK/grafana-dashboard-tenant.json" <<'JSON'
{
  "panels": [
    {"type": "row", "title": "Gateway Health"},
    {"type": "timeseries", "title": "Active sessions"},
    {"type": "timeseries", "title": "Jobs acquired/min"},
    {"type": "row", "title": "Egress Proxy"},
    {"type": "timeseries", "title": "Active CONNECT tunnels"}
  ]
}
JSON
}

# run_gate NAME WANT — run the gate over the fixture pair and compare its exit
# status. GITHUB_ACTIONS is unset rather than inherited: it switches the
# findings between `file:` and `::error` annotations, and CI sets it, so an
# assertion on either format would otherwise pass or fail on where the suite
# runs rather than on what the gate did.
run_gate() {
    local name="$1" want="$2" got=0
    env -u GITHUB_ACTIONS "$GATE" "$WORK/doc.md" "$WORK/grafana-dashboard-tenant.json" \
        >"$WORK/gate.out" 2>&1 || got=$?
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

# --- the control: a reconciled page ----------------------------------------

new_case <<'MD'
## Tenant dashboard

**Row 1 — Gateway Health**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `a` | Stat |
| Jobs acquired/min | `b` | Time series |

**Row 2 — Egress Proxy**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active CONNECT tunnels | `c` | Time series |
MD
run_gate "a reconciled page passes" 0

# --- Q961's first instance: a panel that escaped its table -----------------

new_case <<'MD'
## Tenant dashboard

**Row 1 — Gateway Health**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `a` | Stat |

Jobs acquired/min | `b` | Time series

**Row 2 — Egress Proxy**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active CONNECT tunnels | `c` | Time series |
MD
run_gate "a panel that escaped its table fails" 1
expect_out "the finding names the count on both sides" "puts 2 panel(s)"

# --- Q961's second instance: a duplicate row number ------------------------

new_case <<'MD'
## Tenant dashboard

**Row 1 — Gateway Health**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `a` | Stat |
| Jobs acquired/min | `b` | Time series |

**Row 1 — Egress Proxy**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active CONNECT tunnels | `c` | Time series |
MD
run_gate "a repeated row number fails" 1
expect_out "the finding names the numbering" "gap or a repeat"

# --- row titles and order --------------------------------------------------

new_case <<'MD'
## Tenant dashboard

**Row 1 — Gateway Heath**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `a` | Stat |
| Jobs acquired/min | `b` | Time series |

**Row 2 — Egress Proxy**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active CONNECT tunnels | `c` | Time series |
MD
run_gate "a row title the dashboard does not use fails" 1
expect_out "the finding quotes both titles" "Gateway Heath"

new_case <<'MD'
## Tenant dashboard

**Row 1 — Egress Proxy**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active CONNECT tunnels | `c` | Time series |

**Row 2 — Gateway Health**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `a` | Stat |
| Jobs acquired/min | `b` | Time series |
MD
run_gate "rows documented out of dashboard order fail" 1

new_case <<'MD'
## Tenant dashboard

**Row 1 — Gateway Health**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `a` | Stat |
| Jobs acquired/min | `b` | Time series |
MD
run_gate "a row the doc never documents fails" 1
expect_out "the finding names the undocumented row" "does not document"

# --- the title carried outside the bold, which reads as prose --------------

new_case <<'MD'
## Tenant dashboard

**Row 1 — Gateway Health**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `a` | Stat |
| Jobs acquired/min | `b` | Time series |

**Row 2 — Egress** Proxy

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active CONNECT tunnels | `c` | Time series |
MD
run_gate "a title half outside the bold fails" 1
expect_out "the finding says where the rest of the title went" "outside the bold"

# --- the control for what the gate ignores on purpose ----------------------

new_case <<'MD'
## Tenant dashboard

**Row 1 — Gateway Health**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Sessions | `a` | Stat |
| Acquisition rate | `b` | Time series |

**Row 2 — Egress Proxy**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Tunnels | `c` | Time series |
MD
run_gate "panel titles that differ from the dashboard's still pass" 0

# --- a colon separator, which the security section ships -------------------

new_case <<'MD'
## Tenant dashboard

**Row 1: Gateway Health**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `a` | Stat |
| Jobs acquired/min | `b` | Time series |

**Row 2: Egress Proxy**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active CONNECT tunnels | `c` | Time series |
MD
run_gate "a colon separator reads the same as an em-dash" 0

# --- the shapes that must refuse rather than pass by checking nothing ------

new_case <<'MD'
## Tenant dashboard

No rows documented here at all.
MD
run_gate "a doc with no row marker refuses" 2
expect_out "the refusal says the gate would check nothing" "check nothing"

new_case <<'MD'
## Platform dashboard

**Row 1 — Fleet Overview**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Managed gateways | `a` | Stat |
MD
run_gate "a dashboard with no section of its own refuses" 2
expect_out "the refusal names the missing section" "has no"

new_case </dev/null
rm -f "$WORK/doc.md"
run_gate "a doc that is not there refuses" 2
expect_out "the refusal names the missing file" "does not exist"

# --- the files the repo actually ships -------------------------------------

shipped=0
env -u GITHUB_ACTIONS "$GATE" >"$WORK/gate.out" 2>&1 || shipped=$?
die_if_killed "the shipped doc and dashboards agree" "$shipped" 0
if ((shipped == 0)); then
    printf 'ok   the shipped doc and dashboards agree\n'
else
    printf 'FAIL the shipped doc and dashboards agree: want exit 0, got %s\n' "$shipped"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
fi

if ((fails > 0)); then
    printf '\ncheck-dashboard-tables-test: %d failure(s)\n' "$fails"
    exit 1
fi
printf '\ncheck-dashboard-tables-test: all cases passed\n'
