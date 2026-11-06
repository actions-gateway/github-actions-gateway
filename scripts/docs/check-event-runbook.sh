#!/usr/bin/env bash
#
# check-event-runbook.sh — hold every Event reason the AGC emits to its runbook
# entry (Q850). An Event is met in `kubectl describe`, where a reason with no remedy
# is no help. What the checks assert is in the package comment beside
# devtools/docs/eventreasons.
#
# Usage:
#   check-event-runbook.sh [AGC_SRC API_SRC RUNBOOK_DOC]
#
# With no arguments the three shipped paths are used. Exits 1 on any finding.

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
RUNBOOK_DOC="docs/operations/troubleshooting.md"

if (($# == 3)); then
    AGC_SRC="$1"
    API_SRC="$2"
    RUNBOOK_DOC="$3"
elif (($# != 0)); then
    printf 'check-event-runbook.sh: expected 0 or 3 arguments, got %d\n' "$#" >&2
    exit 2
fi

cd "$REPO_ROOT"

for d in "$AGC_SRC" "$API_SRC"; do
    if [[ ! -d "$d" ]]; then
        printf 'check-event-runbook: %s is not a directory\n' "$d" >&2
        exit 2
    fi
done
if [[ ! -f "$RUNBOOK_DOC" ]]; then
    printf 'check-event-runbook: %s does not exist\n' "$RUNBOOK_DOC" >&2
    exit 2
fi

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/eventreasons"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/eventreasons)

"$bin" "$AGC_SRC" "$API_SRC" "$RUNBOOK_DOC"
