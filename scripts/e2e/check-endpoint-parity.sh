#!/usr/bin/env bash
#
# check-endpoint-parity.sh — fail when the AGC calls a GitHub REST endpoint the
# e2e fake does not serve (Q871).
#
# Q811 added a run read to the eviction path. test/fakegithub served two paths on
# that prefix and 404'd the rest, so the drained-worker spec failed thirteen
# merge-queue entries while the PR that introduced the call stayed green. Nothing
# compared the venue against the code it exercises, and the fake's endpoint set
# was a list somebody kept by hand — which is only ever as fresh as the last
# person who remembered it.
#
# So neither side is a list. The caller side is folded out of the AGC's own
# source at every http.NewRequest site; the served side is this fake, built and
# run, answering a probe per endpoint. What each of the two checks asserts, why
# the served side is probed rather than read, and what the pin registry can and
# cannot silence are in the package comment beside devtools/e2e/endpointparity.
#
# Usage:
#   check-endpoint-parity.sh [FAKE_SRC SRC_ROOT...]
#
# With no arguments the shipped paths are used. Exits 1 on any finding, 2 when
# the check could not be taken at all.

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location rather than from the
# git root, which a test suite scopes to a throwaway tree with no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

# The modules the AGC binary links that address the GitHub API base. cmd/probe is
# deliberately absent: it talks to real GitHub under credentials, never to the
# fake, so holding the venue to its endpoints would demand routes no deployed
# binary asks for.
FAKE_SRC="test/fakegithub"
SRC_ROOTS=(cmd/agc broker githubapp scaleset)

if (($# == 1)); then
    printf 'check-endpoint-parity.sh: expected 0 arguments, or a fake source dir followed by at least one source root\n' >&2
    exit 2
elif (($# >= 2)); then
    FAKE_SRC="$1"
    shift
    SRC_ROOTS=("$@")
fi

cd "$REPO_ROOT"

for d in "$FAKE_SRC" "${SRC_ROOTS[@]}"; do
    if [[ ! -d "$d" ]]; then
        printf 'check-endpoint-parity: %s is not a directory\n' "$d" >&2
        exit 2
    fi
done

require_cmd go "https://go.dev/dl/"

# Both binaries are built and exec'd rather than `go run`: the checker's exit
# status IS the gate's verdict, and `go run` prints its own "exit status 1" line
# on top of the findings. Both build with GOWORK=off — devtools/ is outside the
# Go workspace (docs/development/go-workspaces.md), and the fake resolves its
# siblings through its own replace directives, so the gate does not depend on
# FAKE_SRC being a workspace member.
#
# A build that fails exits 2, not 1: the check was never taken, and reporting
# that as a finding would send someone looking for an endpoint that is missing
# from the venue rather than for a broken compile.
build_or_die() {
    local dir="$1" out="$2" pkg="$3"
    if ! (cd "$dir" && GOWORK=off go build -o "$out" "$pkg"); then
        printf 'check-endpoint-parity: could not build %s in %s\n' "$pkg" "$dir" >&2
        exit 2
    fi
}

checker="$SCRIPT_DIR/../../.build/endpointparity"
mkdir -p "$(dirname "$checker")"
build_or_die "$DEVTOOLS_DIR" "$checker" ./e2e/endpointparity

# The fake gets a path of its own per process: FAKE_SRC is an argument, so two
# invocations with different sources would otherwise write one binary. It stays
# under the gitignored .build/ rather than host-wide temp, which is shared across
# every worktree on the machine.
BUILD_DIR="$SCRIPT_DIR/../../.build/endpoint-parity-fake.$$"
mkdir -p "$BUILD_DIR"
trap 'rm -rf "$BUILD_DIR"' EXIT
fake="$BUILD_DIR/fakegithub"
build_or_die "$FAKE_SRC" "$fake" .

"$checker" "$fake" "${SRC_ROOTS[@]}"
