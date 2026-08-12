#!/usr/bin/env bash
#
# check-plan-index.sh — keep docs/plan/README.md and the docs/plan/ tree in sync,
# in both directions. Two invariants, both fail-fast so drift can't ship:
#
#   1. README → STATUS.  Every plan in the *active* (non-Archive) part of
#      docs/plan/README.md must still be referenced by docs/STATUS.md — a
#      Progress-table row or a Queue/Deferred item — UNLESS its README row is
#      marked ⓘ (informational: ongoing spec / strategy / research with no
#      progress to track). A ✅/⚠️ plan that STATUS.md no longer references is
#      closed work that was never archived: exactly the drift that makes the
#      plan index read as "lots still open" when it isn't. The fix is to archive
#      it the moment its last STATUS reference is removed — see
#      docs/development/maintaining-backlog.md#archiving-completed-plan-docs.
#
#   2. disk ↔ README.  Every plan file on disk must have a row in README, in the
#      matching section (docs/plan/*.md → an active row; docs/plan/archive/*.md →
#      an Archive row), and every README row must point at a file that exists.
#      A plan doc on disk but missing from the index is invisible —
#      q264-scale-set-protocol.md was, for its whole life (Q290). A README row
#      whose file is gone is a stale link. Direction 1 only sees plans that made
#      it into README; this direction catches the ones that never did.
#
#   3. Status cell ↔ Queue row. In an active row's Status cell (column 3), a
#      QNNN that is still a row in docs/STATUS.md must be written as a link to
#      its anchor, and one that is not must be written bare. The bare form is
#      the closed form — maintaining-backlog.md's closing protocol already says
#      to de-link an ID when its row goes — so requiring the link while the row
#      is live is what makes that step forced rather than remembered: the anchor
#      dies with the row and `make doc-links` says so, on the cell that now
#      summarizes work that finished. Nothing else can see this. A plan's rollup
#      only changes when its last Queue row closes, and the worker closing that
#      row has no way to know it was the last (Q800: three stale cells across
#      1.4 and 1.5, every individual row updated correctly).
#
#      Column 3 only. A bare ID in the Scope cell describes what the plan was
#      about and stays true after the row closes; the Status cell is the claim
#      that goes stale.
#
set -euo pipefail
shopt -s inherit_errexit

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
readme="$repo_root/docs/plan/README.md"
status="$repo_root/docs/STATUS.md"
plan_dir="$repo_root/docs/plan"

for f in "$readme" "$status"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-plan-index: required file not found: %s\n' "$f" >&2
        exit 2
    fi
done

# --- README extraction -------------------------------------------------------
# Each table row is `| [text](target) | ... |`; the column-1 link is the plan.
# Active rows (before "## Archive") link a bare "<name>.md" (no slash); Archive
# rows (after it) link "archive/<name>.md". Regexes are strings (not /.../) so
# the "/" in a character class cannot terminate them.

# Active plans EXCLUDING ⓘ rows — these must stay STATUS-referenced (invariant 1).
mapfile -t status_checked < <(awk '
    /^## Archive/ { exit }
    /^\| \[/ && $0 !~ /ⓘ/ {
        if (match($0, "\\]\\([^/):]+\\.md\\)")) {
            print substr($0, RSTART + 2, RLENGTH - 3)
        }
    }
' "$readme" | sort -u)

# Active plans INCLUDING ⓘ rows — the full active index (invariant 2, active side).
mapfile -t indexed_active < <(awk '
    /^## Archive/ { exit }
    /^\| \[/ {
        if (match($0, "\\]\\([^/):]+\\.md\\)")) {
            print substr($0, RSTART + 2, RLENGTH - 3)
        }
    }
' "$readme" | sort -u)

# Archived plans — column-1 "archive/<name>.md" links after "## Archive"
# (invariant 2, archive side). Basename only, to compare against the disk tree.
mapfile -t indexed_archive < <(awk '
    seen && /^\| \[/ {
        if (match($0, "\\]\\(archive/[^):]+\\.md\\)")) {
            s = substr($0, RSTART + 2, RLENGTH - 3)
            sub(/^archive\//, "", s)
            print s
        }
    }
    /^## Archive/ { seen = 1 }
' "$readme" | sort -u)

# --- disk enumeration --------------------------------------------------------
mapfile -t disk_active < <(find "$plan_dir" -maxdepth 1 -name '*.md' ! -name 'README.md' -exec basename {} \; | sort -u)
mapfile -t disk_archive < <(find "$plan_dir/archive" -maxdepth 1 -name '*.md' -exec basename {} \; 2>/dev/null | sort -u)

# --- helpers -----------------------------------------------------------------
# contains <needle> <haystack...> — exact-match membership test.
contains() {
    local needle="$1"; shift
    local x
    for x in "$@"; do
        [[ "$x" == "$needle" ]] && return 0
    done
    return 1
}

errors=0

# Invariant 1: active README plans (non-ⓘ) must be STATUS-referenced.
unref=()
for plan in "${status_checked[@]}"; do
    grep -qF "$plan" "$status" || unref+=("$plan")
done
if (( ${#unref[@]} > 0 )); then
    errors=1
    {
        printf 'check-plan-index: %d active plan(s) in docs/plan/README.md are no longer referenced by docs/STATUS.md.\n' "${#unref[@]}"
        printf 'Archive each (git mv to docs/plan/archive/, move its README row to the Archive table, rebase its links)\n'
        printf 'or — if it is ongoing spec/strategy/research — mark its README row ⓘ. See\n'
        printf 'docs/development/maintaining-backlog.md#archiving-completed-plan-docs\n'
        for c in "${unref[@]}"; do printf '  - docs/plan/%s\n' "$c"; done
    } >&2
fi

# Invariant 2a (disk → README): every plan file has a row in the matching section.
missing_active=()
for f in "${disk_active[@]}"; do
    contains "$f" "${indexed_active[@]}" || missing_active+=("$f")
done
missing_archive=()
for f in "${disk_archive[@]}"; do
    contains "$f" "${indexed_archive[@]}" || missing_archive+=("$f")
done
if (( ${#missing_active[@]} + ${#missing_archive[@]} > 0 )); then
    errors=1
    {
        printf 'check-plan-index: %d plan file(s) on disk have no row in docs/plan/README.md.\n' "$(( ${#missing_active[@]} + ${#missing_archive[@]} ))"
        printf 'Add a row so the plan is visible in the index — see the Conventions at the end of docs/plan/README.md.\n'
        for c in "${missing_active[@]}"; do printf '  - docs/plan/%s (needs an active-section row)\n' "$c"; done
        for c in "${missing_archive[@]}"; do printf '  - docs/plan/archive/%s (needs an Archive-section row)\n' "$c"; done
    } >&2
fi

# Invariant 2b (README → disk): every row points at a file that exists.
dangling_active=()
for f in "${indexed_active[@]}"; do
    contains "$f" "${disk_active[@]}" || dangling_active+=("$f")
done
dangling_archive=()
for f in "${indexed_archive[@]}"; do
    contains "$f" "${disk_archive[@]}" || dangling_archive+=("$f")
done
if (( ${#dangling_active[@]} + ${#dangling_archive[@]} > 0 )); then
    errors=1
    {
        printf 'check-plan-index: %d docs/plan/README.md row(s) link a file that does not exist.\n' "$(( ${#dangling_active[@]} + ${#dangling_archive[@]} ))"
        printf 'Fix the link target, or remove the row if the plan is gone.\n'
        for c in "${dangling_active[@]}"; do printf '  - row links %s — no such file in docs/plan/\n' "$c"; done
        for c in "${dangling_archive[@]}"; do printf '  - row links archive/%s — no such file in docs/plan/archive/\n' "$c"; done
    } >&2
fi

# Invariant 3: a Status cell links a QNNN iff that row is still in STATUS.md.
# One awk pass over both files — STATUS.md first for the anchor set, then the
# active part of README. Escaped pipes are protected before the column split:
# a `\|` inside a cell shifts every column after it, which is the defect the
# backlog lint carried until Q625.
mapfile -t cell_findings < <(awk '
    FNR == NR {
        s = $0
        while (match(s, "<a id=\"Q[0-9]+\"></a>")) {
            anchor[substr(s, RSTART + 7, RLENGTH - 13)] = 1
            s = substr(s, RSTART + RLENGTH)
        }
        next
    }
    /^## Archive/ { archived = 1 }
    archived { next }
    /^\| \[/ {
        line = $0
        gsub(/\\\|/, "\001", line)
        if (split(line, col, "|") != 5) {
            print "shape\t" FNR "\t"
            next
        }
        cell = col[4]
        # Linked IDs first, removing each match so the bare scan cannot see it.
        while (match(cell, "\\[Q[0-9]+\\]\\(\\.\\./STATUS\\.md#Q[0-9]+\\)")) {
            split(substr(cell, RSTART, RLENGTH), part, "]")
            label = substr(part[1], 2)
            target = substr(part[2], index(part[2], "#") + 1)
            sub(/\)$/, "", target)
            if (label != target) {
                print "mismatch\t" FNR "\t" label " -> #" target
            } else if (!(label in anchor)) {
                print "dangling\t" FNR "\t" label
            }
            cell = substr(cell, 1, RSTART - 1) substr(cell, RSTART + RLENGTH)
        }
        while (match(cell, "Q[0-9]+")) {
            id = substr(cell, RSTART, RLENGTH)
            if (id in anchor) {
                print "unlinked\t" FNR "\t" id
            }
            cell = substr(cell, RSTART + RLENGTH)
        }
    }
' "$status" "$readme")

shape=() dangling=() unlinked=() mismatch=()
for f in "${cell_findings[@]}"; do
    IFS=$'\t' read -r kind lineno id <<<"$f"
    case "$kind" in
    shape) shape+=("$lineno") ;;
    dangling) dangling+=("$lineno"$'\t'"$id") ;;
    unlinked) unlinked+=("$lineno"$'\t'"$id") ;;
    mismatch) mismatch+=("$lineno"$'\t'"$id") ;;
    esac
done

if (( ${#shape[@]} > 0 )); then
    errors=1
    {
        printf 'check-plan-index: %d docs/plan/README.md plan row(s) do not have exactly three columns.\n' "${#shape[@]}"
        printf 'Every plan row carries three: plan, scope, status. A row missing its Status cell shows no\n'
        printf 'status at all, and leaves nothing for the Queue-row check below to read.\n'
        for c in "${shape[@]}"; do printf '  - docs/plan/README.md:%s\n' "$c"; done
    } >&2
fi

if (( ${#dangling[@]} > 0 )); then
    errors=1
    {
        printf 'check-plan-index: %d Status cell(s) link a Queue row that no longer exists.\n' "${#dangling[@]}"
        printf 'The row closed, so the cell summarizes work that has finished and is stale by construction.\n'
        printf 'Re-read it against the plan doc, then write the ID bare — the closed form — or drop it. See\n'
        printf 'docs/development/maintaining-backlog.md#closing-a-row-what-else-moves\n'
        for c in "${dangling[@]}"; do
            IFS=$'\t' read -r l i <<<"$c"
            printf '  - docs/plan/README.md:%s links %s, which has no <a id> in docs/STATUS.md\n' "$l" "$i"
        done
    } >&2
fi

if (( ${#unlinked[@]} > 0 )); then
    errors=1
    {
        printf 'check-plan-index: %d Status cell(s) name a live Queue row without linking it.\n' "${#unlinked[@]}"
        printf 'A bare ID is the closed form and nothing notices when it goes stale. Write [QNNN](../STATUS.md#QNNN)\n'
        printf 'so closing the row breaks the anchor and forces the cell to be re-read.\n'
        for c in "${unlinked[@]}"; do
            IFS=$'\t' read -r l i <<<"$c"
            printf '  - docs/plan/README.md:%s names %s, which is still a row in docs/STATUS.md\n' "$l" "$i"
        done
    } >&2
fi

if (( ${#mismatch[@]} > 0 )); then
    errors=1
    {
        printf 'check-plan-index: %d Status cell link(s) label one Queue row and point at another.\n' "${#mismatch[@]}"
        printf 'doc-links validates the target only, so a mismatched label reads as correct forever.\n'
        for c in "${mismatch[@]}"; do
            IFS=$'\t' read -r l i <<<"$c"
            printf '  - docs/plan/README.md:%s links %s\n' "$l" "$i"
        done
    } >&2
fi

if (( errors )); then
    exit 1
fi

printf 'check-plan-index: ok (%d active, %d archived; all STATUS-referenced or ⓘ, all indexed both ways, every Status-cell QNNN linked iff its row is live)\n' \
    "${#indexed_active[@]}" "${#indexed_archive[@]}"
