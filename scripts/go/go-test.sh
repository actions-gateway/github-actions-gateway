#!/usr/bin/env bash
#
# Run the unit tests across the Go workspace in a single `go test` invocation
# covering every go.work module (a repo-root `./...` does not work in a
# workspace, but explicit per-module patterns — `./api/... ./broker/... …` —
# do). One invocation lets Go schedule the whole workspace as a single build
# graph: modules no longer compile and test one after another, so the many
# small modules overlap with the big cmd/agc / cmd/gmc dependency compiles.
# Measured on the CI -race unit gate: 189s → 163s (Q17) — the 4-vCPU runner
# was already near CPU-bound during the big compiles, so the win is the
# removal of the serial inter-module barriers, not a multiple.
# Backs `make test` and `make test-race`.
#
# Usage: scripts/go/go-test.sh [--race]
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
# scripts/agent/local-throttle.sh.
#
# -trimpath is load-bearing for parallel worktree sessions, not a build-hygiene
# nicety: it removes the absolute worktree path from the test binary, which
# makes go's test-RESULT cache key identical across checkouts of the same
# content. Without it every .claude/worktrees/* clone re-runs the whole unit
# suite once (measured: cmd/agc coverage 226s cold vs 5s in a second worktree
# with the flag, byte-identical profile). See
# docs/plan/archive/local-gate-throughput.md. It is deliberately NOT set globally via
# GOFLAGS: cmd/gmc/test/e2e resolves the v2 CRD chart dir from
# runtime.Caller(0), which a trimmed path breaks. The unit tier does not do
# that, and the release images already build with -trimpath.
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

# One `./<module>/...` pattern per go.work module. Unit-only stays intact:
# the integration (envtest) and e2e packages are build-tagged, so they no-op
# under these patterns exactly as they did under a per-module `./...`.
patterns=()
for dir in $(workspace_modules); do
	patterns+=("$dir/...")
done

[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"
echo "==> go test ${race_flag:+$race_flag }${patterns[*]}"
# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
$THROTTLE_PREFIX go test -trimpath $race_flag -timeout "$timeout" $p_flag $verbose_flag "${patterns[@]}"

# The single invocation above resolves against go.work, which cannot reach a
# module the workspace does not list, so those run separately with GOWORK=off.
for dir in $(firstparty_nonworkspace_modules); do
	echo "==> go test ${race_flag:+$race_flag }./$dir/... (GOWORK=off)"
	(
		cd "$dir"
		export GOWORK=off
		# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
		$THROTTLE_PREFIX go test -trimpath $race_flag -timeout "$timeout" $p_flag $verbose_flag ./...
	)
done
