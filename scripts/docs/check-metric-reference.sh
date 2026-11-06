#!/usr/bin/env bash
#
# check-metric-reference.sh — hold the AGC's metrics to the metrics reference an
# operator reads and to the source that emits them (Q776). What each check asserts,
# and why the emission analysis reads the AST rather than the type graph, are in the
# package comment beside devtools/docs/metricreference.
#
# Usage:
#   check-metric-reference.sh [AGC_SRC METRICS_DOC]
#
# With no arguments the two shipped paths are used. Exits 1 on any finding.

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
METRICS_DOC="docs/operations/observability-metrics.md"

if (($# == 2)); then
    AGC_SRC="$1"
    METRICS_DOC="$2"
elif (($# != 0)); then
    printf 'check-metric-reference.sh: expected 0 or 2 arguments, got %d\n' "$#" >&2
    exit 2
fi

cd "$REPO_ROOT"

if [[ ! -d "$AGC_SRC" ]]; then
    printf 'check-metric-reference: %s is not a directory\n' "$AGC_SRC" >&2
    exit 2
fi
if [[ ! -f "$METRICS_DOC" ]]; then
    printf 'check-metric-reference: %s does not exist\n' "$METRICS_DOC" >&2
    exit 2
fi

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/metricreference"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/metricreference)

"$bin" "$AGC_SRC" "$METRICS_DOC"
