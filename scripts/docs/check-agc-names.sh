#!/usr/bin/env bash
#
# check-agc-names.sh — entry point for the AGC Deployment-name rules (Q1098).
#
# The logic is check-agc-names.py. This wrapper exists for the same reason
# check-queue-rules.sh does: every gate in this repo is a scripts/ file, which
# the Makefile recipe, the workflow step and gate-list.sh's derivation all key
# on, and it brings the gate under the shell linter and the errexit prologue
# check alongside its siblings.
#
# Usage: check-agc-names.sh
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/check-agc-names.py" "$@"
