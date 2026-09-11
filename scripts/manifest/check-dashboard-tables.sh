#!/usr/bin/env bash
#
# check-dashboard-tables.sh — compare the dashboard doc's panel tables to the
# shipped dashboard JSON (Q1091).
#
# Both sides are machine-readable and nothing compared them, so Q961 reconciled
# all four dashboards by hand: the tenant section numbered nine rows 1..7, 7, 8,
# and a Cross-tenant Throughput panel had escaped its table to render as literal
# pipe-separated text mid-paragraph. check-promql.sh parses panel expressions
# out of the JSON and never reads the prose; check-dashboard-render.sh is a
# screenshot gate. Neither instance was visible to either.
#
# The checking is [check-dashboard-tables.py](check-dashboard-tables.py); this
# script is the entry point that selects the files, so the gate map stays in
# scripts/. What it asserts, and why panel titles are deliberately out of scope,
# are in that program's docstring.
#
# Usage:
#   check-dashboard-tables.sh [DOC [DASHBOARD...]]
#
# With no arguments the shipped paths are used: the dashboard doc and all four
# dashboards. Exits 1 on any finding, and 2 when the doc holds no row marker at
# all, since the gate would otherwise pass by checking nothing.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

# Run from the root so every finding reads as the repo-relative path the reader
# opens.
repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

DOC="${1:-docs/operations/observability-dashboards.md}"
shift || true
if (($# > 0)); then
    dashboards=("$@")
else
    dashboards=(
        deploy/monitoring/grafana-dashboard-tenant.json
        deploy/monitoring/grafana-dashboard-platform.json
        deploy/monitoring/grafana-dashboard-budget.json
        deploy/monitoring/grafana-dashboard-security.json
    )
fi

# A file that is not there is a refusal, not a pass: the gate's subject would
# otherwise vanish with a rename and take the verdict green with it.
for f in "$DOC" "${dashboards[@]}"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-dashboard-tables: %s does not exist, so this gate would check nothing\n' "$f" >&2
        exit 2
    fi
done

require_cmd python3 "https://www.python.org/downloads/"
exec python3 "$SCRIPT_DIR/check-dashboard-tables.py" "$DOC" "${dashboards[@]}"
