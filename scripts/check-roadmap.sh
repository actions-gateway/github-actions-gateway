#!/usr/bin/env bash
#
# check-roadmap.sh — keep the public roadmap honest against the backlog.
#
# docs/roadmap.md is adopter-facing narrative; docs/STATUS.md is the terse
# internal backlog. Neither can be generated from the other, so they drift: a
# 2026-07-25 audit found six of seven "In progress / near-term" roadmap items
# had already shipped, some of them release-frozen into published docs.
#
# The signal that makes this mechanical: this repo *deletes* a Queue row when
# its work ships (git is the archive). So a roadmap bullet naming a Q-ID that
# no longer exists in STATUS.md is an exact, zero-false-negative indicator that
# the item shipped and the bullet belongs under "Available now".
#
# Each forward-looking bullet therefore carries an invisible annotation naming
# the backlog rows behind it:
#
#     - **Capacity-aware job intake.** <!-- q:Q405,Q406 --> Additional opt-in …
#
# HTML comments render nowhere, on github.com or the MkDocs site.
#
# Rules:
#   1. Every top-level bullet under "In progress / near-term" and under
#      "Exploring / longer-term" carries a `<!-- q:QN[,QM…] -->` annotation.
#   2. Every annotated ID resolves to a row in STATUS.md. A dangling ID means
#      the work shipped — move the bullet to "Available now", or drop just that
#      ID when only part of a multi-item bullet shipped.
#   3. A near-term bullet names at least one row that is in the **Queue** (an
#      all-Deferred bullet was parked and belongs under "Exploring").
#   4. An exploring bullet names at least one row that is in **Deferred** (an
#      ID that moved into the Queue is active work and belongs under
#      "In progress / near-term").
#
# "Available now" is deliberately ungated: it describes shipped capability in
# editorial prose, with no backlog row left to point at.
#
# Usage:
#   check-roadmap.sh [path/to/roadmap.md] [path/to/STATUS.md]

set -euo pipefail

NEAR_TERM_HEADING="In progress / near-term"
EXPLORING_HEADING="Exploring / longer-term"

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
ROADMAP="${1:-$repo_root/docs/roadmap.md}"
STATUS="${2:-$repo_root/docs/STATUS.md}"

for f in "$ROADMAP" "$STATUS"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-roadmap: file not found: %s\n' "$f" >&2
        exit 2
    fi
done

# IDs in a given STATUS.md table. Rows are `| <a id="QN"></a>QN | … |` and the
# two tables are delimited by their `## Queue` / `## Deferred` headings.
status_ids() {
    local heading="$1"
    awk -v heading="^## $heading\$" '
        $0 ~ heading { in_section = 1; next }
        /^## /       { in_section = 0 }
        in_section && /^\| *<a id="Q[0-9]+"><\/a>/ {
            # `<a id="Q408">` — skip the 7-char `<a id="` prefix and the
            # trailing `">`, leaving the bare ID.
            if (match($0, /<a id="Q[0-9]+">/)) {
                print substr($0, RSTART + 7, RLENGTH - 9)
            }
        }
    ' "$STATUS"
}

queue_ids="$(status_ids Queue)"
deferred_ids="$(status_ids Deferred)"

if [[ -z "$queue_ids" && -z "$deferred_ids" ]]; then
    printf 'check-roadmap: parsed no Q-IDs from %s — the table format changed?\n' "$STATUS" >&2
    exit 2
fi

in_list() {
    local needle="$1" haystack="$2"
    grep -qx -- "$needle" <<<"$haystack"
}

# Emit one `<section>\t<line>\t<bullet label>\t<annotation ids>` record per
# top-level bullet in the two gated sections. A bullet runs from its `- ` line
# to the next top-level bullet or the next heading, so an annotation placed on
# any of its continuation lines still counts.
bullets() {
    awk -v near="^## $NEAR_TERM_HEADING\$" -v exploring="^## $EXPLORING_HEADING\$" '
        function flush() {
            if (label != "") printf "%s\t%d\t%s\t%s\n", section, line_no, label, ids
            label = ""; ids = ""
        }
        $0 ~ near      { flush(); section = "near-term"; next }
        $0 ~ exploring { flush(); section = "exploring"; next }
        /^## /         { flush(); section = ""; next }
        section == ""  { next }
        /^- / {
            flush()
            line_no = NR
            label = $0
            sub(/^- +/, "", label)
            sub(/\*\*/, "", label)
            sub(/\*\*.*$/, "", label)
        }
        label != "" && /<!-- *q:/ {
            annotation = $0
            sub(/^.*<!-- *q:/, "", annotation)
            sub(/-->.*$/, "", annotation)
            gsub(/[[:space:]]/, "", annotation)
            ids = ids (ids == "" ? "" : ",") annotation
        }
        END { flush() }
    ' "$ROADMAP"
}

fail=0
checked=0

report() {
    printf 'check-roadmap: %s:%s: %s\n' "${ROADMAP##*/}" "$1" "$2" >&2
    fail=1
}

while IFS=$'\t' read -r section line_no label ids; do
    [[ -n "$section" ]] || continue
    checked=$((checked + 1))

    if [[ -z "$ids" ]]; then
        report "$line_no" "\"$label\" has no <!-- q:QN --> annotation. Name the backlog row(s) behind it so this bullet fails when they ship."
        continue
    fi

    local_in_queue=0
    local_in_deferred=0
    IFS=',' read -r -a id_list <<<"$ids"
    for id in "${id_list[@]}"; do
        [[ -n "$id" ]] || continue
        if [[ ! "$id" =~ ^Q[0-9]+$ ]]; then
            report "$line_no" "\"$label\" annotation \"$id\" is not a Q-ID."
            continue
        fi
        if in_list "$id" "$queue_ids"; then
            local_in_queue=1
        elif in_list "$id" "$deferred_ids"; then
            local_in_deferred=1
        else
            report "$line_no" "\"$label\" names $id, which no longer exists in STATUS.md — the row was deleted, so the work shipped. Move this bullet to \"Available now\", or drop $id if only part of it shipped."
        fi
    done

    if [[ "$section" == "near-term" ]] && (( ! local_in_queue && local_in_deferred )); then
        report "$line_no" "\"$label\" names only Deferred rows — it was parked. Move it to \"$EXPLORING_HEADING\"."
    fi

    if [[ "$section" == "exploring" ]] && (( ! local_in_deferred && local_in_queue )); then
        report "$line_no" "\"$label\" names only Queue rows — it is active work. Move it to \"$NEAR_TERM_HEADING\"."
    fi
done < <(bullets)

if (( checked == 0 )); then
    printf 'check-roadmap: found no bullets under "%s" or "%s" in %s — the headings changed?\n' \
        "$NEAR_TERM_HEADING" "$EXPLORING_HEADING" "$ROADMAP" >&2
    exit 2
fi

if (( fail )); then
    printf 'check-roadmap: roadmap and backlog disagree (see above). Reconcile per docs/development/doc-update-matrix.md.\n' >&2
    exit 1
fi

printf 'check-roadmap: ok (%d forward-looking bullet(s) all backed by live STATUS.md rows)\n' "$checked"
