#!/usr/bin/env bash
#
# check-card-bullets.sh — hold every bullet inside a `.gag-pillars` card to the
# width of the column it renders in (Q711).
#
# The card grid is a scanning surface: one wrapped bullet reads as prose among
# labels. website.md states the invariant and the two column budgets it was
# measured at, and nothing checked it — measured 2026-09-10 against
# `make docs-serve` at 1440px, two bullets on docs/why-gag.md wrapped, on a
# page recorded as compliant a month earlier.
#
# The checking is done by devtools/docs/cardbullets, a Go program over the same
# Markdown parser the other docs gates use, so what it measures is the rendered
# text a reader sees rather than the source. Its package comment documents the
# budgets, why they are a proxy, and what this gate therefore cannot catch.
#
# Usage:
#   check-card-bullets.sh [page.md...]
#
# Exits 1 on any finding, and 2 when no card bullet was found at all, since the
# gate would otherwise pass by checking nothing.

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which a test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

# Run from the root so every finding reads as the repo-relative path the reader
# opens.
repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

# Only these two pages carry the grid, per website.md. Naming them rather than
# scanning the tree keeps the gate's subject explicit: a third page adopting the
# cards is a deliberate edit here, not a silent enrolment.
if (($# > 0)); then
    pages=("$@")
else
    pages=(docs/index.md docs/why-gag.md)
fi

# A page that is not there is a refusal, not a pass: the gate's subject would
# otherwise vanish with a rename and take the verdict green with it.
for page in "${pages[@]}"; do
    if [[ ! -f "$page" ]]; then
        printf 'check-card-bullets: %s does not exist, so this gate would check nothing\n' "$page" >&2
        exit 2
    fi
done

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/cardbullets"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/cardbullets)

"$bin" "${pages[@]}"
