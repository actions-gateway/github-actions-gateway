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
# The marketing badges (rules 9-11) are checked on every positional page. The
# two beyond the trio carry no roadmap bullets and no capability index, so they
# are passed for their badges alone.
#
# The current release is resolved by resolve_release_tag (scripts/lib/common.sh),
# shared with check-release-pins.sh: without one, rule 9 cannot say what a
# `new in X.Y` chip is behind, and the checker reports that it skipped.
#
# Usage:
#   check-roadmap.sh [path/to/roadmap.md] [path/to/queue/] [path/to/features.md]
#   GAG_RELEASE_TAG=v9.9.9 check-roadmap.sh
#
# Exits 1 on any finding, and 2 when either page's format drifted far enough
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

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
ROADMAP="${1:-$repo_root/docs/roadmap.md}"
# The backlog is a directory of item files since Q889, not one table.
STORE="${2:-$repo_root/docs/queue}"
FEATURES="${3:-$repo_root/docs/features.md}"

# The remaining marketing surfaces, checked for badges only. They are resolved
# beside the roadmap rather than under the git root, so a test suite pointing
# the gate at a throwaway tree gets that tree's pages (usually none) and not the
# real ones.
badge_pages=()
for page in "$(dirname "$ROADMAP")/index.md" "$(dirname "$ROADMAP")/why-gag.md"; do
    [[ -f "$page" ]] && badge_pages+=("$page")
done

release_args=()
IFS=$'\t' read -r release_tag _ < <(resolve_release_tag "$repo_root") || true
[[ -n "${release_tag:-}" ]] && release_args=(-release "$release_tag")

# Built and exec'd rather than `go run`: the checker's exit status IS the gate's
# verdict, and `go run` prints its own "exit status 1" line on top of the
# findings. devtools/ is outside the Go workspace, hence GOWORK=off — see
# docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/roadmapcheck"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./docs/roadmapcheck)

"$bin" "${release_args[@]}" "$ROADMAP" "$STORE" "$FEATURES" "${badge_pages[@]+"${badge_pages[@]}"}"
