#!/usr/bin/env bash
#
# check-upgrade-toc.sh — hold upgrade.md's hand-kept Table of Contents to the
# headings it indexes (Q865).
#
# doc-links resolves every `#anchor` that is written, which leaves it blind to
# a heading the index never mentions: there is no link to fail. This gate asks
# the other direction — that every level-2 and level-3 heading has an entry,
# that no entry names a heading the page does not have, and that the entries
# follow the document's own order and nesting.
#
# The checking is done by devtools/docs/upgradetoc, a Go program over the same
# Markdown parser and slugger doc-links uses, so the two gates cannot disagree
# about what an anchor points at; this script is the entry point that resolves
# the file, so the gate map stays in scripts/. What it fails on, and what it
# deliberately ignores, is documented in that program's package comment.
#
# Scoped to this one page on purpose. Every other doc in the tree either has no
# hand-kept index or is short enough to read whole; a repo-wide version is a
# larger change and would want its own backlog item.
#
# Usage:
#   check-upgrade-toc.sh [path/to/upgrade.md]
#
# Exits 1 on any finding, and 2 when the page's shape drifted far enough that
# the gate would otherwise pass by checking nothing.

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which a test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

# Run from the root so the default subject, and so every finding, reads as the
# repo-relative path the reader opens — the same shape doc-links reports.
repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"
PAGE="${1:-docs/operations/upgrade.md}"

# A page that is not there is a refusal, not a pass: the gate's whole subject
# would otherwise vanish with a rename and take the verdict green with it.
if [[ ! -f "$PAGE" ]]; then
    printf 'check-upgrade-toc: %s does not exist, so this gate would check nothing\n' "$PAGE" >&2
    exit 2
fi

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/upgradetoc"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/upgradetoc)

"$bin" "$PAGE"
