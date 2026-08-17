#!/usr/bin/env bash
#
# pr-mergeability-watch.sh — this repo's entry point for the mergeability watch.
#
# The logic is pr-mergeability-watch.py, vendored byte-identical from the
# session-orchestrator skill. Keeping it unmodified is the point: the shell
# original this replaces was written here, the skill ported it, and the port
# then fixed defects the original still carried — so the copies drifted with
# nothing reporting it (Q889). An unmodified vendor means the next upstream fix
# applies as a clean overwrite, and this wrapper is the one place a repo
# specific lives.
#
# It forwards every argument, so `--interval`, `--timeout`, `--trunk` and a
# `--gate` override all reach the Python; the defaults below are simply what
# this repo means when it says nothing.
#
# Usage: pr-mergeability-watch.sh <pr> [args...]
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/pr-mergeability-watch.py" --gate 'make check' "$@"
