#!/usr/bin/env bash
#
# lint-backlog.sh — format checks for a repo-local backlog file (docs/STATUS.md).
#
# Content rules (see the backlog skill's SKILL.md):
#   1. No `**Next ID:** QN` line. IDs come from `scripts/docs/alloc-queue-id.sh`,
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
#   9. Deleting the LAST Queue row that points at a plan doc changes that plan's
#      Progress verdict to ✅ (deferred residuals don't count), and the flip must
#      land in the same edit. Also checked against a git baseline, and only for
#      plans whose last row just disappeared: a steady-state scan would misread
#      the many rows that merely *cite* a completed plan as evidence. Set
#      BACKLOG_ALLOW_PROGRESS_STALE="plan/foo.md" when the vanished row was such
#      a citation and real work genuinely remains.
#  10. A row the baseline *deleted* may not reappear. Done rows are deleted, so
#      a resurrected one re-opens finished work — and it arrives silently:
#      reordering a row moves it, so a branch that relocates a row while main
#      deletes it merges with no conflict at all (maintaining-backlog.md § A
#      moved row defeats conflict detection). A clean rebase is not evidence of
#      a correct one. An ID absent from the baseline file is a new row if the
#      baseline's history never carried it, and a resurrection if it did.
#      Deliberately re-opening a closed item? Set BACKLOG_ALLOW_RESURRECT="Q1 Q2".
#  11. Every label a row wears is declared on the `**Labels:**` line, across the
#      Progress, Queue, and Deferred tables. A retired or mistyped label is
#      otherwise invisible: Q592 was filed with `infra` from a branch cut before
#      that label was split into ci/dogfood/debt, and merged without a conflict
#      because the two edits touched different rows.
#  12. A Q-ID this branch ADDS holds a `refs/queue-ids/QN` claim on the remote.
#      Rule 1 removes the shared counter; this removes the other way to obtain
#      an ID without reserving it, which is to read the file's highest and add
#      one. Q656 measured what that costs: a row carrying Q644 was committed 43
#      minutes before any Q644 claim existed, a second session then claimed
#      Q644 legitimately, and the rule-follower paid the renumber across a
#      commit message, a PR body and a plan doc. Scoped to *new* IDs against the
#      git baseline, and to IDs at or above the namespace's lowest claim —
#      everything below predates the allocator and holds no ref. Skipped when
#      the remote is unreachable, so an offline clone still lints; CI re-runs it
#      with a network. Filing a row whose ID was claimed elsewhere (a dispatcher
#      allocating for a worker)? Set BACKLOG_ALLOW_UNCLAIMED_ID="Q1 Q2".
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

# baseline_ref prints the ref to compare the backlog against — the pre-commit
# state in --staged mode, otherwise origin/main (the branch point for any PR).
# Empty when neither resolves, which makes the git-baseline rules no-ops rather than
# failures on a fresh clone with no origin.
baseline_ref() {
    if (( STAGED )); then
        printf 'HEAD'
    elif git rev-parse --verify --quiet origin/main >/dev/null; then
        printf 'origin/main'
    fi
}

# backlog_relpath prints FILE relative to the repo root, for `git show REF:path`.
# Empty outside a repo.
#
# Asks git for the prefix rather than stripping `git rev-parse --show-toplevel`
# off the front: on macOS the two spellings of a temp path (/var/... and
# /private/var/...) differ by a symlink, so the string strip silently failed to
# match and quietly disabled the git-baseline rules.
backlog_relpath() {
    local dir prefix
    dir="$(dirname "$FILE")"
    prefix="$(git -C "$dir" rev-parse --show-prefix 2>/dev/null)" || return 0
    printf '%s%s' "$prefix" "$(basename "$FILE")"
}

# anchor_ids prints every "QN" that has a row anchor in the STATUS.md on stdin.
anchor_ids() {
    grep -oE '<a id="Q[0-9]+"></a>' | grep -oE 'Q[0-9]+'
}

check_flake_rows_preserved() {
    local baseline_ref="" baseline="" rel
    rel="$(backlog_relpath)"
    [[ -n "$rel" ]] || return 0
    baseline_ref="$(baseline_ref)"
    [[ -n "$baseline_ref" ]] || return 0

    baseline="$(git show "$baseline_ref:$rel" 2>/dev/null || true)"
    [[ -n "$baseline" ]] || return 0

    local -a allowed=()
    read -r -a allowed <<<"${BACKLOG_ALLOW_FLAKE_DELETE:-}"

    local id missing=0
    while read -r id; do
        [[ -n "$id" ]] || continue
        # Still present anywhere in the file (Queue or Deferred) -> fine.
        grep -q "<a id=\"$id\"></a>" "$FILE" && continue
        # Absent — but a branch opened before the row was filed never had it to
        # delete. Only flag when HEAD already carries the commit that added it;
        # otherwise every branch that is merely behind main reports a deletion
        # it did not make. (Same staleness trap as rule 9.)
        local added_in
        added_in="$(git log -1 --format=%H -S"<a id=\"$id\"></a>" \
            "$baseline_ref" -- "$rel" 2>/dev/null || true)"
        if [[ -n "$added_in" ]]; then
            git merge-base --is-ancestor "$added_in" HEAD 2>/dev/null || continue
        fi
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

# Rule 10. Catches the resurrection the manual `comm` check in
# maintaining-backlog.md asks for, and distinguishes the two cases it cannot:
# an ID missing from the baseline FILE is a new row when the baseline's HISTORY
# never carried it, and a resurrected done row when it did.
check_no_resurrected_rows() {
    local baseline_ref="" baseline="" rel
    rel="$(backlog_relpath)"
    [[ -n "$rel" ]] || return 0
    baseline_ref="$(baseline_ref)"
    [[ -n "$baseline_ref" ]] || return 0

    baseline="$(git show "$baseline_ref:$rel" 2>/dev/null || true)"
    [[ -n "$baseline" ]] || return 0

    local -a allowed=()
    read -r -a allowed <<<"${BACKLOG_ALLOW_RESURRECT:-}"

    local id rc=0 ok a
    while read -r id; do
        [[ -n "$id" ]] || continue
        # Still in the baseline file: not a deletion, nothing to resurrect.
        grep -q "<a id=\"$id\"></a>" <<<"$baseline" && continue
        # Absent from the baseline file. If its history never held the anchor
        # either, this is simply a newly filed row.
        #
        # Captured rather than piped into `grep -q`: under `set -o pipefail`,
        # grep exits at the first line, git log dies of SIGPIPE, and the
        # pipeline reports 141 — which read as "no history" and silently
        # skipped every resurrection this rule exists to catch.
        local removed_in
        removed_in="$(git log -1 --format=%H -S"<a id=\"$id\"></a>" \
            "$baseline_ref" -- "$rel" 2>/dev/null || true)"
        [[ -n "$removed_in" ]] || continue

        # The history held it, so it was deleted — but by whom, relative to this
        # branch? If the deleting commit is not yet an ancestor of HEAD, the
        # branch simply predates the deletion and a rebase will apply it. Only a
        # branch that ALREADY carries the deletion and still shows the row has
        # actually brought it back. Without this the rule fires on every branch
        # that is merely behind main.
        git merge-base --is-ancestor "$removed_in" HEAD 2>/dev/null || continue
        ok=0
        for a in ${allowed+"${allowed[@]}"}; do
            [[ "$a" == "$id" ]] && ok=1 && break
        done
        (( ok )) && continue
        if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
            printf '::error file=%s::%s is back in %s but %s deleted it — done rows are deleted, so this re-opens finished work. A reordered row merges cleanly over a delete, so a clean rebase is not evidence of a correct one. See docs/development/maintaining-backlog.md#a-moved-row-defeats-conflict-detection\n' \
                "$FILE" "$id" "$(basename "$FILE")" "$baseline_ref"
        else
            printf 'lint-backlog: %s: %s is present here but %s deleted it.\n' "$FILE" "$id" "$baseline_ref" >&2
            printf '  Done rows are deleted (git is the archive), so a row that comes back\n' >&2
            printf '  re-opens finished work. Reordering a row moves it, so a branch that\n' >&2
            printf '  relocates a row while main deletes it merges with NO conflict — a clean\n' >&2
            printf '  rebase is not evidence of a correct one. Check whether the work shipped:\n' >&2
            printf '    git log -S%s%s%s --oneline %s -- %s\n' \
                "'" "<a id=\"$id\"></a>" "'" "$baseline_ref" "$rel" >&2
            printf '  See docs/development/maintaining-backlog.md#a-moved-row-defeats-conflict-detection.\n' >&2
            printf '  Deliberately re-opening it? BACKLOG_ALLOW_RESURRECT=%s\n' "$id" >&2
        fi
        rc=1
    done < <(anchor_ids <"$FILE")

    return "$rc"
}

# Rule 12. The allocator's claim namespace. An ID with no claim was never
# reserved, so a concurrent session can still be handed it.
QUEUE_ID_REF_NS='refs/queue-ids'

check_new_ids_claimed() {
    local baseline_ref="" baseline="" rel
    rel="$(backlog_relpath)"
    [[ -n "$rel" ]] || return 0
    baseline_ref="$(baseline_ref)"
    [[ -n "$baseline_ref" ]] || return 0

    baseline="$(git show "$baseline_ref:$rel" 2>/dev/null || true)"
    [[ -n "$baseline" ]] || return 0

    # Collect the new IDs before touching the network: a branch that adds no row
    # pays nothing, which is most runs of `make check`.
    local id
    local -a new_ids=()
    while read -r id; do
        [[ -n "$id" ]] || continue
        grep -q "<a id=\"$id\"></a>" <<<"$baseline" && continue
        new_ids+=("$id")
    done < <(anchor_ids <"$FILE")
    (( ${#new_ids[@]} )) || return 0

    # No remote, no network, no claims yet: nothing to check against. An empty
    # namespace is indistinguishable from an unreachable one here, and both mean
    # this clone cannot answer the question.
    local claims
    claims="$(git ls-remote origin "$QUEUE_ID_REF_NS/*" 2>/dev/null)" || return 0
    [[ -n "$claims" ]] || return 0

    # IDs below the namespace's lowest claim predate the allocator entirely.
    local floor
    floor="$(awk -F/ '$NF ~ /^Q[0-9]+$/ {
        n = substr($NF, 2) + 0
        if (min == 0 || n < min) min = n
    } END { print min + 0 }' <<<"$claims")"

    local -a allowed=()
    read -r -a allowed <<<"${BACKLOG_ALLOW_UNCLAIMED_ID:-}"

    local rc=0 ok a
    for id in "${new_ids[@]}"; do
        (( ${id#Q} >= floor )) || continue
        # Anchored to the end of the ls-remote line: an unanchored match reads a
        # claim on Q4010 as a claim on Q401, and the failure is a silent pass.
        grep -qE "[[:space:]]$QUEUE_ID_REF_NS/$id\$" <<<"$claims" && continue
        ok=0
        for a in ${allowed+"${allowed[@]}"}; do
            [[ "$a" == "$id" ]] && ok=1 && break
        done
        (( ok )) && continue
        if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
            printf '::error file=%s::%s is a new row here but holds no %s/%s claim, so it was never reserved and a concurrent session can be handed the same ID. Allocate it with make queue-id TITLE=... and renumber the row now, while it is still one file. See docs/development/queue-id-allocation.md\n' \
                "$FILE" "$id" "$QUEUE_ID_REF_NS" "$id"
        else
            printf 'lint-backlog: %s: %s is new here but holds no %s/%s claim.\n' \
                "$FILE" "$id" "$QUEUE_ID_REF_NS" "$id" >&2
            printf '  An unclaimed ID was never reserved, so a concurrent session can still be\n' >&2
            printf '  handed it — and the loser renumbers the row, its anchor, every cross-\n' >&2
            printf '  reference, the plan doc, the PR body and the commit subject. Allocate it:\n' >&2
            printf '    make queue-id TITLE=%sthe row title%s\n' "'" "'" >&2
            printf '  then renumber now, while the change is still one file. See\n' >&2
            printf '  docs/development/queue-id-allocation.md.\n' >&2
            printf '  Claimed from another clone or session? BACKLOG_ALLOW_UNCLAIMED_ID=%s\n' "$id" >&2
        fi
        rc=1
    done

    return "$rc"
}

flake_check_rc=0
check_flake_rows_preserved || flake_check_rc=1
check_no_resurrected_rows || flake_check_rc=1
check_new_ids_claimed || flake_check_rc=1

# Rule 9. Every `plan/NAME.md` path linked from a Queue row's Item or Notes cell.
# Deferred rows are deliberately excluded: a deferred residual does not hold a
# plan at ⚠️.
queue_plan_links() {
    awk '
        /^## Queue/ { in_queue = 1; next }
        /^## /      { in_queue = 0 }
        in_queue && /^\|/ {
            line = $0
            while (match(line, /\(plan\/[A-Za-z0-9._-]+\.md/)) {
                print substr(line, RSTART + 1, RLENGTH - 1)
                line = substr(line, RSTART + RLENGTH)
            }
        }
    ' | sort -u
}

# progress_status PLAN < FILE — the Progress table's Status cell for the row
# linking PLAN, or "" when no such row exists.
progress_status() {
    awk -F'|' -v want="($1" '
        /^## Progress/ { in_progress = 1; next }
        /^## /         { in_progress = 0 }
        in_progress && /^\|/ && index($2, want) {
            st = $4
            gsub(/[[:space:]]/, "", st)
            print st
            exit
        }
    '
}

check_progress_rederived() {
    local baseline_ref="" baseline="" rel
    rel="$(backlog_relpath)"
    [[ -n "$rel" ]] || return 0
    baseline_ref="$(baseline_ref)"
    [[ -n "$baseline_ref" ]] || return 0

    baseline="$(git show "$baseline_ref:$rel" 2>/dev/null || true)"
    [[ -n "$baseline" ]] || return 0

    local -a allowed=()
    read -r -a allowed <<<"${BACKLOG_ALLOW_PROGRESS_STALE:-}"

    local plan stale=0 st ok a
    # Plans the baseline's Queue pointed at that the current Queue no longer does.
    while read -r plan; do
        [[ -n "$plan" ]] || continue
        st="$(progress_status "$plan" <"$FILE")"
        # No Progress row, or already re-derived to ✅ -> nothing owed.
        [[ "$st" == "⚠️" ]] || continue
        ok=0
        for a in ${allowed+"${allowed[@]}"}; do
            [[ "$a" == "$plan" ]] && ok=1 && break
        done
        (( ok )) && continue
        if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
            printf '::error file=%s::the last Queue row pointing at %s is gone, but its Progress row is still ⚠️; a plan with only deferred residuals is ✅. Flip it in this same edit. See docs/development/maintaining-backlog.md#-means-an-open-queue-row-remains--deferred-residuals-dont-count\n' \
                "$FILE" "$plan"
        else
            printf 'lint-backlog: %s: the last Queue row pointing at %s is gone (it was in %s),\n' "$FILE" "$plan" "$baseline_ref" >&2
            printf '  but its Progress row is still ⚠️. ⚠️ means an open *Queue* row remains;\n' >&2
            printf '  deferred residuals do not count, so the row is now ✅. Flip it in this same\n' >&2
            printf '  edit — the Progress table is only ever re-derived by hand. See\n' >&2
            printf '  docs/development/maintaining-backlog.md#-means-an-open-queue-row-remains--deferred-residuals-dont-count.\n' >&2
            printf '  Was the vanished row only *citing* that plan, with real work left? BACKLOG_ALLOW_PROGRESS_STALE=%s\n' "$plan" >&2
        fi
        stale=1
    done < <(comm -23 <(queue_plan_links <<<"$baseline") <(queue_plan_links <"$FILE"))

    return "$stale"
}

progress_check_rc=0
check_progress_rederived || progress_check_rc=1

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

# Strip HTML tags and Markdown link syntax so a row reads plainly in a message.
function plain(cell,    s) {
    s = cell
    gsub(/<[^>]*>/, "", s)
    gsub(/\[|\]\([^)]*\)/, "", s)
    return trim(s)
}

# Every backticked token in a Labels cell must appear on the **Labels:** line.
# A cell with no backticks carries no vocabulary — table headers and separators
# land here too.
function check_labels(who, cell,    rest, tok) {
    rest = cell
    while (match(rest, /`[^`]+`/)) {
        tok = substr(rest, RSTART + 1, RLENGTH - 2)
        if (!seen_labels_line) {
            if (!warned_no_decl) {
                warned_no_decl = 1
                fail("rows carry labels but no **Labels:** line declares the vocabulary")
            }
        } else if (!(tok in declared)) {
            fail(who " uses undeclared label `" tok "`; declared: " declared_list)
        }
        rest = substr(rest, RSTART + RLENGTH)
    }
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
    fail("old format: drop the Next ID counter; allocate with scripts/docs/alloc-queue-id.sh (a file-local counter conflicts by construction under concurrent sessions)")
}

/^Last touched:/ {
    fail("old format: drop the Last touched line; use git log -1 --format=%as -- " file)
}

# The declared vocabulary. Parenthetical glosses on the -gate labels carry their
# own backticked link text, so drop link constructs before reading tokens.
/^\*\*Labels:\*\*/ {
    seen_labels_line = 1
    decl = $0
    gsub(/\[[^]]*\]\([^)]*\)/, "", decl)
    while (match(decl, /`[^`]+`/)) {
        tok = substr(decl, RSTART + 1, RLENGTH - 2)
        if (!(tok in declared)) {
            declared[tok] = 1
            declared_list = (declared_list == "" ? tok : declared_list " " tok)
        }
        decl = substr(decl, RSTART + RLENGTH)
    }
    next
}

/^## Progress/ { section = "Progress"; next }
/^## Queue/    { section = "Queue"; seen_queue = 1; next }
/^## Deferred/ { section = "Deferred"; next }
/^## /         { section = "" }

# Progress rows carry no ID, and their Labels cell is one field earlier.
section == "Progress" && /^\|/ {
    check_labels(plain($2), $3)
}

section == "Queue" && /^\|/ {
    id = parse_id($2)
    if (id == "") next
    register_id(id, section)
    check_labels(id, $4)
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
    check_labels(id, $4)
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

if (( ${awk_rc:-0} || flake_check_rc || progress_check_rc )); then
    exit 1
fi

printf 'lint-backlog: ok (%s)\n' "$FILE"
