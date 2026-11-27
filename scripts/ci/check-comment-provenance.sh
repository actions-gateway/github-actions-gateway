#!/usr/bin/env bash
#
# check-comment-provenance.sh — entry point for check-comment-provenance.py,
# which fails a branch that adds a comment line carrying a backlog ID, a pull
# request number, a date, or change narration.
#
# Usage: check-comment-provenance.sh [--base REV] [--report] [--all]
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/check-comment-provenance.py" "$@"
