#!/usr/bin/env bash
#
# next-task.sh — print a kickoff prompt for the top ready item in the backlog.
#
# Usage:
#   claude -n "$(scripts/docs/next-task.sh --title)" "$(scripts/docs/next-task.sh)"
#     -> session named "QN: <title>", prompted with the full kickoff
#   scripts/docs/next-task.sh [--title]   # just print
#
# Naming the session after the Q-ID keeps `claude --resume` history and
# after-the-fact metrics readable. The session still re-verifies the pick
# (open PRs, blockers); if the item turns out to be in flight, it takes the
# next one (rename with /rename if that happens).
#
# The logic is `queue.py next`, which reads docs/queue/ rather than the table
# this script used to parse with awk (Q889). The wrapper stays because the
# invocation above is what the docs teach and what muscle memory types.
# It no longer accepts a file path: the backlog is a directory, and
# `queue.py --store` is how you point at a different one.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/queue.py" next "$@"
