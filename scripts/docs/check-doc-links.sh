#!/usr/bin/env bash
#
# check-doc-links.sh — GitHub-slug-aware markdown link & anchor checker (Q52).
#
# The checking is done by devtools/docs/doclinks, a Go program over a real
# Markdown parser (Q612); this script is the entry point that selects the files
# and builds the existence oracle, so the gate map stays in scripts/. What the
# checker fails on, and what it deliberately ignores, is documented in that
# program's package comment.
#
# It walks every present, non-vendored Markdown file — tracked or
# untracked-and-not-gitignored, so a brand-new doc's links are checked by its
# own first `make check` (Q619).
#
# Usage:
#   scripts/docs/check-doc-links.sh
#
# Exits non-zero on the first run that finds any broken link/anchor, printing
# `file:line: message` for each (GitHub `::error::` annotations under CI).

set -euo pipefail

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which a test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

# Files to scan: present Markdown, excluding the vendored third-party trees.
# Command substitution, not `mapfile < <(...)`: it keeps the selection under
# `set -o pipefail`, so a failing `git ls-files` aborts the gate instead of
# quietly reducing it to "no markdown files to check".
md_files=()
selected="$(git_candidates '*.md' ':!:**/vendor/**' ':!:vendor/**' |
    select_present_files | LC_ALL=C sort)"
if [[ -n "$selected" ]]; then
    mapfile -t md_files <<<"$selected"
fi

# Skip symlinks (e.g. AGENTS.md -> CLAUDE.md) so the target is scanned once.
scan_files=()
for f in "${md_files[@]}"; do
    [[ -L "$f" ]] && continue
    scan_files+=("$f")
done

if (( ${#scan_files[@]} == 0 )); then
    echo "check-doc-links: no markdown files to check" >&2
    exit 0
fi

# Existence oracle for relative-link resolution: the same candidate set the scan
# list is drawn from, so a link to a brand-new file added in the same change
# resolves before it is staged. The checker derives ancestor directories from
# these so directory links resolve too.
exist_file="$(mktemp "${TMPDIR:-/tmp}/check-doc-links.XXXXXX")"
trap 'rm -f "$exist_file"' EXIT
git_candidates > "$exist_file"

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. The binary lands beside the source it is built from — not under
# REPO_ROOT, which a test suite points at a throwaway tree.
# devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/doclinks"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/doclinks)

"$bin" -root "$REPO_ROOT" -exist-file "$exist_file" "${scan_files[@]}"
