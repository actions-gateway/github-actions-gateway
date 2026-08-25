#!/usr/bin/env bash
#
# check-promql.sh — validate the shipped PromQL against its docs (Q827, Q818, Q910).
#
# deploy/monitoring/prometheusrule.yaml is an appliable artifact: its README tells
# an operator to kubectl apply it. Nothing parsed its PromQL, so a malformed
# expression could merge and then silently never fire — the same failure the
# alerts exist to catch, arriving through the alerts. Nothing compared it to the
# docs either, and Q818 was a rule the docs described for weeks that no operator
# ever received.
#
# The two Grafana dashboards beside it are appliable the same way, and Q910 found
# their 62 panel queries parsed by nothing: manifest-validate runs `jq empty` over
# them, which asserts the JSON is well formed and accepts any query string inside
# it. Measured 2026-08-18 on this tree: `sum by ((((` as a panel expr passes
# `jq empty` and fails here.
#
# The checking is done by devtools/monitoring/promqlcheck, which parses the
# expressions with Prometheus's own promql parser; this script is the entry point
# that selects the files, so the gate map stays in scripts/. What each of its four
# checks asserts, and why this is a Go program rather than promtool, are in that
# program's package comment.
#
# Usage:
#   check-promql.sh [RULE_FILE ALERTING_DOC RUNBOOK [DASHBOARD...]]
#
# With no arguments the shipped paths are used: the rule file, its two docs, and
# both dashboards. Exits 1 on any finding.

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location rather than from the
# git root, which a test suite scopes to a throwaway tree with no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

RULE_FILE="deploy/monitoring/prometheusrule.yaml"
ALERTING_DOC="docs/operations/observability-alerting.md"
RUNBOOK="docs/operations/runbook.md"
DASHBOARDS=(
    "deploy/monitoring/grafana-dashboard-tenant.json"
    "deploy/monitoring/grafana-dashboard-platform.json"
    "deploy/monitoring/grafana-dashboard-budget.json"
)

if (($# >= 3)); then
    RULE_FILE="$1"
    ALERTING_DOC="$2"
    RUNBOOK="$3"
    shift 3
    DASHBOARDS=("$@")
elif (($# != 0)); then
    printf 'check-promql.sh: expected 0 arguments, or 3 plus any dashboards, got %d\n' "$#" >&2
    exit 2
fi

cd "$REPO_ROOT"

for f in "$RULE_FILE" "$ALERTING_DOC" "$RUNBOOK" "${DASHBOARDS[@]}"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-promql: %s does not exist\n' "$f" >&2
        exit 2
    fi
done

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/promqlcheck"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./monitoring/promqlcheck)

"$bin" "$RULE_FILE" "$ALERTING_DOC" "$RUNBOOK" "${DASHBOARDS[@]}"
