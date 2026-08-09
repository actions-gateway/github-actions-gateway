#!/usr/bin/env bash
#
# lint-backlog.sh — format checks for a repo-local backlog file (docs/STATUS.md).
#
# The checking is done by devtools/docs/backloglint, a Go program over a real
# Markdown parser (Q613); this script is the entry point that picks the file and
# maps the environment interface onto its flags, so the gate map stays in
# scripts/. The rule list, and why rows are read from the GFM table AST rather
# than split on a literal `|`, are in that program's package comment.
#
# Usage:
#   lint-backlog.sh [--staged] [path/to/STATUS.md]
#
# Defaults to docs/STATUS.md under the repo root. With --staged (pre-commit
# mode): exits 0 untouched when the backlog file is not staged; when it is
# staged, requires it to be the *only* staged file (backlog edits are isolated
# commits so rebase conflicts resolve on one file) and then runs the content
# rules. Bypass a single commit with `git commit --no-verify`.
#
# Environment:
#   NOTES_MAX_CHARS               hard cap on a Notes/trigger cell (default 250)
#   NOTES_LINK_CHARS              length above which the cell must link a doc (200)
#   BACKLOG_ALLOW_FLAKE_DELETE    IDs whose flake row may be retired (rule 8)
#   BACKLOG_ALLOW_PROGRESS_STALE  plan paths whose Progress row may stay ⚠️ (rule 9)
#   BACKLOG_ALLOW_RESURRECT       IDs that may be deliberately re-opened (rule 10)
#   BACKLOG_ALLOW_UNCLAIMED_ID    IDs claimed from another clone/session (rule 12)

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which a test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

STAGED=0
if [[ "${1:-}" == "--staged" ]]; then
    STAGED=1
    shift
fi

if [[ -n "${1:-}" ]]; then
    FILE="$1"
else
    repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
    FILE="$repo_root/docs/STATUS.md"
fi

if [[ ! -f "$FILE" ]]; then
    printf 'lint-backlog: file not found: %s\n' "$FILE" >&2
    exit 2
fi

# Fast path for the pre-commit hook, which runs on every commit: a commit that
# does not stage the backlog owes nothing, and answering that with one git call
# keeps the hook off the build below. The checker repeats the test — this is an
# optimization, not the rule.
args=()
if (( STAGED )); then
    rel="$(git -C "$(dirname "$FILE")" rev-parse --show-prefix)$(basename "$FILE")"
    staged_files="$(git diff --cached --name-only --diff-filter=ACMRD)"
    if ! grep -qxF -- "$rel" <<<"$staged_files"; then
        exit 0
    fi
    args+=(--staged)
fi

# add_flag FLAG VALUE — append `FLAG VALUE` when VALUE is non-empty, so an
# unset environment override leaves the checker's own default in place.
add_flag() {
    [[ -n "$2" ]] && args+=("$1" "$2")
    return 0
}
add_flag --max-chars "${NOTES_MAX_CHARS:-}"
add_flag --link-chars "${NOTES_LINK_CHARS:-}"
add_flag --allow-flake-delete "${BACKLOG_ALLOW_FLAKE_DELETE:-}"
add_flag --allow-progress-stale "${BACKLOG_ALLOW_PROGRESS_STALE:-}"
add_flag --allow-resurrect "${BACKLOG_ALLOW_RESURRECT:-}"
add_flag --allow-unclaimed-id "${BACKLOG_ALLOW_UNCLAIMED_ID:-}"

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. The binary lands beside the source it is built from — not under the
# tree being checked, which a test suite points at a throwaway repo.
# devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/backloglint"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/backloglint)

"$bin" ${args+"${args[@]}"} "$FILE"
