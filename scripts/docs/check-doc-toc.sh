#!/usr/bin/env bash
#
# check-doc-toc.sh — hold every hand-kept Table of Contents in docs/ to the
# headings it indexes (Q865, widened to the tree by Q911).
#
# doc-links resolves every `#anchor` that is written, which leaves it blind to
# a heading the index never mentions: there is no link to fail. This gate asks
# the other direction — that every heading the page indexes has an entry, that
# no entry names a heading the page does not have, and that the entries follow
# the document's own order and nesting.
#
# The checking is done by devtools/docs/doctoc, a Go program over the same
# Markdown parser and slugger doc-links uses, so the two gates cannot disagree
# about what an anchor points at; this script is the entry point that selects
# the pages, so the gate map stays in scripts/. What it fails on, how deep each
# page is held, and what it deliberately ignores, is documented in that
# program's package comment.
#
# Selection is by content, not by a registry: every Markdown file under docs/
# that carries a `## Table of Contents` heading. A page gaining one is checked
# from its first run, and a page losing one drops out rather than refusing —
# the index this gate exists to hold is gone, so there is nothing to hold.
#
# Usage:
#   check-doc-toc.sh [path/to/page.md ...]
#
# Exits 1 on any finding, and 2 when a named page's shape drifted far enough
# that the gate would otherwise pass by checking nothing.

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which a test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

# Run from the root so the default subjects, and so every finding, read as the
# repo-relative path the reader opens — the same shape doc-links reports.
repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

pages=()
if (($# > 0)); then
    # A page named on the command line is checked whatever it holds: the caller
    # asserted it has an index, so a missing one is a finding rather than a
    # reason to drop it silently. A page that is not there is a refusal, not a
    # pass — the gate's subject would otherwise vanish with a rename and take
    # the verdict green with it.
    for page in "$@"; do
        if [[ ! -f "$page" ]]; then
            printf 'check-doc-toc: %s does not exist, so this gate would check nothing\n' "$page" >&2
            exit 2
        fi
        pages+=("$page")
    done
else
    # Command substitution, not `mapfile < <(...)`: it keeps the selection under
    # `set -o pipefail`, so a failing `git ls-files` aborts the gate instead of
    # quietly reducing it to "no pages to check".
    selected="$(git_candidates 'docs/**/*.md' 'docs/*.md' |
        select_present_files | LC_ALL=C sort)"
    if [[ -n "$selected" ]]; then
        while IFS= read -r page; do
            [[ -L "$page" ]] && continue
            grep -qx '## Table of Contents' "$page" || continue
            pages+=("$page")
        done <<<"$selected"
    fi
fi

# An empty set is a refusal. The selection is a content grep over a tree that
# holds 20 such pages, so zero means the query stopped matching rather than
# that the indexes went away, and reporting ok would be reporting on nothing.
if ((${#pages[@]} == 0)); then
    printf 'check-doc-toc: no page under docs/ carries a %s heading, so this gate would check nothing\n' '## Table of Contents' >&2
    exit 2
fi

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/doctoc"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/doctoc)

"$bin" "${pages[@]}"
