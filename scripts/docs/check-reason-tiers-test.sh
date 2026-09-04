#!/usr/bin/env bash
#
# check-reason-tiers-test.sh — assertions for check-reason-tiers.sh.
#
# The Go checker's own cases live in devtools/docs/reasontiers/main_test.go and
# cover what each finding means against a synthetic source tree. What only this
# suite can cover is the entry point: that the gate defaults to the four shipped
# paths, that it reports the real tree as clean, and that it fails on a defect
# seeded into the real ledger rather than passing by checking nothing.
#
# Both directions are asserted. A gate that stopped finding anything fails as
# silently as one that finds everything, and the shipped-tree case is the one
# that would go quiet: this gate's whole job is to be the thing that notices, so
# a green run has to be shown to mean something.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check-reason-tiers.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

AGC_SRC="$REPO_ROOT/cmd/agc"
API_SRC="$REPO_ROOT/api"
LEDGER_DOC="$REPO_ROOT/docs/operations/observability-metrics.md"
RUNBOOK_DOC="$REPO_ROOT/docs/operations/troubleshooting.md"

failures=0

ok() { printf 'ok   %s\n' "$1"; }
fail() {
    printf 'FAIL %s\n' "$1"
    failures=$((failures + 1))
}

# expect_rc NAME EXPECTED_RC -- command...
expect_rc() {
    local name="$1" want="$2" rc=0
    shift 3
    "$@" >/dev/null 2>&1 || rc=$?
    die_if_killed "$name" "$rc" "$want"
    if ((rc == want)); then
        ok "$name (rc=$rc)"
    else
        fail "$name: expected rc=$want, got rc=$rc"
    fi
}

# expect_output NAME NEEDLE -- command...
expect_output() {
    local name="$1" needle="$2" out
    shift 3
    out="$("$@" 2>&1 || true)"
    if [[ "$out" == *"$needle"* ]]; then
        ok "$name"
    else
        fail "$name: output did not contain '$needle'; got: $out"
    fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# The shipped tree is clean, with no arguments and with the paths named.
expect_rc "shipped tree passes with no arguments" 0 -- "$GATE"
expect_rc "shipped tree passes with the paths named" 0 -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$LEDGER_DOC" "$RUNBOOK_DOC"

# A condition reason dropped from the ledger is the case the gate exists for.
awk '!/^\| `RunnerGroupNotFound` \| Scale-set only/' "$LEDGER_DOC" > "$TMP/no-cond-row.md"
expect_rc "a condition reason with no ledger row fails" 1 -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/no-cond-row.md" "$RUNBOOK_DOC"
expect_output "and says which tier question is unanswered" "which acquisition tier reaches it" -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/no-cond-row.md" "$RUNBOOK_DOC"

# The same for an Event reason, whose inventory is derived differently — from the
# recorder call's argument rather than from a constant reference.
awk '!/^\| `AssignmentAbandoned` \| Scale-set only/' "$LEDGER_DOC" > "$TMP/no-event-row.md"
expect_output "an Event reason with no ledger row fails" "AssignmentAbandoned is a Event reason" -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/no-event-row.md" "$RUNBOOK_DOC"

# The other direction: a ledger row the source refutes. AssignmentAbandoned is
# recorded from the scale-set listener alone, so calling it classic-only
# contradicts the site the source actually has.
awk '{ if ($0 ~ /^\| `AssignmentAbandoned` \| Scale-set only \|/) \
         print "| `AssignmentAbandoned` | Classic only | Seeded defect. |"; \
       else print }' "$LEDGER_DOC" > "$TMP/contradicted.md"
expect_output "a single-tier claim the source refutes fails" "is emitted here, and the ledger calls it" -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/contradicted.md" "$RUNBOOK_DOC"

# An unknown tier value: the vocabulary is closed on purpose.
awk '{ if ($0 ~ /^\| `WorkerPodStuckPending` \| Both \|/) \
         print "| `WorkerPodStuckPending` | Mostly | Seeded defect. |"; \
       else print }' "$LEDGER_DOC" > "$TMP/bad-tier.md"
expect_output "an unknown tier value fails" "which is not one of" -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/bad-tier.md" "$RUNBOOK_DOC"

# An Event an operator meets in kubectl describe and cannot look up gets a tier
# and no remedy.
awk '!/^\| `OrphanedWorkerRecovered` \| Warning \|/' "$RUNBOOK_DOC" > "$TMP/no-runbook.md"
expect_output "an Event reason with no runbook entry fails" "no runbook entry" -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$LEDGER_DOC" "$TMP/no-runbook.md"

# The ledger is the gate's only input for the tier question, so its absence must
# be an error rather than a green run over zero rows.
awk '!/^### Condition reasons$/' "$LEDGER_DOC" > "$TMP/no-ledger.md"
expect_rc "a missing ledger section is an error, not a pass" 2 -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/no-ledger.md" "$RUNBOOK_DOC"

# A source tree with no reasons in it looks exactly like a clean one.
mkdir -p "$TMP/empty-src"
expect_rc "a source tree emitting no reasons is an error" 2 -- \
    "$GATE" "$TMP/empty-src" "$API_SRC" "$LEDGER_DOC" "$RUNBOOK_DOC"

# Argument handling: 0 or 4, never a partial set.
expect_rc "one argument is rejected" 2 -- "$GATE" "$AGC_SRC"
expect_rc "three arguments are rejected" 2 -- "$GATE" "$AGC_SRC" "$API_SRC" "$LEDGER_DOC"
expect_rc "a missing doc is rejected" 2 -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/nope.md" "$RUNBOOK_DOC"
expect_rc "a missing source dir is rejected" 2 -- \
    "$GATE" "$TMP/nope-src" "$API_SRC" "$LEDGER_DOC" "$RUNBOOK_DOC"

printf '\n'
if ((failures > 0)); then
    printf 'check-reason-tiers-test: %d failure(s)\n' "$failures" >&2
    exit 1
fi
printf 'check-reason-tiers-test: ok\n'
