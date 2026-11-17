#!/usr/bin/env bash
#
# check-chart-values-docs.sh — entry point for the chart values-docs gate.
#
# The logic is check-chart-values-docs.py. This wrapper exists for the reason
# check-webhook-versions.sh's does: the Makefile recipe, the workflow step and
# gate-list.sh's derivation key on a scripts/ file, and it brings the gate under
# the shell linter and the errexit prologue check.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$(git rev-parse --show-toplevel)"

exec python3 "$SCRIPT_DIR/check-chart-values-docs.py" "$@"
