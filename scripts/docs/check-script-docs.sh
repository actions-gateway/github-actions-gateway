#!/usr/bin/env bash
#
# check-script-docs.sh — fail when a script under scripts/ has no entry in
# scripts/README.md (Q688).
#
# That page is the only map from a script to the gate that runs it, and nothing
# held it to the tree: sixteen *-test.sh files and check-page-density.sh had
# drifted off it by the time this gate was written. Listing them fixes the day;
# the gate is what stops the set drifting again.
#
# The checking is done by devtools/docs/scriptdocs, a Go program over a real
# Markdown parser; this script is the entry point that selects the files, so the
# gate map stays in scripts/. What counts as a mention, and why a fenced example
# is not one, are in that program's package comment.
#
# File selection is the same query the shellcheck gate uses — tracked files PLUS
# untracked-and-not-gitignored ones — so a brand-new script is checked by its own
# first `make check` rather than from the commit that tracks it (Q432/Q619).
#
# Usage:
#   check-script-docs.sh [--readme PATH] [script.sh ...]
#
# With no scripts named, every present scripts/**/*.sh is checked. Exits 1 on
# any finding, and 2 when the README's format drifted far enough that the gate
# would otherwise pass by checking nothing.

set -euo pipefail

# The library is resolved from this script's own location rather than from the
# git root, which a test suite scopes to a throwaway tree with no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
# The checker resolves each script against the README's own directory, so the
# two must be given in the same form. Both default to repo-relative, under the
# `cd "$REPO_ROOT"` below; a caller overriding one overrides both.
README="scripts/README.md"

while (($# > 0)); do
    case "$1" in
    --readme)
        README="$2"
        shift
        ;;
    --)
        shift
        break
        ;;
    -*)
        printf 'check-script-docs.sh: unknown argument: %s\n' "$1" >&2
        exit 2
        ;;
    *)
        break
        ;;
    esac
    shift
done

cd "$REPO_ROOT"

scripts=()
if (($# > 0)); then
    scripts=("$@")
else
    # Command substitution rather than `mapfile < <(...)` so the selection stays
    # under `set -o pipefail`: a failing `git ls-files` aborts the gate instead
    # of quietly reducing it to "no scripts to check".
    selected="$(git_candidates 'scripts/*.sh' | select_present_files | LC_ALL=C sort)"
    if [[ -n "$selected" ]]; then
        mapfile -t scripts <<<"$selected"
    fi
fi

if ((${#scripts[@]} == 0)); then
    printf 'check-script-docs: no scripts to check\n' >&2
    exit 2
fi

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/scriptdocs"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/scriptdocs)

"$bin" "$README" "${scripts[@]}"
