#!/usr/bin/env bash
#
# lint-status.sh — enforce churn-reduction format rules for docs/STATUS.md.
#
# Rules enforced (see docs/development/maintaining-backlog.md):
#   1. Exactly one `Last touched: YYYY-MM-DD` line — no narrative, no
#      multiple "Last refreshed:" entries.
#   2. Queue table IDs are unique. IDs are stable pointers; duplicates would
#      silently shadow cross-references in plan docs, commits, and PRs.
#   3. Queue Notes column is ≤ NOTES_MAX_CHARS characters (default 250). Long
#      notes belong in the linked plan doc, not in the high-contention Queue.
#
# Usage:
#   scripts/lint-status.sh [path/to/STATUS.md]
#
# Defaults to docs/STATUS.md relative to the repo root.

set -euo pipefail

NOTES_MAX_CHARS="${NOTES_MAX_CHARS:-250}"

# Locate the target file. Default to docs/STATUS.md under the git repo root,
# so the script works whether invoked from the repo root or a subdirectory.
locate_file() {
    local arg="${1:-}"
    if [[ -n "$arg" ]]; then
        printf '%s\n' "$arg"
        return
    fi
    local repo_root
    repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
    printf '%s/docs/STATUS.md\n' "$repo_root"
}

FILE="$(locate_file "${1:-}")"

if [[ ! -f "$FILE" ]]; then
    printf 'lint-status: file not found: %s\n' "$FILE" >&2
    exit 2
fi

exit_code=0

fail() {
    # Use a GitHub Actions error annotation when running in CI, plain text
    # otherwise. Either way, the message is printed and exit_code is set.
    local msg="$1"
    if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
        printf '::error file=%s::%s\n' "$FILE" "$msg"
    else
        printf 'lint-status: %s: %s\n' "$FILE" "$msg" >&2
    fi
    exit_code=1
}

# ----------------------------------------------------------------------------
# Rule 1: `Last touched:` is one line, date only.
# ----------------------------------------------------------------------------
check_last_touched() {
    local lt_lines
    lt_lines="$(grep -n '^Last touched:' "$FILE" || true)"
    local lt_count=0
    if [[ -n "$lt_lines" ]]; then
        lt_count="$(printf '%s\n' "$lt_lines" | wc -l | tr -d ' ')"
    fi

    if (( lt_count == 0 )); then
        fail "missing 'Last touched: YYYY-MM-DD' header"
        return
    fi
    if (( lt_count > 1 )); then
        fail "found $lt_count 'Last touched:' lines; expected exactly 1 (no narrative, no 'Earlier:' entries)"
        return
    fi

    # Validate the date-only format. Lines look like "12:Last touched: 2026-05-30".
    local content="${lt_lines#*:}"
    if [[ ! "$content" =~ ^Last\ touched:\ [0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
        fail "'Last touched:' must match 'Last touched: YYYY-MM-DD' exactly; got: ${content}"
    fi
}

# ----------------------------------------------------------------------------
# Helpers for Queue parsing.
#
# Queue rows look like:
#   | <a id="Q44"></a>Q44 | Item title | `label1` `label2` | 🔲 | S | Notes. |
#
# Splitting on `|` yields 8 awk fields (empty $1 before the leading pipe,
# empty $8 after the trailing pipe). The Q-prefixed ID lives in $2 (after
# stripping the inline anchor tag and whitespace); Notes is $7. Header
# (`| ID | Item | ...`) and divider (`|---|---|...`) rows are excluded by
# the post-strip regex guard on $2.
# ----------------------------------------------------------------------------

# Emit "id<TAB>notes_length<TAB>notes" for each Queue row, one per line.
queue_rows() {
    awk -F'|' '
        /^## Queue/ { in_queue=1; next }
        /^## /      { in_queue=0 }
        in_queue && /^\|/ {
            id=$2
            gsub(/<[^>]*>/, "", id)      # strip inline anchor tags
            gsub(/[[:space:]]/, "", id)
            if (id !~ /^Q[0-9]+$/) next  # skip header, divider, non-rows
            notes=$7
            sub(/^[[:space:]]+/, "", notes)
            sub(/[[:space:]]+$/, "", notes)
            printf "%s\t%d\t%s\n", id, length(notes), notes
        }
    ' "$FILE"
}

# ----------------------------------------------------------------------------
# Rule 2: Queue IDs are unique.
# ----------------------------------------------------------------------------
check_unique_ids() {
    local dups
    dups="$(queue_rows | cut -f1 | sort | uniq -d)"
    if [[ -n "$dups" ]]; then
        local id
        while IFS= read -r id; do
            fail "duplicate Queue ID: ${id}"
        done <<< "$dups"
    fi
}

# ----------------------------------------------------------------------------
# Rule 3: Notes ≤ NOTES_MAX_CHARS.
# ----------------------------------------------------------------------------
check_notes_length() {
    local id len _notes
    while IFS=$'\t' read -r id len _notes; do
        if (( len > NOTES_MAX_CHARS )); then
            fail "Queue ${id} Notes is ${len} chars (max ${NOTES_MAX_CHARS}); move detail to the linked plan doc"
        fi
    done < <(queue_rows)
}

check_last_touched
check_unique_ids
check_notes_length

if (( exit_code == 0 )); then
    printf 'lint-status: ok (%s)\n' "$FILE"
fi

exit "$exit_code"
