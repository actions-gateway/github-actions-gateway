#!/usr/bin/env bash
#
# check-promql-test.sh — assertions for check-promql.sh.
#
# The Go checker's own cases live in devtools/monitoring/promqlcheck/main_test.go
# and cover what each finding means. What only this suite can cover is the entry
# point: that the gate defaults to the three shipped paths, that it reports the
# real tree as clean, and that it fails on a defect rather than passing by
# checking nothing.
#
# Both directions are asserted throughout. A gate that stopped finding anything
# fails as silently as one that finds everything, and the shipped-tree case is
# the one that would go quiet.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check-promql.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

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

# The shipped tree is clean. This is the assertion that goes quiet if the gate
# ever stops checking anything, so it is paired with the failure cases below
# rather than standing alone.
expect_rc "shipped tree passes with no arguments" 0 -- "$GATE"
expect_output "names the file it checked" "prometheusrule.yaml" -- "$GATE"

# A malformed expression is caught.
cp "$REPO_ROOT/deploy/monitoring/prometheusrule.yaml" "$TMP/rule.yaml"
awk '{ if ($0 ~ /actions_gateway_active_sessions == 0/) print "            actions_gateway_active_sessions =="; else print }' \
    "$REPO_ROOT/deploy/monitoring/prometheusrule.yaml" > "$TMP/broken-expr.yaml"
expect_rc "malformed expr fails" 1 -- "$GATE" "$TMP/broken-expr.yaml" \
    "$REPO_ROOT/docs/operations/observability-alerting.md" "$REPO_ROOT/docs/operations/runbook.md"
expect_output "malformed expr names the parse failure" "does not parse" -- "$GATE" "$TMP/broken-expr.yaml" \
    "$REPO_ROOT/docs/operations/observability-alerting.md" "$REPO_ROOT/docs/operations/runbook.md"

# A rule the docs describe but the file does not ship — Q818's defect.
awk '/- alert: ActionsGatewayScaleSetJobsDeferred/ { skip = 1 }
     skip && /^        - alert: ActionsGatewayProxyConnectDenied/ { skip = 0 }
     !skip { print }' \
    "$REPO_ROOT/deploy/monitoring/prometheusrule.yaml" > "$TMP/missing-rule.yaml"
expect_output "a documented-but-unshipped rule is caught" "documented but not shipped" -- \
    "$GATE" "$TMP/missing-rule.yaml" "$REPO_ROOT/docs/operations/observability-alerting.md" \
    "$REPO_ROOT/docs/operations/runbook.md"

# Argument handling: 0 or 3, never 1 or 2.
expect_rc "one argument is rejected" 2 -- "$GATE" "$TMP/rule.yaml"
expect_rc "a missing file is rejected" 2 -- "$GATE" "$TMP/nope.yaml" \
    "$REPO_ROOT/docs/operations/observability-alerting.md" "$REPO_ROOT/docs/operations/runbook.md"

printf '\n'
if ((failures > 0)); then
    printf 'check-promql-test: %d failure(s)\n' "$failures" >&2
    exit 1
fi
printf 'check-promql-test: ok\n'
