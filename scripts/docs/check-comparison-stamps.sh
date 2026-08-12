#!/usr/bin/env bash
#
# check-comparison-stamps.sh — fail when a competitor-side cell in the GAG vs ARC
# comparison table renders a verdict without an ARC version and a measurement
# date (Q801).
#
# The table is a green-check/red-X verdict table, and a verdict table needs a
# definite cell in every row. The working notes it was built from had marked most
# competitor-side facts unverified, and the format had nowhere to put "we believe
# this but have not checked it" — so eleven unverified ARC-side facts shipped as
# red X's, none carrying an ARC version or a date. Two then went false at datable
# upstream releases with nothing going red. The settled format gives the column a
# third state and this gate enforces it: no version and date, no verdict.
#
# The rule, per ARC-column cell:
#   * a verdict (.gag-yes / .gag-no) requires exactly one <span class="gag-asof">
#     holding exactly one version token and exactly one ISO date;
#   * the unverified state (.gag-unverified) carries neither a verdict icon nor a
#     stamp, because a stamp is what a verdict rests on;
#   * every cell is one or the other. A cell that is neither is a row the format
#     cannot express, which is the failure this gate exists to catch.
#
# Only the competitor column is checked. A wrong claim about GAG's own behavior
# goes red in a test, which is the gate a citation would only restate
# (docs/development/documentation-standards.md#an-upstream-behavior-claim-cites-a-measurement).
#
# Staleness is deliberately NOT a failure. A gate that goes red on a date nobody
# committed turns main red with no change to revert, so the age of each stamp is
# reported by `--report` instead and read at release pre-flight
# (docs/operations/release.md#1-pre-flight).
#
# Usage:
#   check-comparison-stamps.sh [--report] [file...]
#
# With no file named, docs/why-gag.md. Exits 1 on any finding, and 2 when the
# table could not be located or holds no body rows — an empty scan cannot tell
# "every cell is stamped" from "my parser no longer sees the table".
#
# Runs under `make comparison-stamps-check` (part of `make check`).
# Assertions: scripts/docs/check-comparison-stamps-test.sh.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

# The one page that renders competitor claims as verdicts. A second such table
# belongs here the day it is written.
DEFAULT_FILES=('docs/why-gag.md')

REPORT=0
while (($# > 0)); do
    case "$1" in
    --report)
        REPORT=1
        ;;
    --)
        shift
        break
        ;;
    -*)
        printf 'check-comparison-stamps.sh: unknown argument: %s\n' "$1" >&2
        exit 2
        ;;
    *)
        break
        ;;
    esac
    shift
done

files=()
if (($# > 0)); then
    files=("$@")
else
    cd "$REPO_ROOT"
    files=("${DEFAULT_FILES[@]}")
fi

for f in "${files[@]}"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-comparison-stamps: file not found: %s\n' "$f" >&2
        exit 2
    fi
done

today="$(date -u +%F)"

# Emit one TAB-separated `line-number, capability, competitor-cell` record per
# body row of the comparison table. The table is found by its header rather than
# by position: a three-column header whose second cell names ARC. Escaped pipes
# are parked on \001 before the split so a cell may hold one, and restored after.
table_rows() {
    awk '
        {
            line = $0
            gsub(/\\\|/, "\001", line)
            if (line !~ /^[ \t]*\|/) { in_table = 0; next }

            n = split(line, cell, "|")
            # A leading and a trailing pipe make n columns arrive as n+2 fields.
            if (n != 5) { in_table = 0; next }
            for (i = 1; i <= n; i++) {
                gsub(/^[ \t]+|[ \t]+$/, "", cell[i])
                gsub(/\001/, "|", cell[i])
            }

            if (!in_table) {
                if (cell[3] ~ /^ARC/) { in_table = 1; seen_delim = 0 }
                next
            }
            # The delimiter row directly under the header.
            if (!seen_delim) {
                if (cell[2] ~ /^-+$/) seen_delim = 1
                else in_table = 0
                next
            }
            printf "%d\t%s\t%s\n", NR, cell[2], cell[3]
            rows++
        }
        END { if (rows == 0) exit 3 }
    ' "$1"
}

fail=0
verdicts=0
unverified=0
report_lines=()

for f in "${files[@]}"; do
    rows=""
    if ! rows="$(table_rows "$f")"; then
        printf 'check-comparison-stamps: %s: no comparison table found. This gate keys on a\n' "$f" >&2
        printf '                         three-column header whose second cell starts "ARC". If the\n' >&2
        printf '                         table moved or was reshaped, teach the parser its new shape —\n' >&2
        printf '                         do not drop the file, which would pass by checking nothing.\n' >&2
        exit 2
    fi

    file_rows=0
    while IFS=$'\t' read -r line_no capability cell; do
        [[ -n "$cell" ]] || continue
        file_rows=$((file_rows + 1))

        has_verdict=0
        [[ "$cell" == *".gag-yes"* || "$cell" == *".gag-no"* ]] && has_verdict=1
        has_unverified=0
        [[ "$cell" == *".gag-unverified"* ]] && has_unverified=1

        # `|| true`: grep exits 1 on no match, which is a finding here rather
        # than an error, and would otherwise abort the loop under `set -e`.
        stamps="$(grep -oE '<span class="gag-asof">[^<]*</span>' <<<"$cell" || true)"
        stamp_count=0
        [[ -n "$stamps" ]] && stamp_count="$(grep -c . <<<"$stamps")"

        if ((has_verdict && has_unverified)); then
            printf 'check-comparison-stamps: %s:%s: "%s" renders a verdict AND the unverified state\n' \
                "$f" "$line_no" "$capability" >&2
            fail=1
            continue
        fi

        if ((has_verdict)); then
            verdicts=$((verdicts + 1))
            if ((stamp_count != 1)); then
                printf 'check-comparison-stamps: %s:%s: "%s" renders a verdict with %s measurement stamp(s);\n' \
                    "$f" "$line_no" "$capability" "$stamp_count" >&2
                printf '                         a verdict needs exactly one <span class="gag-asof">VERSION · DATE</span>,\n' >&2
                printf '                         or it must render as unverified instead\n' >&2
                fail=1
                continue
            fi
            payload="${stamps#*\">}"
            payload="${payload%</span>}"
            version="$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' <<<"$payload" || true)"
            measured="$(grep -oE '[0-9]{4}-[0-9]{2}-[0-9]{2}' <<<"$payload" || true)"
            if [[ -z "$version" || -z "$measured" ]]; then
                printf 'check-comparison-stamps: %s:%s: "%s" stamp "%s" needs both an ARC version\n' \
                    "$f" "$line_no" "$capability" "$payload" >&2
                printf '                         (X.Y.Z) and a measurement date (YYYY-MM-DD)\n' >&2
                fail=1
                continue
            fi
            if [[ "$(grep -c . <<<"$version")" != 1 || "$(grep -c . <<<"$measured")" != 1 ]]; then
                printf 'check-comparison-stamps: %s:%s: "%s" stamp "%s" holds more than one version\n' \
                    "$f" "$line_no" "$capability" "$payload" >&2
                printf '                         or date, so which one the verdict rests on is ambiguous\n' >&2
                fail=1
                continue
            fi
            if [[ "$measured" > "$today" ]]; then
                printf 'check-comparison-stamps: %s:%s: "%s" is stamped %s, which is in the future\n' \
                    "$f" "$line_no" "$capability" "$measured" >&2
                fail=1
                continue
            fi
            report_lines+=("$measured"$'\t'"$version"$'\t'"$capability")
            continue
        fi

        if ((has_unverified)); then
            unverified=$((unverified + 1))
            if ((stamp_count > 0)); then
                printf 'check-comparison-stamps: %s:%s: "%s" is unverified but carries a measurement\n' \
                    "$f" "$line_no" "$capability" >&2
                printf '                         stamp. A stamp is what a verdict rests on — give the cell\n' >&2
                printf '                         its verdict back, or drop the stamp\n' >&2
                fail=1
            fi
            continue
        fi

        printf 'check-comparison-stamps: %s:%s: "%s" renders neither a verdict nor the unverified\n' \
            "$f" "$line_no" "$capability" >&2
        printf '                         state. Every competitor-side cell is one or the other\n' >&2
        fail=1
    done <<<"$rows"

    printf 'check-comparison-stamps: %s: %d competitor cell(s)\n' "$f" "$file_rows"
done

if ((fail)); then
    printf '\ncheck-comparison-stamps: a competitor claim is rendering as a verdict without the\n' >&2
    printf 'measurement it rests on. The format and the state it adds are documented in\n' >&2
    printf 'docs/development/documentation-standards.md#a-competitor-side-verdict-carries-its-own-stamp.\n' >&2
    exit 1
fi

if ((REPORT)); then
    printf '\nOldest stamps first, the re-check worklist for release pre-flight:\n'
    if ((${#report_lines[@]} > 0)); then
        printf '%s\n' "${report_lines[@]}" | LC_ALL=C sort
    else
        printf '  (no stamped verdicts)\n'
    fi
fi

printf 'check-comparison-stamps: ok (%d stamped verdict(s), %d unverified)\n' \
    "$verdicts" "$unverified"
