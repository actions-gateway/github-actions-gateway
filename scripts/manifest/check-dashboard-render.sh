#!/usr/bin/env bash
#
# check-dashboard-render.sh — entry point for the dashboard screenshot gate (Q868).
#
# The logic is check-dashboard-render.py. This wrapper exists for the reason
# check-queue-rules.sh does: every gate in this repo is a scripts/ file, and the
# Makefile recipe, the workflow step and gate-list.sh's own derivation all key on
# that. It also brings the gate under the shell linter and the errexit prologue
# check.
#
# Usage: check-dashboard-render.sh [--base REF] [--head REF]
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/check-dashboard-render.py" "$@"
