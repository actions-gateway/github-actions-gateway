#!/usr/bin/env bash
#
# check-test-cache-inputs.sh — entry point for the out-of-module test read gate (Q895).
#
# The logic is check-test-cache-inputs.py. This wrapper exists because every
# gate in this repo is a scripts/ file: the Makefile recipe, the workflow step
# and gate-lists.mk's own derivation all key on that, and a recipe reading
# `python3 <file>` satisfies none of them. It also brings the gate under the
# shell linter and the errexit prologue check, like its siblings.
#
# Usage: check-test-cache-inputs.sh [--list]
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/check-test-cache-inputs.py" "$@"
