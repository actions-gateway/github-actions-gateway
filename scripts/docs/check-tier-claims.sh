#!/usr/bin/env bash
#
# check-tier-claims.sh — entry point for the prose tier-claim stamps (Q848).
#
# The logic is check-tier-claims.py. This wrapper exists for the same reason
# check-queue-rules.sh does: every gate in this repo is a scripts/ file, which
# the Makefile recipe, the workflow step and gate-list.sh's derivation all key
# on, and it brings the gate under the shell linter and the errexit prologue
# check alongside its siblings.
#
# Usage: check-tier-claims.sh [--write] [page.md...]
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/check-tier-claims.py" "$@"
