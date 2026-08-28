#!/usr/bin/env bash
#
# check-registry-mirror-wiring.sh — entry point for the Q408 registry-mirror
# wiring gate.
#
# The logic is check-registry-mirror-wiring.py. This wrapper exists for the
# reason check-dashboard-render.sh's does: every gate here is a scripts/ file,
# and the Makefile recipe, the workflow step and gate-list.sh's derivation all
# key on that. It also brings the gate under the shell linter and the errexit
# prologue check.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$(git rev-parse --show-toplevel)"

exec python3 "$SCRIPT_DIR/check-registry-mirror-wiring.py" "$@"
