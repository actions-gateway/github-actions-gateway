#!/usr/bin/env bash
#
# Run the per-module unit tests across the Go workspace (`go test` in each
# go.work module — a repo-root `./...` does not work in a workspace). Backs
# `make test` and `make test-race`.
#
# Usage: scripts/go-test.sh [--race]
#   --race   Run under the race detector (the CI unit gate). ~2-10× CPU/
#            memory/I/O amplifier, so the timeout is bumped from 2m to 5m.
#
# Env:
#   V / VERBOSE  Non-empty streams test output live (-v). Off by default so
#                the green path stays compressed — go test already prints one
#                `ok pkg` line per passing package and the full output of any
#                package that fails. Turn it on when debugging a slow or
#                hanging test: without -v, go test buffers each package's
#                output until the package completes, so a hung test shows
#                nothing (not even its t.Log lines) until it finishes or hits
#                -timeout; with -v the output streams as it is produced.
#
# Applies the local throttle (GOMAXPROCS + `go test -p` cap and a low-priority
# QoS prefix) on a GUI dev shell; a no-op on CI/headless — see
# scripts/local-throttle.sh.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# Serialize against a concurrent heavy build on this machine (no-op on
# CI/headless) so sibling runs queue instead of saturating the cores; re-execs
# self under a machine-wide lock. Passes "$@" so the re-exec keeps --race.
serialize_heavy_build "$@"

race_flag="" timeout=2m
case "${1:-}" in
	--race) race_flag="-race"; timeout=5m ;;
	"") ;;
	*) echo "usage: $0 [--race]" >&2; exit 2 ;;
esac

verbose_flag=""
[[ -n "${V:-}${VERBOSE:-}" ]] && verbose_flag="-v"

init_throttle
p_flag=""
[[ -n "$THROTTLE_JOBS" ]] && p_flag="-p $THROTTLE_JOBS"

for dir in $(workspace_modules); do
	echo "==> go test ${race_flag:+$race_flag }$dir"
	(
		cd "$dir"
		[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"
		# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
		$THROTTLE_PREFIX go test $race_flag -timeout "$timeout" $p_flag $verbose_flag ./...
	)
done
