#!/usr/bin/env bash
#
# check-em-dash.sh — enforce the em-dash density rule (Q654).
#
# docs/development/documentation-standards.md rations the em-dash and names a
# threshold ("above roughly 3 per 1,000 words, rewrite"), but nothing enforced
# it, so it drifted the way the prose-only rule before it did.
#
# The counting is done by devtools/docs/emdash over the parsed document
# (Q612's devtools/docs/markdown); this script is the entry point that selects
# the files, so the gate map stays in scripts/. What it excludes from the count
# — code, headings, link text, raw HTML — and why each is legitimate is in that
# program's package comment.
#
# It walks the same file set as check-doc-links.sh: every present, non-vendored
# Markdown file, tracked or untracked-and-not-gitignored, minus symlinks.
#
# On top of the ceilings it runs a diff ratchet (Q742): a whole-file ceiling is
# slack wherever a file sits under it, and two PRs can each spend that same slack
# on their own base and merge to a total above it, which per-PR CI never sees.
# So the files that changed since the base revision are also measured there, and
# a file already over the rule may only lose em-dashes. The base is the
# `origin/main` merge-base, which under the merge queue resolves to the tip the
# candidate commit was built on — the one view holding every queued change at
# once. No base resolves in a shallow clone, and there the gate degrades to the
# ceilings alone rather than blocking every PR.
#
# Usage:
#   scripts/docs/check-em-dash.sh [--write] [--report]
# Options (for the test suite; defaults to the real file):
#   --baseline PATH   the per-file ceilings to check against
#   --base REV        the revision to ratchet against, in place of the merge-base
#
# Exits non-zero when any file gains em-dashes above its baseline ceiling, when a
# changed file over the rule gains any, or when a file with no ceiling is over
# the rule. Findings print as `file: message` (GitHub `::error::` annotations
# under CI).

set -euo pipefail
shopt -s inherit_errexit

# The baseline and the checker are resolved from this script's own location,
# not from the git root below: the root is whatever tree the gate is pointed
# at, which the test suite scopes to a throwaway repo.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

BASELINE="$SCRIPT_DIR/em-dash-baseline.txt"
MODE="check"
BASE_REV=""

while (($# > 0)); do
    case "$1" in
    --write)
        MODE="write"
        ;;
    --report)
        MODE="report"
        ;;
    --baseline)
        BASELINE="$2"
        shift
        ;;
    --base)
        BASE_REV="$2"
        shift
        ;;
    *)
        printf 'check-em-dash.sh: unknown argument: %s\n' "$1" >&2
        exit 2
        ;;
    esac
    shift
done

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

# Command substitution, not `mapfile < <(...)`: it keeps the selection under
# `set -o pipefail`, so a failing `git ls-files` aborts the gate instead of
# quietly reducing it to "no markdown files to check".
md_files=()
selected="$(git_candidates '*.md' ':!:**/vendor/**' ':!:vendor/**' |
    select_present_files | LC_ALL=C sort)"
if [[ -n "$selected" ]]; then
    mapfile -t md_files <<<"$selected"
fi

# Skip symlinks (e.g. AGENTS.md -> CLAUDE.md) so the target is counted once.
scan_files=()
for f in "${md_files[@]}"; do
    [[ -L "$f" ]] && continue
    scan_files+=("$f")
done

if ((${#scan_files[@]} == 0)); then
    echo "check-em-dash: no markdown files to check" >&2
    exit 0
fi

# resolve_base — print the revision the diff ratchet measures against, or
# nothing when there is none. `--base` wins; otherwise the origin/main
# merge-base, which is the branch point on a PR and the tip the candidate was
# built on under the merge queue.
#
# A base equal to HEAD is still a base, not a reason to skip: the comparison
# below runs against the worktree, so on `main` — or on a branch that has not
# committed yet — it is what holds an uncommitted edit to the ratchet. A clean
# tree there simply has nothing to extract.
#
# Fails open locally, the way scripts/go/go-lint.sh scopes itself: a clone with
# no merge-base leaves the ceilings enforcing the rule, because a gate that went
# red on every PR whenever its inputs were missing would be turned off.
#
# Under CI it refuses instead. The whole of Q742 is a gate that was silently not
# checking what it claimed to, so a run where the ratchet cannot measure has to
# say so out loud rather than report the ceilings as the whole verdict. The
# workflow fetches refs/heads/main for exactly this reason, and a red here means
# that fetch is gone rather than that a PR did anything.
resolve_base() {
    local base
    if [[ -n "$BASE_REV" ]]; then
        base="$(git rev-parse --verify --quiet "${BASE_REV}^{commit}")" ||
            die "--base $BASE_REV does not resolve to a commit"
    elif ! base="$(git merge-base HEAD origin/main 2>/dev/null)" || [[ -z "$base" ]]; then
        [[ -z "${CI:-}" ]] ||
            die "no origin/main merge-base under CI, so the diff ratchet cannot run - the job needs fetch-depth: 0 and refs/heads/main"
        echo "check-em-dash: no origin/main merge-base - diff ratchet skipped, ceilings only" >&2
        return 0
    fi
    printf '%s\n' "$base"
}

# extract_base REV DIR — write each changed Markdown file as REV had it into
# DIR, under its repo-relative path. A path REV does not carry (added on this
# branch, or a rename's destination) is left absent, and the counter reads that
# as no base and leaves the file to its ceiling.
#
# Command substitution rather than `< <(...)` for the reason the file selection
# above uses it: a failing `git diff` aborts the gate instead of quietly
# reducing the ratchet to "nothing changed".
extract_base() {
    local rev="$1" dir="$2" changed f
    changed="$(git diff --name-only --diff-filter=d "$rev" -- '*.md' ':!:**/vendor/**' ':!:vendor/**')"
    [[ -n "$changed" ]] || return 0
    while IFS= read -r f; do
        [[ -n "$f" ]] || continue
        [[ -f "$f" && ! -L "$f" ]] || continue
        mkdir -p "$dir/$(dirname "$f")"
        git show "$rev:$f" >"$dir/$f" 2>/dev/null || rm -f "$dir/$f"
    done <<<"$changed"
}

# Built and exec'd rather than `go run` for the same reason check-doc-links.sh
# is: the counter's exit status IS the gate's verdict, and `go run` prints its
# own "exit status 1" line on top of the findings. devtools/ is outside the Go
# workspace, hence GOWORK=off — see docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/emdash"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/emdash)

case "$MODE" in
report)
    "$bin" -report "${scan_files[@]}"
    ;;
write)
    "$bin" -baseline "$BASELINE" -write-baseline "${scan_files[@]}"
    ;;
*)
    # An absent baseline holds every file to the rule itself, which is stricter
    # than the ratchet — a deleted baseline fails loudly rather than silently
    # disabling the gate.
    baseline_args=()
    if [[ -f "$BASELINE" ]]; then
        baseline_args=(-baseline "$BASELINE")
    fi
    base_args=()
    base="$(resolve_base)"
    if [[ -n "$base" ]]; then
        base_dir="$(mktemp -d)"
        trap 'rm -rf "$base_dir"' EXIT
        extract_base "$base" "$base_dir"
        base_args=(-base-dir "$base_dir")
    fi
    "$bin" "${baseline_args[@]}" "${base_args[@]}" "${scan_files[@]}"
    ;;
esac
