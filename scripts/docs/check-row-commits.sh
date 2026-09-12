#!/usr/bin/env bash
#
# check-row-commits.sh — entry point for the row-deletion commit rule (Q1100).
#
# The logic is check-row-commits.py. This wrapper exists for the same reason
# check-agc-names.sh does: every gate in this repo is a scripts/ file, which the
# Makefile recipe, the workflow step and gate-list.sh's derivation all key on,
# and it brings the gate under the shell linter and the errexit prologue check
# alongside its siblings.
#
# Usage: check-row-commits.sh
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/check-row-commits.py" "$@"
