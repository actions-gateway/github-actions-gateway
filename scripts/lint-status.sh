#!/usr/bin/env bash
# lint-status.sh — Lint docs/STATUS.md for format rules.
#
# Enforces:
#   - Single-line `Last touched:` (date only, no narrative)
#   - No duplicate Queue IDs
#   - Notes column ≤250 characters
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATUS_FILE="$REPO_ROOT/docs/STATUS.md"

if [[ ! -f "$STATUS_FILE" ]]; then
    echo "ERROR: $STATUS_FILE not found" >&2
    exit 1
fi

exit_code=0

# Rule 1: Single-line `Last touched:`
last_touched_lines=$(grep -c "^Last touched:" "$STATUS_FILE" || true)
if [[ "$last_touched_lines" -ne 1 ]]; then
    echo "ERROR: Expected exactly one 'Last touched:' line, found $last_touched_lines" >&2
    exit_code=1
else
    last_touched=$(grep "^Last touched:" "$STATUS_FILE")
    if ! [[ "$last_touched" =~ ^Last\ touched:\ [0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
        echo "ERROR: 'Last touched:' must be 'Last touched: YYYY-MM-DD' (date only, no narrative)" >&2
        echo "  Got: $last_touched" >&2
        exit_code=1
    fi
fi

# Rule 2: No duplicate Queue IDs
# Extract IDs from Queue table (rows like "| 44 |")
mapfile -t queue_ids < <(awk '
    /^## Queue/,/^---/ {
        if ($0 ~ /^\| [0-9]+ \|/) {
            gsub(/^\| +/, "");
            gsub(/ +\|.*/, "");
            print $0
        }
    }
' "$STATUS_FILE")

declare -A seen_ids
for id in "${queue_ids[@]}"; do
    if [[ -n "$id" ]]; then
        if [[ -v seen_ids["$id"] ]]; then
            echo "ERROR: Duplicate Queue ID: $id" >&2
            exit_code=1
        fi
        seen_ids["$id"]=1
    fi
done

# Rule 3: Notes column ≤250 characters
mapfile -t long_notes < <(awk '
    /^## Queue/,/^---/ {
        if ($0 ~ /^\| [0-9]+ \|/) {
            # Extract Notes (last column after final |)
            match($0, /\| ([^|]+)$/)
            notes = substr($0, RSTART+2, RLENGTH-3)
            gsub(/^[ ]+|[ ]+$/, "", notes)
            if (length(notes) > 250) {
                # Extract ID for error message
                id_match = match($0, /^\| +([0-9]+) \|/)
                id = substr($0, RSTART+2, RLENGTH-4)
                gsub(/[ ]+/, "", id)
                print id ": " length(notes) " chars"
            }
        }
    }
' "$STATUS_FILE")

if [[ ${#long_notes[@]} -gt 0 ]]; then
    echo "ERROR: Notes column exceeds 250 characters:" >&2
    for note in "${long_notes[@]}"; do
        echo "  Queue item $note" >&2
    done
    exit_code=1
fi

if [[ $exit_code -eq 0 ]]; then
    echo "✓ docs/STATUS.md format OK"
fi

exit $exit_code
