#!/usr/bin/env bash
#
# backlog-metrics.sh — replay the backlog's git history into per-item events and
# summary flow metrics, across both storage eras.
#
# The backlog was a single table at docs/STATUS.md until Q889 moved it to one
# file per item under docs/queue/. The series is continuous across that move:
# this script runs one git log per era and hands both to the reporter, which
# suppresses the two bulk seam commits and marks where the storage changed.
# The table's history is read by path, so it keeps answering after the file is
# deleted — deletion ends the era, it does not end the record.
#
# The replay is done by devtools/docs/backlogmetrics, a Go program reading the
# rows through a real Markdown parser (Q614); this script is the entry point
# that runs the git logs, so the gate map stays in scripts/. What the replay
# counts, how removal reasons are derived, and what happens at the seam are
# documented in that program's package comment.
#
# This script only reads, and stays offline; the authoritative allocation count
# is the ref namespace (`git ls-remote origin 'refs/queue-ids/*' | wc -l`).
#
# Usage:
#   backlog-metrics.sh [--events] [path/to/STATUS.md]
#
# The path names the table era's file, present or deleted; the store is its
# `queue/` sibling. Default: summary (throughput, cycle time, arrival rate,
# prune ratio, aging WIP). --events: TSV event stream (id, filed date, removed
# date, days open, reason, era, size, title) for further analysis.

set -euo pipefail
shopt -s inherit_errexit

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

DIR="$(dirname "$FILE")"
if [[ ! -d "$DIR" ]]; then
    printf 'backlog-metrics: directory not found: %s\n' "$DIR" >&2
    exit 2
fi
DIR="$(cd "$DIR" && pwd)"
BASE="$(basename "$FILE")"
# The store is the table's sibling, so the optional path argument selects both
# and a test suite can point the whole replay at a throwaway repo.
STORE="$DIR/queue"

# The seam. Empty until the deletion is in history, which is the state on any
# branch that has not yet cut over: no seam, and the table era is the series.
CUTOVER="$(git -C "$DIR" log -1 --diff-filter=D --format=%as -- "$BASE")"

# Built and exec'd rather than `go run`, which would print its own status line
# on top of the report. devtools/ is outside the Go workspace, hence GOWORK=off
# — see docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/backlogmetrics"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/backlogmetrics)

# The store log goes to a file rather than a second pipe: a process
# substitution would hide a failing `git log` as an empty era, which reads as
# "nothing filed since the cutover" and is indistinguishable from the truth.
store_log="$(mktemp)"
trap 'rm -f "$store_log"' EXIT
git -C "$DIR" log --reverse --name-status --format='@COMMIT %as %s' \
    -- "$(basename "$STORE")" > "$store_log"

# Replay oldest-first. The pipeline runs in a subshell so `set -o pipefail`
# carries a failing `git log` out as the script's own status.
git -C "$DIR" log --reverse -p --format='@COMMIT %as %s' -- "$BASE" |
    "$bin" ${MODE+"${MODE[@]}"} \
        -status "$FILE" -store "$STORE" -store-log "$store_log" \
        -cutover "$CUTOVER" -today "$(date +%Y-%m-%d)"
