#!/usr/bin/env bash
#
# md-reflow-coverage.sh — report what fraction of the docset's prose sits at a
# sentence boundary, and where the rest is.
#
# The measuring is done by devtools/docs/mdreflowcoverage, a Go program over the
# same Markdown parser mdreflow reflows against; this script is the entry point
# that selects the files, so the gate map stays in scripts/. The denominator
# rule, and why a line classifier cannot stand in for a parser, are documented
# in that program's package comment.
#
# Not a gate. It writes nothing and exits 0 whatever it finds: a paragraph
# mdreflow declines is a correct outcome. `make md-reflow-check` is the gate,
# and `make md-reflow-explain` names each declined paragraph and its reason.
#
# The file set mirrors .mdreflow.yaml: mdreflow always skips vendor/, and the
# config excludes two generated pages, the backlog, and the AGENTS.md symlink.
# Every tracked symlink is dropped by class below, so a testdata copy of an
# excluded page cannot re-enter the set under a second path.
# Keep the two in step — a file measured here but not reflowed reads as
# permanent residue.
#
# Usage:
#   md-reflow-coverage.sh [-v]      # -v lists every residue line

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

# Mirrors .mdreflow.yaml's `exclude:` list. Anything vendored is dropped by the
# path test below, matching mdreflow's own unconditional vendor/ skip.
excluded() {
    local path="$1"
    case "$path" in
        docs/reference/api.md | docs/development/broker-compatibility.md) return 0 ;;
        docs/STATUS.md | AGENTS.md) return 0 ;;
        vendor/* | */vendor/*) return 0 ;;
    esac
    return 1
}

# Skip symlinks, as check-em-dash.sh and check-doc-links.sh do, so the target is
# measured once. mdreflow refuses a non-regular file outright, so a symlink's
# interior breaks book as permanent residue and inflate the denominator.
files=()
while IFS= read -r path; do
    [[ -L "$repo_root/$path" ]] && continue
    excluded "$path" || files+=("$path")
done < <(cd "$repo_root" && git ls-files '*.md')

if (( ${#files[@]} == 0 )); then
    echo "md-reflow-coverage: no in-scope Markdown files" >&2
    exit 2
fi

# Built and exec'd rather than `go run`, matching check-roadmap.sh: `go run`
# prints its own status line on top of the report. devtools/ is outside the Go
# workspace, hence GOWORK=off — see docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/mdreflowcoverage"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/mdreflowcoverage)

(cd "$repo_root" && "$bin" "$@" "${files[@]}")
