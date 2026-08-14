#!/usr/bin/env bash
#
# check-reason-tiers.sh — reconcile the condition reasons and Event reasons the
# AGC emits against the acquisition-tier ledger an operator reads (Q850).
#
# The sibling of check-metric-tiers.sh, which did this for the 53 actions_gateway_*
# series (Q776). Metrics were one of three signal surfaces an operator reads, and
# the completeness claim that walk earned covered only that one: a capability
# reaching just the classic tier is as invisible in a condition reason or a
# Kubernetes Event as it is in a counter, and neither was gated.
#
# This one inverts the same obligation for the other two, and adds the check the
# metric side has no need of: an Event reason must also have a runbook entry, so
# an operator who meets it in `kubectl describe` gets a remedy and not just a
# tier. What each check asserts, and why the reason argument's index is read off
# the callee's declaration rather than tabulated, are in the package comment
# beside devtools/docs/reasontiers.
#
# Usage:
#   check-reason-tiers.sh [AGC_SRC API_SRC LEDGER_DOC RUNBOOK_DOC]
#
# With no arguments the four shipped paths are used. Exits 1 on any finding.

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location rather than from the
# git root, which a test suite scopes to a throwaway tree with no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

AGC_SRC="cmd/agc"
API_SRC="api"
LEDGER_DOC="docs/operations/observability-metrics.md"
RUNBOOK_DOC="docs/operations/troubleshooting.md"

if (($# == 4)); then
    AGC_SRC="$1"
    API_SRC="$2"
    LEDGER_DOC="$3"
    RUNBOOK_DOC="$4"
elif (($# != 0)); then
    printf 'check-reason-tiers.sh: expected 0 or 4 arguments, got %d\n' "$#" >&2
    exit 2
fi

cd "$REPO_ROOT"

for d in "$AGC_SRC" "$API_SRC"; do
    if [[ ! -d "$d" ]]; then
        printf 'check-reason-tiers: %s is not a directory\n' "$d" >&2
        exit 2
    fi
done
for f in "$LEDGER_DOC" "$RUNBOOK_DOC"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-reason-tiers: %s does not exist\n' "$f" >&2
        exit 2
    fi
done

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/reasontiers"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/reasontiers)

"$bin" "$AGC_SRC" "$API_SRC" "$LEDGER_DOC" "$RUNBOOK_DOC"
