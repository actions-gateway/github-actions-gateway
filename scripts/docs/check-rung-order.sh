#!/usr/bin/env bash
#
# check-rung-order.sh — reconcile the classic admission ladder's rung order in
# the design doc against the order Provisioner.Admit evaluates it in.
#
# The order is load-bearing rather than presentational: the rate rung is last so
# nothing refuses after the bucket has been charged, and Q977 documents a
# transient that exists only because the ceiling rung reserves before the rate
# rung refuses. A doc listing them the other way round describes a system with
# different failure modes from the one that ships.
#
# It drifted for real. 04-operational-flows.md listed Rate before Ceiling from
# Q717 until Q972 happened to edit that paragraph, and nothing reported it —
# prose and evaluation order are the two halves metrictiers and reasontiers pair
# for a metric's tier and a reason's tier, and nobody paired for this one.
#
# What the checker asserts, and why the code side is read from the AST rather
# than grepped, are in the package comment beside devtools/docs/rungorder.
#
# Usage:
#   check-rung-order.sh [ADMISSION_SRC FLOWS_DOC]
#
# With no arguments the two shipped paths are used. Exits 1 on a finding, 2 on a
# read it could not take.

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location rather than from the
# git root, which a test suite scopes to a throwaway tree with no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

ADMISSION_SRC="cmd/agc/internal/provisioner/admission.go"
FLOWS_DOC="docs/design/04-operational-flows.md"

if (($# == 2)); then
    ADMISSION_SRC="$1"
    FLOWS_DOC="$2"
elif (($# != 0)); then
    printf 'check-rung-order.sh: expected 0 or 2 arguments, got %d\n' "$#" >&2
    exit 2
fi

cd "$REPO_ROOT"

for f in "$ADMISSION_SRC" "$FLOWS_DOC"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-rung-order: %s does not exist\n' "$f" >&2
        exit 2
    fi
done

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/rungorder"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/rungorder)

"$bin" "$ADMISSION_SRC" "$FLOWS_DOC"
