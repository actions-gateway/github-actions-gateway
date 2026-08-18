#!/usr/bin/env bash
# reconcile-queue-rows.sh — name every backlog item a merge or rebase resolution
# removed, and say which removals the two sides account for (Q858).
#
# Usage: scripts/docs/reconcile-queue-rows.sh [--base REF] [--onto REF]
# Or:    make queue-reconcile
#
# Run it while a conflicted rebase or merge is still in progress — before
# `git rebase --continue` or `git commit` — or immediately after one finishes.
#
# Deleting an item is how an item closes, so no lint pass can tell a completed
# item from a casualty of a hand-resolved hunk, and `check-queue-rules.py` rule
# 8 only refuses the deletion of a `flake` item: every other dropped row passes
# clean. What separates the two here is *who* deleted it. An item that leaves
# your row set is accounted for when the side you are merging deleted it, and an
# item on their side that your resolution does not carry is accounted for when
# you deleted it. Anything else is collateral from the resolution, which is what
# this prints and exits 1 for.
#
# Three states, because the row set being resolved does not live in the same
# place in each. Measured on git 2.55.0:
#
#   rebase in progress  base .git/rebase-*/orig-head  ours index  theirs onto
#   merge in progress   base HEAD                     ours index  theirs MERGE_HEAD
#   neither             base ORIG_HEAD                ours HEAD   theirs origin/main
#
# Mid-operation the resolution is in the **index**, not in `HEAD`: `HEAD` is the
# replay so far and excludes the commit being resolved, so reading it reports
# every row the branch adds as a casualty and buries the real one. Only the
# third state needs `ORIG_HEAD`, which the next rebase or merge overwrites.
#
# Exits 0 when every difference is accounted for, 1 on collateral, and 2 on a
# read it could not take — never a verdict.
set -euo pipefail
shopt -s inherit_errexit

STORE="docs/queue"

BASE_REF=""
ONTO_REF=""
while (($# > 0)); do
    case "$1" in
    --base)
        [[ $# -ge 2 ]] || { echo "reconcile-queue-rows.sh: --base needs a ref" >&2; exit 2; }
        BASE_REF="$2"
        shift 2
        ;;
    --onto)
        [[ $# -ge 2 ]] || { echo "reconcile-queue-rows.sh: --onto needs a ref" >&2; exit 2; }
        ONTO_REF="$2"
        shift 2
        ;;
    -h | --help)
        # The header comment is the usage text, so print it to its end rather
        # than to a line number that drifts as it grows.
        awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' \
            "${BASH_SOURCE[0]}"
        exit 0
        ;;
    *)
        echo "reconcile-queue-rows.sh: unknown argument: $1" >&2
        exit 2
        ;;
    esac
done

# The repository comes from the caller's working directory, not from this
# script's location: it reports on the tree that is mid-resolution, which in a
# worktree is a different root, and a test suite scopes it to a throwaway one.
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
[[ -n "$REPO_ROOT" ]] || {
    echo "reconcile-queue-rows: could not read a git repository; refusing to guess" >&2
    exit 2
}
cd "$REPO_ROOT"

WORK="$(mktemp -d)"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# refuse REASON — a read that did not happen is never a verdict.
refuse() {
    echo "reconcile-queue-rows: could not read $*; refusing to guess" >&2
    exit 2
}

# store_ids — read paths on stdin, emit the item IDs among them, sorted.
# Anything that is not a `Q<n>.md` directly in the store is not an item, which
# drops README.md and any nested path.
store_ids() {
    awk -v store="$STORE" '
        {
            n = split($0, part, "/")
            base = part[n]
            dir = substr($0, 1, length($0) - length(base) - 1)
            if (dir != store) next
            if (base ~ /^Q[0-9]+\.md$/) { sub(/\.md$/, "", base); print base }
        }
    ' | sort -u
}

# ids_at REF FILE — write the store's item IDs at REF into FILE.
ids_at() {
    local ref="$1" out="$2"
    git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null || refuse "the commit $ref"
    git ls-tree -r --name-only "$ref" -- "$STORE" | store_ids > "$out"
}

# ids_in_index FILE — write the store's item IDs in the index into FILE.
ids_in_index() {
    git ls-files -- "$STORE" | store_ids > "$1"
}

rebase_dir=""
for candidate in rebase-merge rebase-apply; do
    dir="$(git rev-parse --git-path "$candidate")"
    if [[ -d "$dir" ]]; then
        rebase_dir="$dir"
        break
    fi
done

if [[ -n "$rebase_dir" ]]; then
    state="rebase"
    [[ -r "$rebase_dir/orig-head" ]] || refuse "$rebase_dir/orig-head"
    base="$(<"$rebase_dir/orig-head")"
    [[ -r "$rebase_dir/onto" ]] || refuse "$rebase_dir/onto"
    onto="$(<"$rebase_dir/onto")"
elif [[ -r "$(git rev-parse --git-path MERGE_HEAD)" ]]; then
    state="merge"
    base="HEAD"
    onto="MERGE_HEAD"
else
    state="settled"
    base="${BASE_REF:-ORIG_HEAD}"
    onto="${ONTO_REF:-origin/main}"
    if [[ -z "$BASE_REF" ]] && ! git rev-parse --verify --quiet ORIG_HEAD >/dev/null; then
        echo "reconcile-queue-rows: no rebase or merge in progress and ORIG_HEAD is unset," >&2
        echo "  so there is no record of the row set before the resolution. The next rebase" >&2
        echo "  or merge overwrites it, so run this promptly, or name the pre-resolution tip" >&2
        echo "  with --base REF." >&2
        exit 2
    fi
fi

# The caller's overrides win in every state, so a resolution reconciled after
# the fact can still name the two sides it actually had.
[[ -n "$BASE_REF" ]] && base="$BASE_REF"
[[ -n "$ONTO_REF" ]] && onto="$ONTO_REF"

if [[ "$state" != "settled" ]]; then
    # An unmerged path under the store means the row set is not settled yet, so
    # any answer here would be about a half-resolved tree. --diff-filter=U emits
    # bare paths; `ls-files -u` emits "mode oid stage<TAB>path", which store_ids
    # matches none of.
    unmerged="$(git diff --name-only --diff-filter=U -- "$STORE" | store_ids)"
    if [[ -n "$unmerged" ]]; then
        echo "reconcile-queue-rows: these items are still unmerged in the index:" >&2
        while read -r stuck; do printf '  %s\n' "$stuck" >&2; done <<< "$unmerged"
        echo "  Resolve them first — the row set is not settled until you do." >&2
        exit 2
    fi
fi

# Both sides are resolved by now, from state or from the caller. Check they read
# before anything reasons about them, so an unusable ref is reported as itself
# rather than as whichever later check happens to trip over it first.
git rev-parse --verify --quiet "${base}^{commit}" >/dev/null || refuse "the commit $base"
git rev-parse --verify --quiet "${onto}^{commit}" >/dev/null || refuse "the commit $onto"

if [[ "$state" == "settled" ]] && ! git merge-base --is-ancestor "$onto" HEAD 2>/dev/null; then
    echo "reconcile-queue-rows: HEAD does not contain ${onto}, so no resolution of the" >&2
    echo "  two has happened yet and there is nothing to reconcile. Every item ${onto}" >&2
    echo "  filed since the fork would read as lost. Rebase or merge first." >&2
    exit 2
fi

ids_at "$base" "$WORK/base"
ids_at "$onto" "$WORK/theirs"
if [[ "$state" == "settled" ]]; then
    ids_at HEAD "$WORK/ours"
else
    ids_in_index "$WORK/ours"
fi

merge_base="$(git merge-base "$base" "$onto" 2>/dev/null)" ||
    refuse "the merge base of $base and $onto"
ids_at "$merge_base" "$WORK/mb"

# Abbreviate for the report only. Recovery names the same ref, and a short OID
# is as good an argument to `git checkout` as the full one.
base_label="$(git rev-parse --short "$base")"
onto_label="$onto"
[[ "$onto" =~ ^[0-9a-f]{40}$ ]] && onto_label="$(git rev-parse --short "$onto")"

comm -23 "$WORK/base" "$WORK/ours" > "$WORK/lost"
comm -13 "$WORK/ours" "$WORK/theirs" > "$WORK/absent"

# holds FILE ID — is ID in this sorted ID list?
holds() { grep -qxF "$2" "$1"; }

collateral=0

echo "==> reconcile-queue-rows (${state}): base ${base_label}, theirs ${onto_label}"
printf '    %s item(s) yours, %s theirs, %s at the merge base\n' \
    "$(wc -l < "$WORK/ours" | tr -d ' ')" \
    "$(wc -l < "$WORK/theirs" | tr -d ' ')" \
    "$(wc -l < "$WORK/mb" | tr -d ' ')"
echo

# Each casualty names the side that still has it, because the two lists recover
# from opposite refs: a row you had is on the base, a row they added is on onto.
if [[ -s "$WORK/lost" ]]; then
    echo "Items you had and no longer have:"
    while read -r id; do
        if holds "$WORK/mb" "$id" && ! holds "$WORK/theirs" "$id"; then
            printf '  %-8s closed on %s\n' "$id" "$onto_label"
        else
            printf '  %-8s COLLATERAL — neither side deleted it\n' "$id"
            printf '           git checkout %s -- %s/%s.md\n' "$base_label" "$STORE" "$id"
            collateral=$((collateral + 1))
        fi
    done < "$WORK/lost"
    echo
fi

if [[ -s "$WORK/absent" ]]; then
    echo "Items ${onto_label} has and you do not:"
    while read -r id; do
        if holds "$WORK/mb" "$id" && ! holds "$WORK/base" "$id"; then
            printf '  %-8s closed on your branch\n' "$id"
        else
            printf '  %-8s COLLATERAL — added on %s, dropped by the resolution\n' "$id" "$onto_label"
            printf '           git checkout %s -- %s/%s.md\n' "$onto_label" "$STORE" "$id"
            collateral=$((collateral + 1))
        fi
    done < "$WORK/absent"
    echo
fi

if ((collateral > 0)); then
    echo "reconcile-queue-rows: FAILED — ${collateral} item(s) lost to the resolution."
    echo "Each line above names the ref that still has it."
    exit 1
fi

if [[ ! -s "$WORK/lost" && ! -s "$WORK/absent" ]]; then
    echo "reconcile-queue-rows: ok — the row sets agree; no item left either side."
else
    echo "reconcile-queue-rows: ok — every difference is one side closing an item."
fi
