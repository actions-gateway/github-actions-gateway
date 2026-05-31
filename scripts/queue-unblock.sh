#!/usr/bin/env bash
# queue-unblock.sh — Enumerate Queue items in docs/STATUS.md blocked by a given ID.
#
# Usage: scripts/queue-unblock.sh <id>
# Or:    make queue-unblock ID=<id>
#
# <id> may be given as `Q12` or bare `12` — both forms are accepted.
# Prints Queue rows whose Notes contain a `Blocked by` reference to `Q<id>`
# (e.g. `Blocked by [Q12](#Q12)`, including comma-separated lists). Use this
# when the dependency lands so every dependent can be unblocked in one commit.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATUS_FILE="$REPO_ROOT/docs/STATUS.md"

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <id>" >&2
    echo "Prints Queue items whose Notes contain 'Blocked by [Q<id>](#Q<id>)'." >&2
    exit 1
fi

local_id="${1#Q}"  # accept Q12 or 12
if ! [[ "$local_id" =~ ^[0-9]+$ ]]; then
    echo "ERROR: ID must be numeric (or Q-prefixed), got: $1" >&2
    exit 1
fi

if [[ ! -f "$STATUS_FILE" ]]; then
    echo "ERROR: $STATUS_FILE not found" >&2
    exit 1
fi

# Scan only the Queue table. For each row, isolate the "Blocked by …"
# substring (up to the next sentence terminator) so a stray `Q12` elsewhere
# in Notes does not produce a false match, then search for `Q<id>` bounded by
# a non-digit so `Q12` does not match `Q125`.
matches=$(awk -v id="$local_id" '
    /^## Queue/        { in_queue = 1; next }
    in_queue && /^---/ { in_queue = 0 }
    in_queue && /^\| <a id="Q[0-9]+"><\/a>Q[0-9]+ \|/ {
        s = $0
        if (match(s, /Blocked by[^.]*/)) {
            blocker = substr(s, RSTART, RLENGTH)
            if (blocker ~ ("Q" id "([^0-9]|$)")) {
                print s
            }
        }
    }
' "$STATUS_FILE")

if [[ -z "$matches" ]]; then
    echo "No Queue items are blocked by Q${local_id}."
    exit 0
fi

echo "Queue items blocked by Q${local_id}:"
echo
echo "$matches"
