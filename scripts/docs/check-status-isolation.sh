#!/usr/bin/env bash
#
# check-status-isolation.sh — a commit that touches docs/STATUS.md touches
# nothing else.
#
# Isolation is what keeps the backlog's rebase conflicts trivial: the file is
# edited by almost every session, so a mixed commit drags code or a plan doc
# into every conflict resolution on it (maintaining-backlog.md § The shared
# process, in brief).
#
# Why this exists alongside the pre-commit hook. `lint-backlog.sh --staged`
# already refuses to *stage* docs/STATUS.md next to another file, but it reads
# the index, not the commit the index is about to produce — so
# `git commit --amend` onto a code commit, with only docs/STATUS.md staged,
# passes it and still writes a mixed commit (measured; that is the case Q652
# names). It is also opt-in per clone (`make hooks`) and `--no-verify` waives
# it. This gate reads the commits themselves, so neither hole is reachable.
#
# Scope: the commits the branch adds, `merge-base(BASE, HEAD)..HEAD`, never
# anything already on BASE. Two consequences worth stating:
#
#   * Every commit it can fail is one the branch owns, so the fix — reorder or
#     split with `git rebase -i` — is always available to whoever is being
#     failed. A mixed commit predating this gate is not exempt: it is still the
#     branch's commit, and BACKLOG_ALLOW_MIXED_COMMITS is the deliberate,
#     reviewable way through when rewriting history costs more than it buys.
#   * It must never run over main's own history. GitHub squash-merges, so a PR
#     that correctly kept its backlog edit in its own commit lands on main as a
#     single commit touching STATUS.md and everything else — mixed by
#     construction. The rule's whole value is during a PR's life, which is
#     exactly the window this scans.
#
# Merge commits are skipped: their diff depends on which parent you pick, so
# "what this commit touched" has no single answer for them.
#
# Usage:
#   check-status-isolation.sh [--base REF] [--head REF] [--file PATH]
#
# Defaults: --base origin/main, --head HEAD, --file docs/STATUS.md. Exits 0
# with a note when BASE does not resolve (a fresh clone with no origin), the
# same no-baseline behaviour lint-backlog.sh's git rules take.
#
# Backs `make status-isolation-check`; runs in `make check`, `make
# status-gates`, and status-lint.yml.
set -euo pipefail

BASE_REF="origin/main"
HEAD_REF="HEAD"
BACKLOG_FILE="docs/STATUS.md"

while (($# > 0)); do
    case "$1" in
    --base)
        BASE_REF="$2"
        shift 2
        ;;
    --head)
        HEAD_REF="$2"
        shift 2
        ;;
    --file)
        BACKLOG_FILE="$2"
        shift 2
        ;;
    *)
        printf 'check-status-isolation: unknown argument: %s\n' "$1" >&2
        exit 2
        ;;
    esac
done

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

if ! git rev-parse --verify --quiet "$BASE_REF^{commit}" >/dev/null; then
    printf 'check-status-isolation: %s does not resolve; skipping\n' "$BASE_REF" >&2
    exit 0
fi

if ! merge_base="$(git merge-base "$BASE_REF" "$HEAD_REF" 2>/dev/null)"; then
    printf 'check-status-isolation: no merge base for %s..%s; skipping\n' \
        "$BASE_REF" "$HEAD_REF" >&2
    exit 0
fi

# Space-separated SHAs (any unambiguous length) this run must admit anyway.
allowed=" ${BACKLOG_ALLOW_MIXED_COMMITS:-} "

violations=0
while IFS= read -r sha; do
    # --root so a branch whose range reaches the initial commit is still read as
    # a diff against the empty tree rather than erroring out.
    files="$(git diff-tree --no-commit-id --name-only -r --root "$sha")"
    grep -qx -- "$BACKLOG_FILE" <<<"$files" || continue
    others="$(grep -vx -- "$BACKLOG_FILE" <<<"$files" || true)"
    [[ -n "$others" ]] || continue

    short="$(git rev-parse --short "$sha")"
    if [[ "$allowed" == *" $short"* || "$allowed" == *" $sha "* ]]; then
        printf 'check-status-isolation: %s allowed by BACKLOG_ALLOW_MIXED_COMMITS\n' \
            "$short" >&2
        continue
    fi

    {
        printf '%s %s\n' "$short" "$(git log -1 --format=%s "$sha")"
        printf '  touches %s alongside:\n' "$BACKLOG_FILE"
        awk '{ print "    " $0 }' <<<"$others"
    } >&2
    violations=$((violations + 1))
done < <(git rev-list --no-merges "$merge_base..$HEAD_REF")

if ((violations > 0)); then
    {
        printf '\ncheck-status-isolation: %d commit(s) mix %s with other files.\n' \
            "$violations" "$BACKLOG_FILE"
        printf 'Backlog edits are isolated commits so rebase conflicts stay on one\n'
        printf 'file: docs/development/maintaining-backlog.md#isolated-commits-and-what-actually-enforces-them\n'
        printf 'Split them with: git rebase -i %s\n' "$merge_base"
        printf 'Or set BACKLOG_ALLOW_MIXED_COMMITS="<sha> ..." to admit specific commits.\n'
    } >&2
    exit 1
fi

exit 0
