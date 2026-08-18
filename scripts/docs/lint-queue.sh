#!/usr/bin/env bash
#
# lint-queue.sh — entry point for the backlog store's own format lint (Q889).
#
# The checker is the vendored queue.py, whose `lint` is a pure function of the
# store directory: frontmatter shape, rank shape, filename/id agreement, the
# 72-character title cap, and targets that no longer resolve. Its sibling
# check-queue-rules.sh carries the three rules that are functions of what the
# *branch* changed instead.
#
# This wrapper exists because every gate in this repo is a scripts/ file: the
# Makefile recipe, the workflow step and gate-list.sh's own derivation all key
# on that, and a recipe reading `python3 <file>` satisfies none of them.
#
# Read the count, not just the exit code. `lint` reports `0 item(s) OK` at exit
# 0 for a directory holding no items, which is a clean bill of health for a
# store it never read — the failure mode when --store is pointed somewhere
# wrong. The explicit --store below is what keeps that from being silent here.
#
# Usage: lint-queue.sh
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git rev-parse --show-toplevel)"

exec python3 "$SCRIPT_DIR/queue.py" --store "$REPO_ROOT/docs/queue" lint "$@"
