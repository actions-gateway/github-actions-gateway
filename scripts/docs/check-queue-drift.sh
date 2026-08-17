#!/usr/bin/env bash
#
# check-queue-drift.sh — entry point for the table/store drift gate (Q889).
#
# The logic is check-queue-drift.py. This wrapper exists for the same reason
# its sibling's does: every gate in this repo is a scripts/ file, and the
# Makefile recipe, the workflow step and gate-list.sh's derivation all key on
# that. It also brings the gate under the shell linter and the errexit
# prologue check.
#
# Usage: check-queue-drift.sh
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/check-queue-drift.py" "$@"
