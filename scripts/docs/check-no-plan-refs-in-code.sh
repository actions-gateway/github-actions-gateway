#!/usr/bin/env bash
#
# check-no-plan-refs-in-code.sh — keep code decoupled from ephemeral plan docs.
#
# Plans are process artifacts that get archived over time (the plan tree's
# active directory -> its archive subdirectory). A code comment that path-links
# a plan rots the moment that plan is archived, and "fixing" the path turns a
# docs-only archival into a code change — which re-triggers the heavy
# path-gated CI (e2e / integration / trivy), the exact tax this guard exists to
# avoid. Cite the durable layer instead: a design/operations doc, or a stable
# Q-ID / appendix §-ref (those survive archival untouched).
#
# Two rules, because the languages differ in what they legitimately do with a
# plan file:
#
#   Go            no legitimate citation, so any path into the plan tree is
#                 rejected anywhere in the file, comment or string literal.
#                 The one exception is the plan index README, which never
#                 moves: the merge driver that resolves it is Go and names it
#                 as the file it merges.
#
#   Shell and     tooling reads plan files as data: a workflow `paths:` filter
#   workflows     names one, and a script may rewrite one. Those are values,
#                 not citations, and a value that moves breaks loudly instead
#                 of rotting into stale prose. So only *comment* text is
#                 scanned, and only a plan *file* path — the thing archival
#                 actually moves. A bare directory reference and the plan
#                 index README survive archival untouched and are left alone.
#
# A comment that must name a plan file — because that file is what the script
# operates on — opts out inline with a `no-plan-refs: <reason>` marker on the
# same line. One line, visible in the diff, silencing exactly that line.
#
# Stable IDs and §-refs in prose ("Q88", "§H.10") are fine and encouraged. See
# docs/development/maintaining-backlog.md#archiving-completed-plan-docs.
#
set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which the test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT" || exit 2

# Both lists cover present files — tracked or untracked-and-not-gitignored — so
# a brand-new script, workflow or Go file is scanned by its own first `make
# check` (Q619). Command substitution, not `mapfile < <(...)`: it keeps the
# selection under `set -o pipefail`, so a failing `git ls-files` aborts the gate
# rather than reducing it to a silently green empty file set.

# Go sources, excluding vendored trees.
go_files=()
selected="$(git_candidates '*.go' ':!:**/vendor/**' ':!:vendor/**' | select_present_files)"
if [[ -n "$selected" ]]; then
    mapfile -t go_files <<<"$selected"
fi

# Shell scripts and GitHub workflows, excluding vendored trees.
comment_files=()
selected="$(git_candidates '*.sh' '.github/workflows/*.yml' '.github/workflows/*.yaml' \
    ':!:**/vendor/**' ':!:vendor/**' | select_present_files)"
if [[ -n "$selected" ]]; then
    mapfile -t comment_files <<<"$selected"
fi

# The plan index is exempt in Go for the same reason it is in a shell comment,
# below: it never moves, archival only re-bases a row inside it, so naming it
# cannot rot. Everything else in the plan tree stays rejected in Go anywhere in
# the file, comment or string literal. The merge driver for that index is Go
# (Q1046), which is what first needed this.
go_hits=""
if (( ${#go_files[@]} > 0 )); then
    go_hits="$(grep -nE 'docs/plan/|\.\./plan/' -- "${go_files[@]}" \
        | grep -vE '(docs|\.\.)/plan/README\.md' || true)"
fi

# The regexes are literal inside the awk program on purpose: passing one
# through `awk -v` would run it through awk's escape processing first, turning
# `\.` into a bare `.` that matches any character.
comment_hits=""
if (( ${#comment_files[@]} > 0 )); then
    comment_hits="$(awk '
        {
            # Comment text only. A plan path anywhere else on the line is a
            # value — a `paths:` filter entry, a file the script reads — and
            # naming a file you actually open is not a stale citation.
            if ($0 ~ /^[[:space:]]*#/) {
                comment = $0
            } else if (match($0, /[[:space:]]#/)) {
                comment = substr($0, RSTART)
            } else {
                next
            }
            if (comment ~ /no-plan-refs:/) next
            # The index never moves; archival re-bases a row inside it.
            gsub(/(docs|\.\.)\/plan\/README\.md/, "", comment)
            if (comment ~ /(docs|\.\.)\/plan\/(archive\/)?[A-Za-z0-9_.-]+\.md/) {
                printf "%s:%d:%s\n", FILENAME, FNR, $0
            }
        }
    ' "${comment_files[@]}" || true)"
fi

if [[ -n "$go_hits" || -n "$comment_hits" ]]; then
    {
        if [[ -n "$go_hits" ]]; then
            printf 'check-no-plan-refs-in-code: Go code references plan docs by path:\n'
            printf '%s\n' "$go_hits"
        fi
        if [[ -n "$comment_hits" ]]; then
            printf 'check-no-plan-refs-in-code: shell/workflow comments cite plan docs by path:\n'
            printf '%s\n' "$comment_hits"
        fi
        printf '\nPlans get archived; a plan path in code rots on archival and forces a code\n'
        printf 'edit (re-triggering heavy CI) during a docs-only move. Cite a durable doc\n'
        printf '(design/operations) or a stable Q-ID / appendix §-ref instead. A comment\n'
        printf 'that must name a plan file the script itself operates on may opt out with a\n'
        printf 'no-plan-refs: <reason> marker on the same line. See\n'
        printf 'docs/development/maintaining-backlog.md#archiving-completed-plan-docs\n'
    } >&2
    exit 1
fi

printf 'check-no-plan-refs-in-code: ok (%d Go files, %d shell/workflow files, no plan path references)\n' \
    "${#go_files[@]}" "${#comment_files[@]}"
