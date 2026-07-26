#!/usr/bin/env bash
#
# lint-backlog.sh — format checks for a repo-local backlog file (docs/STATUS.md).
#
# Content rules (see the backlog skill's SKILL.md):
#   1. No `**Next ID:** QN` line. IDs come from `scripts/alloc-queue-id.sh`,
#      which claims a `refs/queue-ids/QN` ref on the remote — an atomic
#      server-side test-and-set. A file-local counter is a single mutable line,
#      so concurrent sessions always took the same ID and always conflicted on
#      the same line, forcing a renumber (Q382). Flagged as old format.
#   2. IDs are unique across the Queue and Deferred tables, and each row's
#      `<a id="QN"></a>QN` anchor matches its visible ID (cross-references
#      resolve through the anchor).
#   3. Queue `St` is 🔲 or 🚫 only. ✅/▶/💤 are old-format markers: done rows
#      are deleted, started is signaled by the open PR, deferred rows live in
#      the Deferred table.
#   4. Queue Notes ≤ NOTES_MAX_CHARS (default 250); over NOTES_LINK_CHARS
#      (default 200) the cell must link another document (a `#QN` sibling
#      anchor doesn't count — sibling rows are capped too). Same caps apply to
#      the Deferred trigger cell.
#   5. A `Blocked by [QN](#QN)` prefix requires St 🚫, and every `(#QN)` link
#      target in the file must resolve to an existing row.
#   6. Deferred trigger cells open with **Demand:**, **Event:**, or
#      **Decision:** — a deferred row without a concrete revive trigger is a
#      zombie in waiting.
#   7. No `Last touched:` line — that fact lives in
#      `git log -1 --format=%as -- <file>` and the manual line only causes
#      conflicts and staleness. Flagged as old format.
#   8. A `flake`-labelled Queue row may not simply vanish: once its mitigation
#      ships the row moves to Deferred § Flake watch, so a recurrence reads as
#      a recurrence rather than a fresh find (maintaining-backlog.md § Flake
#      fixes go first). Checked against a git baseline, since a deletion is
#      invisible from the file alone. Retiring a Flake watch row is a separate,
#      grooming-time decision — set BACKLOG_ALLOW_FLAKE_DELETE="Q1 Q2" to allow
#      specific IDs through.
#
# Usage:
#   lint-backlog.sh [--staged] [path/to/STATUS.md]
#
# Defaults to docs/STATUS.md under the repo root. With --staged (pre-commit
# mode): exits 0 untouched when the backlog file is not staged; when it is
# staged, requires it to be the *only* staged file (backlog edits are isolated
# commits so rebase conflicts resolve on one file) and then runs the content
# rules. Bypass a single commit with `git commit --no-verify`.

set -euo pipefail

NOTES_MAX_CHARS="${NOTES_MAX_CHARS:-250}"
NOTES_LINK_CHARS="${NOTES_LINK_CHARS:-200}"

STAGED=0
if [[ "${1:-}" == "--staged" ]]; then
    STAGED=1
    shift
fi

if [[ -n "${1:-}" ]]; then
    FILE="$1"
else
    repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
    FILE="$repo_root/docs/STATUS.md"
fi

if (( STAGED )); then
    repo_root="$(git rev-parse --show-toplevel)"
    rel="${FILE#"$repo_root"/}"
    staged_files="$(git diff --cached --name-only --diff-filter=ACMRD)"
    if ! grep -qx "$rel" <<<"$staged_files"; then
        exit 0
    fi
    others="$(grep -vx "$rel" <<<"$staged_files" || true)"
    if [[ -n "$others" ]]; then
        {
            printf 'lint-backlog: %s must be committed in isolation, but these files are staged with it:\n' "$rel"
            awk '{ print "  " $0 }' <<<"$others"
            printf 'commit the backlog edit separately (git reset <files>, or commit them first)\n'
        } >&2
        exit 1
    fi
fi

if [[ ! -f "$FILE" ]]; then
    printf 'lint-backlog: file not found: %s\n' "$FILE" >&2
    exit 2
fi

# Rule 8. A deletion is invisible from the file alone, so compare against a git
# baseline: the pre-commit state in --staged mode, otherwise origin/main (the
# branch point for any PR). Silently skipped when no baseline resolves — a
# fresh clone with no origin, or the backlog file not yet in git.
flake_queue_ids() {
    awk -F'|' '
        /^## Queue/    { in_queue = 1; next }
        /^## /         { in_queue = 0 }
        in_queue && /^\|/ && $4 ~ /`flake`/ {
            cell = $2
            gsub(/<[^>]*>/, "", cell)
            gsub(/[[:space:]]/, "", cell)
            if (cell ~ /^Q[0-9]+$/) print cell
        }
    '
}

check_flake_rows_preserved() {
    local baseline_ref="" baseline="" rel repo_root
    repo_root="$(git rev-parse --show-toplevel 2>/dev/null || true)"
    [[ -n "$repo_root" ]] || return 0
    rel="${FILE#"$repo_root"/}"

    if (( STAGED )); then
        baseline_ref="HEAD"
    elif git rev-parse --verify --quiet origin/main >/dev/null; then
        baseline_ref="origin/main"
    else
        return 0
    fi

    baseline="$(git show "$baseline_ref:$rel" 2>/dev/null || true)"
    [[ -n "$baseline" ]] || return 0

    local -a allowed=()
    read -r -a allowed <<<"${BACKLOG_ALLOW_FLAKE_DELETE:-}"

    local id missing=0
    while read -r id; do
        [[ -n "$id" ]] || continue
        # Still present anywhere in the file (Queue or Deferred) -> fine.
        grep -q "<a id=\"$id\"></a>" "$FILE" && continue
        local ok=0 a
        for a in ${allowed+"${allowed[@]}"}; do
            [[ "$a" == "$id" ]] && ok=1 && break
        done
        (( ok )) && continue
        if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
            printf '::error file=%s::%s was a flake-labelled Queue row in %s and is now gone; a shipped flake mitigation moves the row to Deferred, Flake watch (trigger: Event: recurs on main after the fix) — it is not deleted. See docs/development/maintaining-backlog.md#flake-fixes-go-first\n' \
                "$FILE" "$id" "$baseline_ref"
        else
            printf 'lint-backlog: %s: %s was a flake-labelled Queue row in %s and is now gone.\n' "$FILE" "$id" "$baseline_ref" >&2
            printf '  A shipped flake mitigation moves the row to Deferred, Flake watch, with an\n' >&2
            printf '  "Event: recurs on main after the fix" trigger — kept, not closed, so a second\n' >&2
            printf '  occurrence reads as a recurrence rather than a fresh find. See\n' >&2
            printf '  docs/development/maintaining-backlog.md#flake-fixes-go-first.\n' >&2
            printf '  Retiring the row instead? BACKLOG_ALLOW_FLAKE_DELETE=%s\n' "$id" >&2
        fi
        missing=1
    done < <(flake_queue_ids <<<"$baseline")

    return "$missing"
}

flake_check_rc=0
check_flake_rows_preserved || flake_check_rc=1

# Single awk pass. Rows split on `|`:
#   Queue:    | <a id="Q4"></a>Q4 | Item | `labels` | St | Sz | Notes |  -> 8 fields
#   Deferred: | <a id="Q4"></a>Q4 | Item | `labels` | Sz | Trigger    |  -> 7 fields
awk -F'|' \
    -v file="$FILE" \
    -v max_chars="$NOTES_MAX_CHARS" \
    -v link_chars="$NOTES_LINK_CHARS" '
function fail(msg) {
    if (ENVIRON["GITHUB_ACTIONS"] != "")
        printf "::error file=%s::%s\n", file, msg
    else
        printf "lint-backlog: %s: %s\n", file, msg | "cat >&2"
    bad = 1
}

function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }

# Extract "<a id=\"QN\"></a>QN" -> record anchor/visible IDs; returns visible ID or "".
function parse_id(cell,    anchor, visible, tmp) {
    anchor = ""
    if (match(cell, /<a id="Q[0-9]+">/)) {
        anchor = substr(cell, RSTART, RLENGTH)
        gsub(/[^0-9]/, "", anchor)
    }
    tmp = cell
    gsub(/<[^>]*>/, "", tmp)
    visible = trim(tmp)
    if (visible !~ /^Q[0-9]+$/) return ""
    if (anchor == "")
        fail(visible " has no <a id=\"" visible "\"></a> anchor; cross-references cannot resolve")
    else if ("Q" anchor != visible)
        fail("anchor id=\"Q" anchor "\" does not match visible ID " visible)
    return visible
}

function register_id(id, section) {
    if (id in ids) fail("duplicate ID " id " (in " ids[id] " and " section ")")
    ids[id] = section
}

# Cross-document link: "](x" where x is not "#" (sibling anchors are capped too).
function has_doc_link(item, notes) { return (item ~ /\]\([^#)]/ || notes ~ /\]\([^#)]/) }

# Collect every "(#QN)" link target in a cell for END-time resolution.
function collect_refs(id, cell,    rest, tgt) {
    rest = cell
    while (match(rest, /\(#Q[0-9]+\)/)) {
        tgt = substr(rest, RSTART + 2, RLENGTH - 3)
        refs[++nrefs] = id "\t" tgt
        rest = substr(rest, RSTART + RLENGTH)
    }
}

/^\*\*Next ID:\*\*/ {
    fail("old format: drop the Next ID counter; allocate with scripts/alloc-queue-id.sh (a file-local counter conflicts by construction under concurrent sessions)")
}

/^Last touched:/ {
    fail("old format: drop the Last touched line; use git log -1 --format=%as -- " file)
}

/^## Queue/    { section = "Queue"; seen_queue = 1; next }
/^## Deferred/ { section = "Deferred"; next }
/^## /         { section = "" }

section == "Queue" && /^\|/ {
    id = parse_id($2)
    if (id == "") next
    register_id(id, section)
    item = $3; st = trim($5); notes = trim($7)
    if (st == "💤")
        fail(id " is 💤 in the Queue; deferred rows move to the ## Deferred table (old format)")
    else if (st == "✅" || st == "▶")
        fail(id " St is " st "; done rows are deleted and started is signaled by the open PR (old format)")
    else if (st != "🔲" && st != "🚫")
        fail(id " St must be 🔲 or 🚫; got: " st)
    if (length(notes) > max_chars)
        fail(id " Notes is " length(notes) " chars (max " max_chars "); move detail to the linked plan doc")
    else if (length(notes) > link_chars && !has_doc_link(item, notes))
        fail(id " Notes is " length(notes) " chars (> " link_chars ") but links no document from its Item or Notes cell (a #QN sibling anchor does not count)")
    if (notes ~ /^Blocked by \[Q[0-9]+\]/ && st != "🚫")
        fail(id " Notes say Blocked by but St is not 🚫")
    collect_refs(id, item "|" notes)
}

section == "Deferred" && /^\|/ {
    id = parse_id($2)
    if (id == "") next
    register_id(id, section)
    item = $3; trigger = trim($6)
    if (trigger !~ /^\*\*(Demand|Event|Decision):\*\*/)
        fail(id " Deferred trigger must open with **Demand:**, **Event:**, or **Decision:**; got: " substr(trigger, 1, 40))
    if (length(trigger) > max_chars)
        fail(id " trigger cell is " length(trigger) " chars (max " max_chars "); move detail to the linked plan doc")
    else if (length(trigger) > link_chars && !has_doc_link(item, trigger))
        fail(id " trigger cell is " length(trigger) " chars (> " link_chars ") but links no document")
    collect_refs(id, item "|" trigger)
}

END {
    if (!seen_queue) fail("no ## Queue section found")
    for (i = 1; i <= nrefs; i++) {
        split(refs[i], r, "\t")
        if (!(r[2] in ids))
            fail(r[1] " links (#" r[2] ") but no row " r[2] " exists")
    }
    exit bad
}
' "$FILE" || awk_rc=1

if (( ${awk_rc:-0} || flake_check_rc )); then
    exit 1
fi

printf 'lint-backlog: ok (%s)\n' "$FILE"
