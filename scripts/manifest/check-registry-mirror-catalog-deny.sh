#!/usr/bin/env bash
#
# check-registry-mirror-catalog-deny.sh — entry point for the Q1022 catalog-deny
# gate.
#
# The logic is check-registry-mirror-catalog-deny.py. This wrapper exists for the
# reason check-registry-mirror-wiring.sh's does: every gate here is a scripts/
# file, and the Makefile recipe, the workflow step and gate-list.sh's derivation
# all key on that.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$(git rev-parse --show-toplevel)"

exec python3 "$SCRIPT_DIR/check-registry-mirror-catalog-deny.py" "$@"
