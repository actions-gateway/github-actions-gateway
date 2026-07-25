#!/usr/bin/env bash
#
# Unit tests for scripts/check-roadmap.sh — the roadmap/backlog coherence gate.
#
# The rule that earns the gate is "a Q-ID the roadmap names but STATUS.md no
# longer has": because done rows are deleted, that is an exact signal the work
# shipped. These fixtures pin that case plus the section-placement rules, so a
# future change to either file's format fails here rather than silently
# passing everything. Runs under `make check` (via `make scripts-test`).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
CHECK="$REPO_ROOT/scripts/check-roadmap.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# status QUEUE_IDS DEFERRED_IDS — write a STATUS.md holding the given
# space-separated IDs in each table. Echoes the file path.
status() {
    local queue="$1" deferred="$2" file="$WORKDIR/STATUS.md" id
    {
        printf '# Project Status\n\n**Next ID:** Q999\n\n## Queue\n\n'
        printf '| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
        for id in $queue; do
            printf '| <a id="%s"></a>%s | Thing | infra | 🔲 | S | note |\n' "$id" "$id"
        done
        printf '\n## Deferred\n\n'
        printf '| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
        for id in $deferred; do
            printf '| <a id="%s"></a>%s | Thing | infra | S | **Demand:** someone asks. |\n' "$id" "$id"
        done
    } >"$file"
    printf '%s\n' "$file"
}

# roadmap NEAR_BULLETS -- EXPLORING_BULLETS — write a roadmap.md with the given
# raw bullet lines in each gated section. Echoes the file path.
roadmap() {
    local file="$WORKDIR/roadmap.md" line in_exploring=0
    {
        printf '# Roadmap\n\n## Available now (1.0)\n\n'
        printf -- '- **Something shipped.** No annotation needed here.\n'
        printf '\n## In progress / near-term\n\n'
        for line in "$@"; do
            if [[ "$line" == "--" ]]; then
                printf '\n## Exploring / longer-term\n\n'
                in_exploring=1
                continue
            fi
            printf '%s\n' "$line"
        done
        (( in_exploring )) || printf '\n## Exploring / longer-term\n\n'
        printf '\n## How priorities are set\n\nFeedback drives the ordering.\n'
    } >"$file"
    printf '%s\n' "$file"
}

# expect NAME EXPECTED_RC ROADMAP STATUS [SUBSTRING] — run the gate and assert
# its exit code, and optionally that its stderr mentions SUBSTRING.
expect() {
    local name="$1" want_rc="$2" rm_file="$3" st_file="$4" needle="${5:-}"
    local out rc=0
    out="$("$CHECK" "$rm_file" "$st_file" 2>&1)" || rc=$?
    if (( rc != want_rc )); then
        printf 'FAIL %-34s rc=%d, want %d\n' "$name" "$rc" "$want_rc"
        printf '%s\n' "$out" | awk '{ print "       " $0 }'
        fails=$((fails + 1))
        return
    fi
    if [[ -n "$needle" ]] && ! grep -qF -- "$needle" <<<"$out"; then
        printf 'FAIL %-34s missing %q in output\n' "$name" "$needle"
        printf '%s\n' "$out" | awk '{ print "       " $0 }'
        fails=$((fails + 1))
        return
    fi
    printf 'ok   %-34s rc=%d\n' "$name" "$rc"
}

NEAR='- **Near thing.** <!-- q:Q1 --> Body text.'
EXPL='- **Far thing.** <!-- q:Q2 --> Body text.'

# The happy path: near-term backed by a Queue row, exploring by a Deferred row.
expect "clean: both sections backed" 0 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1" "Q2")"

# An "Available now" bullet carries no annotation and must not be gated.
expect "clean: available-now ungated" 0 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1" "Q2")"

# The drift this gate exists for: the row was deleted because the work shipped.
expect "dangling ID (row deleted)" 1 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "" "Q2")" \
    'no longer exists in STATUS.md'

# A bullet nobody annotated cannot be checked at all.
expect "missing annotation" 1 \
    "$(roadmap '- **Near thing.** Body text.' -- "$EXPL")" "$(status "Q1" "Q2")" \
    'has no <!-- q:QN --> annotation'

# Parked: the row moved to Deferred, so the bullet belongs under Exploring.
expect "near-term names only Deferred" 1 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "" "Q1 Q2")" \
    'Move it to "Exploring / longer-term"'

# Revived: the row moved into the Queue, so it is active work.
expect "exploring names only Queue" 1 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1 Q2" "")" \
    'Move it to "In progress / near-term"'

# A multi-item bullet stays green while any one of its rows is still open...
expect "multi-ID, all live" 0 \
    "$(roadmap '- **Near thing.** <!-- q:Q1,Q3 --> Body.' -- "$EXPL")" \
    "$(status "Q1 Q3" "Q2")"

# ...but a deleted row is still reported, so a half-shipped bullet gets edited.
expect "multi-ID, one shipped" 1 \
    "$(roadmap '- **Near thing.** <!-- q:Q1,Q3 --> Body.' -- "$EXPL")" \
    "$(status "Q1" "Q2")" 'names Q3'

# The annotation may sit on any line of the bullet, not just the first.
expect "annotation on continuation line" 0 \
    "$(roadmap '- **Near thing.** Body text runs on' '  and ends here. <!-- q:Q1 -->' -- "$EXPL")" \
    "$(status "Q1" "Q2")"

# A typo'd annotation must not silently pass as "unbacked but unchecked".
expect "non-Q-ID annotation" 1 \
    "$(roadmap '- **Near thing.** <!-- q:TODO --> Body.' -- "$EXPL")" \
    "$(status "Q1" "Q2")" 'is not a Q-ID'

# Format drift on either side is a hard error (rc 2), never a silent pass.
printf '# Roadmap\n\n## Something Else\n\n- **Orphan.** Body.\n' >"$WORKDIR/no-sections.md"
expect "roadmap headings renamed" 2 \
    "$WORKDIR/no-sections.md" "$(status "Q1" "Q2")" 'found no bullets'

printf '# Project Status\n\nNo tables here.\n' >"$WORKDIR/no-rows.md"
expect "STATUS tables unparseable" 2 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$WORKDIR/no-rows.md" 'parsed no Q-IDs'

expect "missing file" 2 "$WORKDIR/nope.md" "$(status "Q1" "Q2")" 'file not found'

if (( fails )); then
    printf '\ncheck-roadmap-test: %d assertion(s) failed\n' "$fails" >&2
    exit 1
fi

printf '\ncheck-roadmap-test: all assertions passed\n'
