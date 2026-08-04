#!/usr/bin/env bash
#
# backlog-metrics.sh — replay the backlog file's git history into per-item
# events and summary flow metrics.
#
# The replay is done by devtools/docs/backlogmetrics, a Go program reading the
# rows through a real Markdown parser (Q614); this script is the entry point
# that runs the git log and points the reporter at the file, so the gate map
# stays in scripts/. What the replay counts, and how removal reasons are
# derived, is documented in that program's package comment.
#
# This script only reads, and stays offline; the authoritative allocation count
# is the ref namespace (`git ls-remote origin 'refs/queue-ids/*' | wc -l`).
#
# Usage:
#   backlog-metrics.sh [--events] [path/to/STATUS.md]
#
# Default: summary (throughput, cycle time, arrival rate, prune ratio, aging
# WIP). --events: TSV event stream (id, filed date, removed date, days open,
# reason, size, title) for further analysis.

set -euo pipefail

# The library is resolved from this script's own location, not from the git root
# of the file under analysis, which a test suite scopes to a throwaway repo.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

MODE=()
if [[ "${1:-}" == "--events" ]]; then
    MODE=(-events)
    shift
fi

if [[ -n "${1:-}" ]]; then
    FILE="$1"
else
    FILE="$(git rev-parse --show-toplevel)/docs/STATUS.md"
fi

if [[ ! -f "$FILE" ]]; then
    printf 'backlog-metrics: file not found: %s\n' "$FILE" >&2
    exit 2
fi

DIR="$(cd "$(dirname "$FILE")" && pwd)"
BASE="$(basename "$FILE")"

# Built and exec'd rather than `go run`, which would print its own status line
# on top of the report. devtools/ is outside the Go workspace, hence GOWORK=off
# — see docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/backlogmetrics"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/backlogmetrics)

# Replay oldest-first. The pipeline runs in a subshell so `set -o pipefail`
# carries a failing `git log` out as the script's own status.
git -C "$DIR" log --reverse -p --format='@COMMIT %as %s' -- "$BASE" |
    "$bin" ${MODE+"${MODE[@]}"} -status "$FILE" -today "$(date +%Y-%m-%d)"
