#!/usr/bin/env bash
# queue-unblock.sh — Enumerate Queue items in docs/STATUS.md blocked by a given ID.
#
# Usage: scripts/queue-unblock.sh <id>
# Or:    make queue-unblock ID=<id>
#
# Prints Queue rows whose Notes contain "Blocked by #<id>" (including
# comma-separated lists like "Blocked by #12, #15"). Use this when the
# dependency lands so every dependent can be unblocked in one commit.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATUS_FILE="$REPO_ROOT/docs/STATUS.md"

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <id>" >&2
    echo "Prints Queue items whose Notes contain 'Blocked by #<id>'." >&2
    exit 1
fi

local_id="$1"
if ! [[ "$local_id" =~ ^[0-9]+$ ]]; then
    echo "ERROR: ID must be numeric, got: $local_id" >&2
    exit 1
fi

if [[ ! -f "$STATUS_FILE" ]]; then
    echo "ERROR: $STATUS_FILE not found" >&2
    exit 1
fi

# Scan only the Queue table. For each row, isolate the "Blocked by …"
# substring (up to the next sentence terminator) so a stray `#12` elsewhere
# in Notes does not produce a false match, then search for `#<id>` bounded by
# a non-digit so `#12` does not match `#125`.
matches=$(awk -v id="$local_id" '
    /^## Queue/        { in_queue = 1; next }
    in_queue && /^---/ { in_queue = 0 }
    in_queue && /^\| [0-9]+ \|/ {
        s = $0
        if (match(s, /Blocked by[^.]*/)) {
            blocker = substr(s, RSTART, RLENGTH)
            if (blocker ~ ("#" id "([^0-9]|$)")) {
                print s
            }
        }
    }
' "$STATUS_FILE")

if [[ -z "$matches" ]]; then
    echo "No Queue items are blocked by #${local_id}."
    exit 0
fi

echo "Queue items blocked by #${local_id}:"
echo
echo "$matches"
