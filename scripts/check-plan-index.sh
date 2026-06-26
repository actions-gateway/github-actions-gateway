#!/usr/bin/env bash
#
# check-plan-index.sh — keep docs/plan/README.md honest about what's still open.
#
# Invariant: every plan listed in the *active* (non-Archive) part of
# docs/plan/README.md must still be referenced by docs/STATUS.md — a Progress-table
# row or a Queue/Deferred item — UNLESS its README row is marked ⓘ (informational:
# ongoing spec / strategy / research with no progress to track).
#
# A ✅/⚠️ plan that STATUS.md no longer references is closed work that was never
# archived. That is exactly the drift that makes the plan index read as "lots
# still open" when it isn't. The fix is to archive it the moment its last STATUS
# reference is removed — see
# docs/development/maintaining-backlog.md#archiving-completed-plan-docs — not to
# wait for a periodic audit. This check makes the omission fail fast.
#
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
readme="$repo_root/docs/plan/README.md"
status="$repo_root/docs/STATUS.md"

for f in "$readme" "$status"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-plan-index: required file not found: %s\n' "$f" >&2
        exit 2
    fi
done

# Active-index plan docs: the column-1 link of each table row before the
# "## Archive" heading, excluding ⓘ-marked rows. The link target is a bare
# "<name>.md" (no slash); archive rows link "archive/<name>.md" and live after
# the cutoff anyway.
mapfile -t plans < <(awk '
    /^## Archive/ { exit }
    /^\| \[/ && $0 !~ /ⓘ/ {
        # String regex (not /.../) so the "/" in the class cannot terminate it.
        if (match($0, "\\]\\([^/):]+\\.md\\)")) {
            print substr($0, RSTART + 2, RLENGTH - 3)
        }
    }
' "$readme" | sort -u)

candidates=()
for plan in "${plans[@]}"; do
    if ! grep -qF "$plan" "$status"; then
        candidates+=("$plan")
    fi
done

if (( ${#candidates[@]} > 0 )); then
    {
        printf 'check-plan-index: %d active plan(s) in docs/plan/README.md are no longer referenced by docs/STATUS.md.\n' "${#candidates[@]}"
        printf 'Archive each (git mv to docs/plan/archive/, move its README row to the Archive table, rebase its links)\n'
        printf 'or — if it is ongoing spec/strategy/research — mark its README row ⓘ. See\n'
        printf 'docs/development/maintaining-backlog.md#archiving-completed-plan-docs\n'
        for c in "${candidates[@]}"; do
            printf '  - docs/plan/%s\n' "$c"
        done
    } >&2
    exit 1
fi

printf 'check-plan-index: ok (%d active plans, all STATUS-referenced or ⓘ)\n' "${#plans[@]}"
