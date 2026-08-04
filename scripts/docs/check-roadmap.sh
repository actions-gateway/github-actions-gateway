#!/usr/bin/env bash
#
# check-roadmap.sh — keep the public roadmap honest against the backlog, and
# keep the feature index from regrowing into prose.
#
# The checking is done by devtools/docs/roadmapcheck, a Go program over a real
# Markdown parser (Q614); this script is the entry point that selects the files,
# so the gate map stays in scripts/. The rules, the `<!-- q:QN -->` annotation
# format, and the word caps are documented in that program's package comment.
#
# Usage:
#   check-roadmap.sh [path/to/roadmap.md] [path/to/STATUS.md] [path/to/features.md]
#
# Exits 1 on any finding, and 2 when either page's format drifted far enough
# that the gate would otherwise pass by checking nothing.

set -euo pipefail

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which a test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
ROADMAP="${1:-$repo_root/docs/roadmap.md}"
STATUS="${2:-$repo_root/docs/STATUS.md}"
FEATURES="${3:-$repo_root/docs/features.md}"

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/roadmapcheck"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/roadmapcheck)

"$bin" "$ROADMAP" "$STATUS" "$FEATURES"
