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
# A second, forced `-count=1` pass covers the packages go's test cache cannot
# key at all (see below); the out-of-module read gate names them.
#
# Backs `make test` and `make test-race`.
#
# Usage: scripts/go/go-test.sh [--race]
#   --race   Run under the race detector (the CI unit gate). ~2-10× CPU/
#            memory/I/O amplifier, so the timeout is bumped from 2m to 5m.
#
# Env:
#   RUN          Non-empty narrows the run to tests whose name matches this
#                regex (`go test -run`), the spelling cmd/agc and cmd/gmc
#                already use on their integration targets (Q592). A value that
#                matches nothing FAILS the run: `go test -run` exits 0 with
#                "[no tests to run]", so a mistyped name otherwise reports a
#                green suite — the knob's whole purpose inverted (Q680).
#   V / VERBOSE  Non-empty streams test output live (-v), bypassing the
#                heartbeat renderer below — the two want opposite things, one
#                showing every line as it is produced and the other showing
#                only what failed. Turn it on when you want a specific test's
#                own output as it runs.
#   TEST_PROGRESS_INTERVAL
#                Seconds between heartbeat lines (default 30); 0 runs plain
#                `go test` with no renderer at all. The same knob paces the
#                e2e watcher (scripts/e2e/progress-watch.sh).
#
# Progress. The run streams through devtools/gotest/progress, which turns
# `go test -json` back into an ordinary test log and interleaves one heartbeat
# line per interval naming what is still running. The measured problem is the
# -race gate: 200 s at 8 % output density, 58 s between lines, and a deadlocked
# test that says nothing whatsoever until -timeout fires. -json is what makes
# the fix possible — plain `go test` both buffers a package until it ends and
# releases packages in command-line order, so nothing can be reported live.
# Rendering from the -json stream also means no test changed. Details:
# docs/development/testing.md § Watching a unit run in progress.
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
# with the flag, same coverage numbers). See the local-gate-throughput plan
# (indexed in docs/plan/README.md). It is deliberately NOT set globally via
# GOFLAGS: cmd/gmc/test/e2e resolves the v2 CRD chart dir from
# runtime.Caller(0), which a trimmed path breaks. The unit tier does not do
# that, and the release images already build with -trimpath.
set -euo pipefail
shopt -s inherit_errexit

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

# RUN narrows to matching test names. -v -count=1 ride along, as they do on the
# integration targets: a targeted run wants its own output, and an uncached one
# is what lets the zero-match check at the bottom see whether anything ran —
# a cached package replays nothing. -v also takes the plain path below, so the
# whole run is capturable through one tee.
run_args=() run_log=""
if [[ -n "${RUN:-}" ]]; then
	run_args=(-run "$RUN" -count=1)
	verbose_flag="-v"
	run_log="$(mktemp)"
	trap 'rm -f "$run_log"' EXIT
fi

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

PROGRESS_INTERVAL="${TEST_PROGRESS_INTERVAL:-30}"
PROGRESS_BIN="$REPO_ROOT/.build/gotest-progress"
# Trimmed from package names in the heartbeat, so a running test reads
# `broker.TestLeaseExpiry` rather than a 55-character import path. Every module
# in go.work and devtools/ sits under it; a stale value costs line width only.
PROGRESS_STRIP="github.com/actions-gateway/github-actions-gateway/"

progress_args=()
if [[ -z "$verbose_flag" && "$PROGRESS_INTERVAL" != 0 ]]; then
	mkdir -p "$REPO_ROOT/.build"
	if (cd "$REPO_ROOT/devtools" && GOWORK=off go build -o "$PROGRESS_BIN" ./gotest/progress); then
		progress_args=(-label unit -interval "${PROGRESS_INTERVAL}s" -strip "$PROGRESS_STRIP")
	else
		echo "==> heartbeat renderer failed to build; running without progress" >&2
	fi
fi

# run_tests PATTERN... — one `go test` invocation, piped through the heartbeat
# renderer unless progress is off. The renderer always exits 0, so pipefail
# makes the run's verdict `go test`'s own status: progress reporting is never
# what fails, or passes, the gate.
run_tests() {
	local total
	if [[ -n "$run_log" ]]; then
		# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
		$THROTTLE_PREFIX go test -trimpath $race_flag -timeout "$timeout" $p_flag $verbose_flag \
			"${run_args[@]}" "$@" | tee -a "$run_log"
		return
	fi
	if ((${#progress_args[@]} == 0)); then
		# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
		$THROTTLE_PREFIX go test -trimpath $race_flag -timeout "$timeout" $p_flag $verbose_flag "$@"
		return
	fi
	# The denominator, for ~0.3 s and no compilation. A `go test -list` pre-pass
	# would buy a test-level one too, but it costs a full build of every test
	# binary ahead of the run — measured at 22 s wall / 131 s CPU with a warm
	# cache — and serializes the compile the single invocation exists to overlap.
	total=$(go list -e "$@" 2>/dev/null | grep -c . || true)
	# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
	$THROTTLE_PREFIX go test -trimpath $race_flag -timeout "$timeout" $p_flag -json "$@" \
		| "$PROGRESS_BIN" "${progress_args[@]}" -packages "$total"
}

echo "==> go test ${race_flag:+$race_flag }${RUN:+-run $RUN }${patterns[*]}"
run_tests "${patterns[@]}"

# A second, forced pass over the packages `go test` cannot key a result for: a
# test that derives its root at runtime, or whose reads happen in a subprocess
# the testlog never sees. Measured 2026-08-21 (Q936) on cmd/probe/compat —
# warm, `(cached)`, then an `_ "net/http/httptest"` import added to cmd/proxy's
# package main still replayed `(cached)` and exited 0, while -count=1 failed.
# The symlink fix does not reach these, because there is no path to rewrite.
# The package list comes from the out-of-module read gate's own UNCACHED map, so
# the two cannot drift: that gate fails when an undeclared one appears. The
# packages replay from cache in the run above, so the cost here is their cold
# run alone (cmd/probe/compat: ~2.4 s). A RUN= run already forces -count=1.
if [[ -z "${RUN:-}" ]]; then
	uncached=()
	while IFS= read -r pkg; do
		[[ -n "$pkg" ]] || continue
		uncached+=("./$pkg")
	done < <(scripts/go/check-test-cache-inputs.sh --uncached-packages)
	if ((${#uncached[@]} > 0)); then
		echo "==> go test -count=1 ${race_flag:+$race_flag }${uncached[*]}"
		# shellcheck disable=SC2086  # flag strings and the throttle prefix word-split intentionally
		$THROTTLE_PREFIX go test -trimpath $race_flag -timeout "$timeout" $p_flag $verbose_flag \
			-count=1 "${uncached[@]}"
	fi
fi

# The single invocation above resolves against go.work, which cannot reach a
# module the workspace does not list, so those run separately with GOWORK=off.
for dir in $(firstparty_nonworkspace_modules); do
	echo "==> go test ${race_flag:+$race_flag }${RUN:+-run $RUN }./$dir/... (GOWORK=off)"
	(
		cd "$dir"
		export GOWORK=off
		run_tests ./...
	)
done

# The zero-match guard, over every module's output at once: a regex may match
# nothing in the workspace and still be right about devtools/. `=== RUN` is the
# per-test marker -v emits, so its total absence means the filter selected no
# test anywhere — which `go test` alone reports as a pass.
if [[ -n "$run_log" ]] && ! grep -q '^=== RUN ' "$run_log"; then
	echo "==> RUN='$RUN' matched no tests in any module — nothing ran" >&2
	exit 1
fi
