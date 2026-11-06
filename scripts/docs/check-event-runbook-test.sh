#!/usr/bin/env bash
#
# check-event-runbook-test.sh — assertions for check-event-runbook.sh.
#
# The Go checker's own cases live in devtools/docs/eventreasons/main_test.go and
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
GATE="$SCRIPT_DIR/check-event-runbook.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

AGC_SRC="$REPO_ROOT/cmd/agc"
API_SRC="$REPO_ROOT/api"
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
    "$GATE" "$AGC_SRC" "$API_SRC" "$RUNBOOK_DOC"

# An Event an operator meets in kubectl describe and cannot look up has no remedy.
awk '!/^\| `OrphanedWorkerRecovered` \| Warning \|/' "$RUNBOOK_DOC" > "$TMP/no-runbook.md"
expect_rc "an Event reason with no runbook entry fails" 1 -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/no-runbook.md"
expect_output "and says so" "no runbook entry" -- \
    "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/no-runbook.md"

# A source tree with no reasons in it looks exactly like a clean one.
mkdir -p "$TMP/empty-src"
expect_rc "a source tree emitting no reasons is an error" 2 -- \
    "$GATE" "$TMP/empty-src" "$API_SRC" "$RUNBOOK_DOC"

# Argument handling: 0 or 3, never a partial set.
expect_rc "one argument is rejected" 2 -- "$GATE" "$AGC_SRC"
expect_rc "two arguments are rejected" 2 -- "$GATE" "$AGC_SRC" "$API_SRC"
expect_rc "a missing doc is rejected" 2 -- "$GATE" "$AGC_SRC" "$API_SRC" "$TMP/nope.md"
expect_rc "a missing source dir is rejected" 2 -- \
    "$GATE" "$TMP/nope-src" "$API_SRC" "$RUNBOOK_DOC"

printf '\n'
if ((failures > 0)); then
    printf 'check-event-runbook-test: %d failure(s)\n' "$failures" >&2
    exit 1
fi
printf 'check-event-runbook-test: ok\n'
