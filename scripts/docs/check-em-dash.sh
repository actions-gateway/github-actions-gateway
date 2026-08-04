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
# Usage:
#   scripts/docs/check-em-dash.sh [--write] [--report]
# Options (for the test suite; defaults to the real file):
#   --baseline PATH   the per-file ceilings to check against
#
# Exits non-zero when any file gains em-dashes above its baseline ceiling, or
# when a file with no ceiling is over the rule. Findings print as
# `file: message` (GitHub `::error::` annotations under CI).

set -euo pipefail

# The baseline and the checker are resolved from this script's own location,
# not from the git root below: the root is whatever tree the gate is pointed
# at, which the test suite scopes to a throwaway repo.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

BASELINE="$SCRIPT_DIR/em-dash-baseline.txt"
MODE="check"

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
    "$bin" "${baseline_args[@]}" "${scan_files[@]}"
    ;;
esac
