#!/usr/bin/env bash
#
# Run one module's envtest-backed integration suite, and report the suite's own
# wall clock against the budget `-timeout` gives it.
# Backs `make -C cmd/agc test-integration`, `make -C cmd/gmc test-integration`
# and the two `go test` steps in .github/workflows/integration-test.yml.
#
# Usage: scripts/go/go-test-integration.sh <module-dir> [go test args...]
#   <module-dir>  a go.work module holding an envtest suite: cmd/agc | cmd/gmc.
#   Extra args are appended to `go test`; the package pattern defaults to
#   ./internal/controller/integration/... when none is given.
#
# Env:
#   KUBEBUILDER_ASSETS       required by envtest; the callers set it.
#   INTEGRATION_TIMEOUT      the per-binary budget (default below).
#   INTEGRATION_BUDGET_WARN  warn at or above this percent of it (default 80).
#
# Why the wrapper exists: when a test binary exceeds -timeout, Go panics and
# names whichever test held the wall at that instant. That test is a bystander —
# a suite that merely got slower reads as one test failing, and the name moves
# from run to run. Q166 spent two wrong hypotheses on a red GMC suite that way,
# first blaming the session's own concurrent runs and then a List the branch had
# added, before origin/main alone measured the same breach with none of the
# branch's code (Q741). So this prints the package total against the budget on
# every run, and on a breach says outright that the named test is not the cause.
#
# The budget is single-sourced here because it had been written out at four call
# sites — two module Makefiles and two CI steps — which is three places to miss.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# The budget. Measured 2026-08-10, solo: GMC 197-220s local / 201-218s on CI,
# AGC 256-258s local / 259-264s on CI. The two environments agree per test to
# within hundredths of a second and no single test dominates either suite, so
# neither a split nor a marginal bump addresses what breached the old 300s —
# which no measurement reproduced. 10m leaves the slowest suite ~2.3x headroom,
# makes a breach mean something, and still bounds a hang well inside the CI
# job's own timeout-minutes. Distributions and the refuted mechanisms:
# docs/development/testing.md#the-envtest-suite-budget.
INTEGRATION_TIMEOUT="${INTEGRATION_TIMEOUT:-10m}"
WARN_PERCENT="${INTEGRATION_BUDGET_WARN:-80}"

module="${1:-}"
case "$module" in
	cmd/agc | cmd/gmc) shift ;;
	"") echo "usage: $0 <module-dir> [go test args...]" >&2; exit 2 ;;
	*) echo "$0: unknown module '$module' (want cmd/agc or cmd/gmc)" >&2; exit 2 ;;
esac

# Serialize against a concurrent heavy build on this machine (a no-op on
# CI/headless), the same machine-wide semaphore `make check` takes. The
# integration tier held no slot until Q741, so a sibling worktree's gate landed
# on a suite already in flight. Two suites cost each other 4-8%, well short of
# the excursion Q741 recorded, so this bounds a wider dispatch wave rather than
# fixing a measured cause.
serialize_heavy_build "$module" "$@"

init_throttle

args=("$@")
((${#args[@]} == 0)) && args=(./internal/controller/integration/...)

log="$REPO_ROOT/tmp/go-test-integration.$$.log"
mkdir -p "$REPO_ROOT/tmp"
trap 'rm -f "$log"' EXIT INT TERM

echo "==> go test -race -tags integration -timeout $INTEGRATION_TIMEOUT ${args[*]} ($module)"
rc=0
(
	cd "$module"
	[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"
	# -count=1 is deliberately NOT forced: the callers that want an uncached run
	# pass it, and a local re-run of an unchanged tree should still replay from
	# the test cache in seconds. A cached package prints no elapsed, so the
	# budget line below correctly says nothing about a run that did not happen.
	# shellcheck disable=SC2086  # the throttle prefix word-splits into command + args intentionally
	$THROTTLE_PREFIX go test -race -tags integration \
		-timeout "$INTEGRATION_TIMEOUT" "${args[@]}"
) 2>&1 | tee "$log" || rc=$?

# The slowest package binary in the run — the one nearest the budget, and under
# the default pattern the only one. `go test` ends each package with an
# `ok`/`FAIL <pkg> <elapsed>s` line, verbose or not.
slowest="$(awk '$1 == "ok" || $1 == "FAIL" {
	for (i = 2; i <= NF; i++) if ($i ~ /^[0-9]+\.[0-9]+s$/) {
		sub(/s$/, "", $i)
		if ($i + 0 > max) { max = $i + 0; pkg = $2 }
	}
} END { if (max > 0) printf "%s %.1f", pkg, max }' "$log")"

# The budget in seconds, for the two Go duration forms this gate is ever given.
# An unrecognized one yields 0 and suppresses the percentage rather than
# reporting a wrong one.
budget_seconds="$(awk -v d="$INTEGRATION_TIMEOUT" 'BEGIN {
	if (d ~ /^[0-9]+m$/) { sub("m", "", d); print d * 60 }
	else if (d ~ /^[0-9]+s$/) { sub("s", "", d); print d + 0 }
	else print 0
}')"

if [[ -n "$slowest" && "$budget_seconds" != 0 ]]; then
	read -r pkg elapsed <<<"$slowest"
	percent="$(awk -v e="$elapsed" -v b="$budget_seconds" 'BEGIN { printf "%.0f", 100 * e / b }')"
	line="==> $pkg used ${elapsed}s of its ${INTEGRATION_TIMEOUT} budget (${percent}%)"
	if ((percent >= WARN_PERCENT)); then
		echo "$line: within ${WARN_PERCENT}% of the budget, so the next run may breach it" >&2
	else
		echo "$line"
	fi
fi

# The breach. Say it in the terms the failure is actually in, and — on CI, where
# the annotation is all most readers see — as an annotation too.
if grep -q '^panic: test timed out after ' "$log"; then
	cat >&2 <<EOF

==> the ${module} integration suite ran out of its ${INTEGRATION_TIMEOUT} budget
    The panic above names whichever test held the wall when the budget expired.
    That test is a bystander: its name moves between runs, and a suite that got
    slower everywhere reads as one test failing (Q166/Q741). Compare the package
    total against the budget before suspecting the test it named.
    docs/development/testing.md#the-envtest-suite-budget
EOF
	if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
		echo "::error title=integration suite over budget::the ${module} suite exceeded its ${INTEGRATION_TIMEOUT} -timeout budget. The test named in the panic is whichever one held the wall, not the cause (Q166/Q741)."
	fi
fi

exit "$rc"
