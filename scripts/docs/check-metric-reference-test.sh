#!/usr/bin/env bash
#
# check-metric-reference-test.sh — assertions for check-metric-reference.sh.
#
# The Go checker's own cases live in devtools/docs/metricreference/main_test.go and
# cover what each of its checks means, against a synthetic source tree.
# What only this suite can cover is the entry point: that the gate defaults to
# the three shipped paths, that it reports the real tree as clean, and that it
# fails on a defect seeded into the real ledger rather than passing by checking
# nothing.
#
# Both directions are asserted. A gate that stopped finding anything fails as
# silently as one that finds everything, and the shipped-tree case is the one
# that would go quiet: this gate's whole job is to be the thing that notices, so
# a green run has to be shown to mean something.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check-metric-reference.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

METRICS_DOC="$REPO_ROOT/docs/operations/observability-metrics.md"
AGC_SRC="$REPO_ROOT/cmd/agc"

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
expect_rc "shipped tree passes with the paths named" 0 -- "$GATE" "$AGC_SRC" "$METRICS_DOC"

# A metric an operator can scrape and cannot look up is the case the gate exists for.
awk '!/actions_gateway_scaleset_session_open/' "$METRICS_DOC" > "$TMP/no-entry.md"
expect_rc "a metric with no reference entry fails" 1 -- "$GATE" "$AGC_SRC" "$TMP/no-entry.md"
expect_output "and names the metric" "actions_gateway_scaleset_session_open (defined in" -- \
    "$GATE" "$AGC_SRC" "$TMP/no-entry.md"

# A source tree with no metrics in it looks exactly like a clean one, so it must
# refuse rather than report green.
mkdir -p "$TMP/empty-src"
expect_rc "a source tree defining no metrics is an error" 2 -- \
    "$GATE" "$TMP/empty-src" "$METRICS_DOC"

# Argument handling: 0 or 2, never 1.
expect_rc "one argument is rejected" 2 -- "$GATE" "$AGC_SRC"
expect_rc "a missing doc is rejected" 2 -- "$GATE" "$AGC_SRC" "$TMP/nope.md"
expect_rc "a missing source dir is rejected" 2 -- "$GATE" "$TMP/nope-src" "$METRICS_DOC"

printf '\n'
if ((failures > 0)); then
    printf 'check-metric-reference-test: %d failure(s)\n' "$failures" >&2
    exit 1
fi
printf 'check-metric-reference-test: ok\n'
