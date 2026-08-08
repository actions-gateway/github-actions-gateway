#!/usr/bin/env bash
#
# check-page-density.sh — catch the two page-density defects that a reader sees
# and a reviewer reading the source does not.
#
# Both come from one review of the marketing pages (documentation-standards.md
# § Cut before you polish), where 24 rounds of feedback collapsed into four
# causes. These are the two of the four a machine can judge; the other two,
# "does this block belong on this page" and "does this prose restate the diagram
# under it", need a reader.
#
#   1. An admonition wall. Six consecutive `!!!` blocks ran 94 lines on
#      why-gag.md. Because all six were styled identically, the most important
#      of them looked exactly like the four routing notes above it: repeating an
#      emphasis device destroys the emphasis. A heading between blocks resets
#      the run, which is the fix the rule wants anyway.
#
#   2. A component saying the same thing on two pages. index.md and why-gag.md
#      both carried a `.gag-stats` band whose first and third tiles matched
#      verbatim, number and lead text alike, so the comparison page read as a
#      repeat of the landing page. Only those two pages use these components, so
#      a duplicate lead is always a mistake rather than a coincidence.
#
# Thresholds are calibrated against the tree, not guessed. Measured 2026-08-08
# with the wall already removed: the highest admonition run anywhere in docs/ is
# 2, and no stat lead appears in more than one file. The limit of 3 therefore
# leaves a file's worth of headroom while still failing the 6-run that prompted
# this.
#
# Deliberately not checked: card-bullet text across pages. Calibration found two
# legitimate shared bullets (the Pod Security Admission and signed-images lines
# appear in a "Secure by default" card on both pages, which is fair), and a
# naive extractor also matches indented YAML inside fenced code blocks. A gate
# that needs a real Markdown parser belongs in devtools/, not here.
#
# Usage:
#   scripts/docs/check-page-density.sh [--limit N] [file ...]
#
# Exits non-zero on any finding. Findings print as `file:line: message`.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

ADMONITION_LIMIT=3

while (($# > 0)); do
    case "$1" in
    --limit)
        ADMONITION_LIMIT="$2"
        shift
        ;;
    --)
        shift
        break
        ;;
    -*)
        printf 'check-page-density.sh: unknown argument: %s\n' "$1" >&2
        exit 2
        ;;
    *)
        break
        ;;
    esac
    shift
done

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

md_files=()
if (($# > 0)); then
    md_files=("$@")
else
    # Command substitution rather than `mapfile < <(...)` so the selection stays
    # under `set -o pipefail`: a failing `git ls-files` aborts the gate instead
    # of quietly reducing it to "no files to check".
    selected="$(git_candidates 'docs/*.md' ':!:**/vendor/**' | select_present_files | LC_ALL=C sort)"
    if [[ -n "$selected" ]]; then
        mapfile -t md_files <<<"$selected"
    fi
fi

if ((${#md_files[@]} == 0)); then
    echo "check-page-density: no markdown files to check" >&2
    exit 0
fi

fail=0

# 1. Admonition runs. A fence toggles code state so an admonition quoted inside
# a code block is not counted; any ATX heading resets the run.
run_findings="$(
    awk -v limit="$ADMONITION_LIMIT" '
        FNR == 1 { run = 0; incode = 0 }
        /^(```|~~~)/ { incode = !incode; next }
        incode { next }
        /^#{1,6} / { run = 0; next }
        /^(!!!|\?\?\?)[[:space:]]/ {
            run++
            if (run == limit + 1) {
                printf "%s:%d: %d consecutive admonitions with no heading between them (limit %d). Repeating a callout destroys its emphasis; promote one to a heading.\n", FILENAME, FNR, run, limit
            }
            next
        }
    ' "${md_files[@]}"
)"

if [[ -n "$run_findings" ]]; then
    printf '%s\n' "$run_findings" >&2
    fail=1
fi

# 2. A stat-tile lead used on more than one page. Emitted as `text<TAB>file`,
# then grouped; the lead text is single-line in every current use.
dup_findings="$(
    awk '
        match($0, /class="gag-stat__lead">/) {
            rest = substr($0, RSTART + RLENGTH)
            end = index(rest, "</strong>")
            if (end > 0) {
                text = substr(rest, 1, end - 1)
                gsub(/^[[:space:]]+|[[:space:]]+$/, "", text)
                if (text != "") print text "\t" FILENAME
            }
        }
    ' "${md_files[@]}" | LC_ALL=C sort -u | awk -F'\t' '
        { if ($1 == prev) { files = files ", " $2; n++ } else { flush(); prev = $1; files = $2; n = 1 } }
        function flush() {
            if (n > 1) printf "%s: stat-tile lead \"%s\" is used on %d pages. A shared component saying the same thing on two pages reads as a repeat.\n", files, prev, n
        }
        END { flush() }
    '
)"

if [[ -n "$dup_findings" ]]; then
    printf '%s\n' "$dup_findings" >&2
    fail=1
fi

if ((fail)); then
    printf 'check-page-density: FAILED\n' >&2
    exit 1
fi

printf 'check-page-density: ok (%d markdown file(s), admonition-run limit %d)\n' \
    "${#md_files[@]}" "$ADMONITION_LIMIT"
