#!/usr/bin/env bash
#
# check-metric-tiers.sh — reconcile the AGC's metric inventory against the
# acquisition-tier ledger an operator reads (Q776).
#
# Capability parity between the classic and scale-set acquisition tiers rested on
# a single seam walk, and that walk went stale four times: Q683, Q691, Q713 and
# Q844 each arrived classic-only from birth, after parity had been declared, with
# nothing re-walking it. The tier badge on docs/features.md is one-directional by
# construction — it fails a badge that outlived its gap, never a gap nobody
# badged — so the case that actually recurs had no gate at all.
#
# This one inverts the obligation: every actions_gateway_* series the AGC defines
# must carry a tier in the ledger, so a metric cannot reach an operator without
# someone answering which tier emits it. What each of its six checks asserts, and
# why the emission analysis reads the AST rather than the type graph, are in the
# package comment beside devtools/docs/metrictiers.
#
# Usage:
#   check-metric-tiers.sh [AGC_SRC METRICS_DOC PARITY_DOC]
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
METRICS_DOC="docs/operations/observability-metrics.md"
PARITY_DOC="docs/plan/v2-ga.md"

if (($# == 3)); then
    AGC_SRC="$1"
    METRICS_DOC="$2"
    PARITY_DOC="$3"
elif (($# != 0)); then
    printf 'check-metric-tiers.sh: expected 0 or 3 arguments, got %d\n' "$#" >&2
    exit 2
fi

cd "$REPO_ROOT"

if [[ ! -d "$AGC_SRC" ]]; then
    printf 'check-metric-tiers: %s is not a directory\n' "$AGC_SRC" >&2
    exit 2
fi
for f in "$METRICS_DOC" "$PARITY_DOC"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-metric-tiers: %s does not exist\n' "$f" >&2
        exit 2
    fi
done

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/metrictiers"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/metrictiers)

"$bin" "$AGC_SRC" "$METRICS_DOC" "$PARITY_DOC"
