#!/usr/bin/env bash
# queue-unblock.sh — Enumerate backlog items blocked by a given ID.
#
# Usage: scripts/docs/queue-unblock.sh <id>
# Or:    make queue-unblock ID=<id>
#
# <id> may be given as `Q12` or bare `12` — both forms are accepted.
# Prints items whose note carries a `Blocked by` reference to `Q<id>`
# (e.g. `Blocked by [Q12](Q12.md)`, including comma-separated lists). Use this
# when the dependency lands so every dependent can be unblocked in one commit.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STORE="$REPO_ROOT/docs/queue"

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <id>" >&2
    echo "Prints backlog items whose note carries 'Blocked by [Q<id>](Q<id>.md)'." >&2
    exit 1
fi

local_id="${1#Q}"  # accept Q12 or 12
if ! [[ "$local_id" =~ ^[0-9]+$ ]]; then
    echo "ERROR: ID must be numeric (or Q-prefixed), got: $1" >&2
    exit 1
fi

if [[ ! -d "$STORE" ]]; then
    echo "ERROR: $STORE not found" >&2
    exit 1
fi

shopt -s nullglob
items=("$STORE"/Q*.md)
shopt -u nullglob
if (( ${#items[@]} == 0 )); then
    echo "ERROR: no items under $STORE" >&2
    exit 1
fi

# Take the blocker clause as "Blocked by" through end of line, not up to the
# next period: an item store links its blockers as `[Q12](Q12.md)`, so a
# period-terminated span stops inside the first link and drops every later
# blocker in a comma-separated list. One sentence per line is what makes the
# line the right unit, and md-reflow-check gates that across docs/.
# The Q<id> match is bounded by a non-digit so Q12 does not match Q125.
matches=$(awk -v id="$local_id" '
    FNR == 1 { title = ""; qid = FILENAME; sub(/.*\//, "", qid); sub(/\.md$/, "", qid) }
    /^# / && title == "" { title = substr($0, 3) }
    /Blocked by/ {
        clause = substr($0, index($0, "Blocked by"))
        if (clause ~ ("Q" id "([^0-9]|$)")) printf "%s  %s\n    %s\n", qid, title, clause
    }
' "${items[@]}")

if [[ -z "$matches" ]]; then
    echo "No backlog items are blocked by Q${local_id}."
    exit 0
fi

echo "Backlog items blocked by Q${local_id}:"
echo
echo "$matches"
