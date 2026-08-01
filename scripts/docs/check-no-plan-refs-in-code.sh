#!/usr/bin/env bash
#
# check-no-plan-refs-in-code.sh — keep Go code decoupled from ephemeral plan docs.
#
# Go comments must not reference `docs/plan/` (or `../plan/`) *paths*. Plans are
# process artifacts that get archived over time (docs/plan/ -> docs/plan/archive/).
# A code comment that path-links a plan rots the moment that plan is archived, and
# "fixing" the path turns a docs-only archival into a code change — which
# re-triggers the heavy path-gated CI (e2e / integration / trivy), the exact tax
# this guard exists to avoid. Cite the durable layer instead: a design/operations
# doc, or a stable Q-ID / appendix §-ref (those survive archival untouched).
#
# Stable IDs and §-refs in prose ("Q88", "§H.10") are fine and encouraged — only
# `docs/plan/` and `../plan/` *paths* are rejected. See
# docs/development/maintaining-backlog.md#archiving-completed-plan-docs.
#
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root" || exit 2

# Tracked Go sources, excluding vendored trees.
mapfile -t go_files < <(git ls-files -- '*.go' ':!:**/vendor/**' ':!:vendor/**')

hits=""
if (( ${#go_files[@]} > 0 )); then
    hits="$(grep -nE 'docs/plan/|\.\./plan/' -- "${go_files[@]}" || true)"
fi

if [[ -n "$hits" ]]; then
    {
        printf 'check-no-plan-refs-in-code: Go code references plan docs by path:\n'
        printf '%s\n' "$hits"
        printf '\nPlans get archived; a docs/plan/ path in code rots on archival and forces a\n'
        printf 'code edit (re-triggering heavy CI) during a docs-only move. Cite a durable doc\n'
        printf '(design/operations) or a stable Q-ID / appendix §-ref instead. See\n'
        printf 'docs/development/maintaining-backlog.md#archiving-completed-plan-docs\n'
    } >&2
    exit 1
fi

printf 'check-no-plan-refs-in-code: ok (%d Go files, no docs/plan/ path references)\n' "${#go_files[@]}"
