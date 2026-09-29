#!/usr/bin/env bash
#
# check-webhook-versions.sh — entry point for the Q1068 webhook-versions gate.
#
# The logic is check-webhook-versions.py. This wrapper exists for the reason
# check-dashboard-render.sh's does: every gate here is a scripts/ file, and the
# Makefile recipe, the workflow step and gate-list.sh's derivation all key on
# that. It also brings the gate under the shell linter and the errexit prologue
# check.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$(git rev-parse --show-toplevel)"

exec python3 "$SCRIPT_DIR/check-webhook-versions.py" "$@"
