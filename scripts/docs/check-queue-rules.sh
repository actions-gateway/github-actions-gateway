#!/usr/bin/env bash
#
# check-queue-rules.sh — entry point for the backlog store rules (Q889).
#
# The logic is check-queue-rules.py. This wrapper exists because every gate in
# this repo is a scripts/ file: the Makefile recipe, the workflow step and
# gate-list.sh's own derivation all key on that, and a recipe reading
# `python3 <file>` satisfies none of them. It also brings the gate under the
# shell linter and the errexit prologue check, like its siblings.
#
# Usage: check-queue-rules.sh
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/check-queue-rules.py" "$@"
