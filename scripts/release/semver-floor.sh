#!/usr/bin/env bash
#
# semver-floor.sh — report the minimum semver bump the merged work already
# requires, and check that the release surface it reads is still derivable.
#
# "What is the smallest version this can be released as?" had no answer here
# short of hand-classifying every commit since the tag, which nobody does until
# release day — by which point the answer is fixed rather than decided.
#
# Counting `feat` subjects is not that answer. In the v1.3.0..v1.4.0 window 17
# commits are `feat` and 11 of them are dev tooling, CI, and docs that ship in
# no image and no chart. So the classification is by the paths a commit
# touched, checked against the surface publish.yml actually packages — derived
# from that workflow's image matrix, the Dockerfile stages behind it, and
# `go list -deps` over the resulting builds, so there is no scope list to rot.
#
# The classifying is done by devtools/release/semverfloor; this script is the
# entry point, so the gate map stays in scripts/. What it derives and what it
# cannot see is in that program's package comment.
#
# Usage:
#   scripts/release/semver-floor.sh [FROM] [TO]
#   scripts/release/semver-floor.sh --check-sources
#
# FROM defaults to the highest stable (non-RC) `v*` tag; TO defaults to
# `origin/main`, falling back to HEAD.
#
# Reporting mode never fails: the floor is an input to the release decision in
# docs/operations/release.md § When to cut, not a verdict on it. --check-sources
# IS a gate — it exits non-zero when publish.yml has grown a release artifact
# the derivation does not cover, which is the one way this can go quietly wrong.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

args=()
for arg in "$@"; do
    case "$arg" in
    -h | --help)
        awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"
        exit 0
        ;;
    --check-sources)
        args+=("-check-sources")
        ;;
    -*)
        echo "semver-floor: unknown option: $arg" >&2
        exit 2
        ;;
    *)
        args+=("$arg")
        ;;
    esac
done

# Positional FROM/TO become the program's flags, so the program stays usable on
# its own without reproducing this script's argument shape.
if ((${#args[@]} > 0)) && [[ "${args[0]}" != -* ]]; then
    from="${args[0]}"
    to="${args[1]:-}"
    args=("-from" "$from")
    [[ -n "$to" ]] && args+=("-to" "$to")
fi

# Built and exec'd rather than `go run` for the same reason check-em-dash.sh is:
# the checker's exit status IS the gate's verdict, and `go run` prints its own
# "exit status 1" line on top of the findings. devtools/ is outside the Go
# workspace, hence GOWORK=off — see docs/development/go-workspaces.md.
require_cmd go "https://go.dev/dl/"
bin="$SCRIPT_DIR/../../.build/semverfloor"
mkdir -p "$(dirname "$bin")"
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./release/semverfloor)

exec "$bin" "${args[@]}"
