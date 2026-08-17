#!/usr/bin/env bash
#
# pr-requeue-eligible.sh — this repo's entry point for the re-enqueue gate.
#
# The logic is pr-requeue-eligible.py, vendored byte-identical from the
# session-worker skill. Keeping it unmodified is the point: the shell original
# was written here, the skill ported it, and the port then closed defects the
# original still carried (Q694, Q814, Q828) while nothing reported the
# divergence. An unmodified vendor means the next upstream fix lands as a clean
# overwrite, and this wrapper is the one place a repo specific would live.
#
# There is nothing to supply today: the Python already defaults --state-dir to
# tmp/requeue and --gitattributes to .gitattributes, which is what this repo
# means. The wrapper exists so every call site and doc reference naming the .sh
# path keeps working, and so a future default has a home that is not the
# vendored file.
#
# Usage:
#   pr-requeue-eligible.sh --assess  <pr>   # before rebasing; records a verdict
#   pr-requeue-eligible.sh --confirm <pr>   # after CI is green; gates the enqueue
#
# Exit: 0 eligible, 1 not eligible (reason on stdout), 2 usage error or a probe
#       that could not run (reason on stderr).
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

exec python3 "$SCRIPT_DIR/pr-requeue-eligible.py" "$@"
