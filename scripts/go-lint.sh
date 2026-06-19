#!/usr/bin/env bash
#
# Lint the Go workspace: a gofmt formatting check across every go.work module,
# then golangci-lint (which includes govet) per module. Backs `make lint` and
# the `lint` job in .github/workflows/unit-test.yml.
#
# Env:
#   GOLANGCI_LINT  Path to the golangci-lint binary (default .build/golangci-lint
#                  at the repo root — build it with `make golangci-lint`).
#
# Applies the local throttle (GOMAXPROCS + `-j` cap and a low-priority QoS
# prefix) on a GUI dev shell; a no-op on CI/headless — see
# scripts/local-throttle.sh. golangci-lint ignores GOMAXPROCS, so the `-j`
# flag is the lever that actually caps its fan-out.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# Serialize against a concurrent heavy build on this machine (no-op on
# CI/headless) so two sessions don't saturate the cores and push golangci-lint
# past its deadline; re-execs self under a machine-wide lock.
serialize_heavy_build "$@"

GOLANGCI_LINT="${GOLANGCI_LINT:-$REPO_ROOT/.build/golangci-lint}"
if [[ ! -x "$GOLANGCI_LINT" ]]; then
	echo "golangci-lint not found at $GOLANGCI_LINT — build it with: make golangci-lint" >&2
	exit 1
fi

# shellcheck disable=SC2046  # module paths word-split intentionally (no spaces in go.work paths)
unformatted="$(gofmt -l $(workspace_modules))"
if [[ -n "$unformatted" ]]; then
	echo "gofmt: the following files are not formatted:"
	echo "$unformatted"
	echo "run: gofmt -w <file>"
	exit 1
fi

init_throttle
j_flag=""
[[ -n "$THROTTLE_JOBS" ]] && j_flag="-j $THROTTLE_JOBS"

for dir in $(workspace_modules); do
	echo "==> golangci-lint $dir"
	(
		cd "$dir"
		[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"
		# shellcheck disable=SC2086  # flag string and the throttle prefix word-split intentionally
		$THROTTLE_PREFIX "$GOLANGCI_LINT" run $j_flag --config "$REPO_ROOT/.golangci.yml" ./...
	)
done
