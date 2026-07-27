#!/usr/bin/env bash
#
# validate-throttle.sh — decide a throttle prefix on evidence, not on a spin loop.
#
# scripts/qos-cluster-probe.sh established what share of the machine each candidate
# prefix can reach (`taskpolicy -c utility` confines a build to one CPU cluster,
# ~21% of an M5 Max; `taskpolicy -d throttle` reaches ~96%). That measured
# compute ceiling with synthetic spin threads — no I/O, no memory pressure, no
# process churn — so it cannot answer the question that gates the change: does
# the faster prefix still leave the desktop responsive under a real build?
#
# This runs a real `make check` phase under each candidate while scripts/uijitter.c
# samples scheduling latency at the QoS tier the compositor runs at.
#
# SATURATION IS THE PRECONDITION, NOT A DETAIL. Two earlier workload choices both
# produced confident-looking nulls. `cmd/agc` alone: all three candidates at an
# identical 14 s, jitter pinned to the idle floor. The full workspace race tier:
# all three at an identical 42 s and only 12-14% mean CPU — about 2.3 of 18
# cores. Neither had loaded the machine, so neither could discriminate between
# prefixes or exercise desktop contention, yet both read as a clean bill of
# health. The harness now samples CPU busy% throughout each trial and marks any
# trial below MIN_SATURATION_PCT as INVALID.
#
# The default workload is therefore `lint`, not the race tier: golangci-lint
# fans out one worker per logical CPU and ignores GOMAXPROCS, and the repo's own
# baseline (docs/plan/local-gate-throughput.md) puts `make check` cost in lint
# and coverage — `-race` is not part of `make check` at all.
#
# Read a VALID row as a trade: wall_s is what the prefix buys, p99/max/over_* are
# what it costs. A prefix that cuts wall_s while holding p99 near the idle floor
# is strictly better; one that pushes p99 into the hundreds of ms is producing
# visible stutter whatever the throughput column says.
#
# Usage:
#   scripts/validate-throttle.sh                 # lint phase, 1 trial per candidate
#   scripts/validate-throttle.sh 3               # 3 trials per candidate
#   scripts/validate-throttle.sh 1 race          # race tier instead (expect INVALID)
#
# Needs .build/golangci-lint (`make golangci-lint`). No sudo required.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
# Scratch (probe output, iostat captures, cold caches) stays in the gitignored
# tmp/; the built probe follows the .build/ convention used for golangci-lint.
readonly TMP_DIR="${REPO_ROOT}/tmp"
readonly PROBE="${REPO_ROOT}/.build/uijitter"
readonly PROBE_SRC="${REPO_ROOT}/scripts/uijitter.c"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
readonly REPORTS_DIR="${HOME}/Library/Logs/DiagnosticReports"

readonly TRIALS="${1:-1}"
# Which phase to measure. `lint` is the default because it is the one that
# actually saturates: golangci-lint fans out one worker per logical CPU and
# ignores GOMAXPROCS (scripts/local-throttle.sh), and `make check` is dominated
# by lint + coverage. The `race` tier is kept for contrast — measured at 12-14%
# mean CPU across all 10 modules, i.e. ~2.3 of 18 cores, far too little to
# discriminate between candidate prefixes.
readonly WORKLOAD="${2:-lint}"
readonly GOLANGCI_LINT="${REPO_ROOT}/.build/golangci-lint"
# Heaviest module, so one cold-cache lint is enough to load the machine.
readonly LINT_MODULE="${3:-cmd/agc}"

# Below this mean CPU busy%, the workload did not load the machine and the trial
# cannot distinguish the candidates or exercise desktop contention.
#
# Judged per TRIAL against the highest CPU% any candidate reached, never per row.
# A restrictive candidate showing low CPU is the effect under measurement, not a
# failed measurement: `taskpolicy -c utility` runs the cold-cache lint at 37-39%
# precisely because the clamp is working, and an earlier per-row version of this
# check libelled exactly those rows INVALID while the real conclusion sat in
# them. The workload is adequate as soon as the least restrictive candidate
# saturates.
readonly MIN_SATURATION_PCT=50

# Candidates, current-first so the baseline is established before anything is
# compared to it. The unthrottled prefix is deliberately absent: its compute
# ceiling is already known and it is the configuration that historically froze
# the GUI, so there is nothing to learn by running it against a real -race build.
readonly CANDIDATES=(
	'taskpolicy -c utility'             # current: scripts/local-throttle.sh
	'nice -n 10 taskpolicy -d throttle' # proposed: I/O demoted, CPU deprioritized
	'taskpolicy -d throttle'            # aggressive: I/O demoted only
)

probe_pid=""
iostat_pid=""

# cleanup stops background samplers if the harness exits early. Armed by main(),
# not at load time, so sourcing this file for its parsers installs no traps.
cleanup() {
	[[ -n "$probe_pid" ]] && kill -TERM "$probe_pid" 2>/dev/null || true
	[[ -n "$iostat_pid" ]] && kill -TERM "$iostat_pid" 2>/dev/null || true
	probe_pid=""
	iostat_pid=""
}

# windowserver_reports counts WindowServer crash/watchdog reports in $1 (default
# $REPORTS_DIR). Any increase across a trial is a hard fail — that is the exact
# failure the throttle prevents. A missing directory counts 0: a Mac that has
# never crashed WindowServer has no DiagnosticReports entry to find.
windowserver_reports() {
	local dir="${1:-$REPORTS_DIR}"
	local f count=0
	shopt -s nullglob nocaseglob
	for f in "$dir"/*windowserver*; do
		[[ -e "$f" ]] && (( count++ ))
	done
	shopt -u nullglob nocaseglob
	printf '%s' "$count"
}

# parse_swapins reads vm_stat output on stdin and prints the cumulative swapin
# counter. Split from swapins() so it can be exercised against recorded text.
#
# Prints 0 rather than nothing when the counter is absent: the caller subtracts
# two readings, and an empty operand makes that delta silently meaningless.
# Prints exactly one line, so the caller's delta arithmetic can never be handed
# "0\n0". Matches on the first Swapins line rather than exiting early, so vm_stat
# is never handed a closed pipe.
parse_swapins() {
	awk '/Swapins/ && !found { gsub(/[^0-9]/, "", $NF); print $NF; found = 1 }
	     END { if (!found) print 0 }'
}

# swapins prints the cumulative swapin counter, whose delta shows whether a
# candidate pushed the machine into memory pressure.
swapins() {
	vm_stat | parse_swapins
}

# mean_busy_pct averages (100 - idle) over an iostat capture. iostat's trailing
# columns are "us sy id 1m 5m 15m", so idle is the 4th field from the end
# regardless of how many disks the machine reports.
#
# The first data row is iostat's since-boot average, not an interval sample, so
# it is skipped — including it dragged a calibration run of 18 spin threads from
# 100% down to 89%.
#
# The NF >= 4 guard is load-bearing, not defensive noise: awk treats $(NF-3) on a
# short or blank line as a negative field index, which is a FATAL error, not an
# empty string. Without the guard one blank line in the capture aborts the whole
# harness under `set -e`, discarding every candidate measured so far in the trial.
# iostat's own header block is skipped by the same test that skips it today — the
# idle column reads "id" there, which is not numeric — and that matters because
# iostat reprints the header every 20 data rows, so any trial long enough to be
# worth running contains at least one.
mean_busy_pct() {
	awk 'NF >= 4 && $(NF-3) ~ /^[0-9]+$/ { if (++row == 1) next; sum += 100 - $(NF-3); n++ }
	     END { printf "%.0f", (n > 0) ? sum / n : 0 }' "$1"
}

# run_race runs the full unit race tier as one invocation, matching
# scripts/go-test.sh: -count=1 defeats the result cache, -trimpath matches what
# the real script passes.
run_race() {
	local jobs="$2"
	local pfx=() patterns=() dir
	[[ -n "$1" ]] && read -r -a pfx <<<"$1"
	for dir in $(workspace_modules); do patterns+=("$dir/..."); done
	(
		cd "$REPO_ROOT" || exit 1
		GOMAXPROCS="$jobs" "${pfx[@]}" go test -trimpath -race -count=1 \
			-timeout 30m -p "$jobs" "${patterns[@]}" >/dev/null 2>&1
	) || true
}

# run_lint lints one heavy module under the candidate prefix, mirroring
# scripts/go-lint.sh's invocation but with BOTH caches cold.
#
# GOCACHE is the load-bearing one. Busting GOLANGCI_LINT_CACHE alone leaves the
# linter reading type information from the warm Go build cache, and the whole
# api module then lints in 1 s — another null result. With a cold GOCACHE,
# cmd/agc measures 29 s at 54% mean CPU, which is enough to separate candidates.
#
# Cold-cache is also the regime that matters: it is what a fresh worktree pays,
# and the only case where the local gate is genuinely CPU-heavy. Each trial
# writes ~1.1 GB into tmp/ and removes it afterwards.
run_lint() {
	local jobs="$2" gocache lintcache
	local pfx=()
	[[ -n "$1" ]] && read -r -a pfx <<<"$1"
	gocache="$(mktemp -d "${TMP_DIR}/gocache.XXXXXX")"
	lintcache="$(mktemp -d "${TMP_DIR}/golangci-cache.XXXXXX")"
	(
		cd "${REPO_ROOT}/${LINT_MODULE#./}" || exit 1
		GOCACHE="${REPO_ROOT}/${gocache#"${REPO_ROOT}/"}" \
			GOLANGCI_LINT_CACHE="${REPO_ROOT}/${lintcache#"${REPO_ROOT}/"}" \
			"${pfx[@]}" "$GOLANGCI_LINT" run -j "$jobs" \
			--config "${REPO_ROOT}/.golangci.yml" ./... >/dev/null 2>&1
	) || true
	rm -rf "$gocache" "$lintcache"
}

# run_workload dispatches to the selected phase and prints elapsed seconds.
run_workload() {
	local started ended
	started="$(date +%s)"
	case "$WORKLOAD" in
		lint) run_lint "$1" "$2" ;;
		race) run_race "$1" "$2" ;;
		*) printf 'unknown workload: %s\n' "$WORKLOAD" >&2; exit 2 ;;
	esac
	ended="$(date +%s)"
	printf '%s' "$(( ended - started ))"
}

# measure runs one candidate once and prints a TSV row.
measure() {
	local prefix="$1" jobs="$2" trial="$3"
	local ws_before ws_after sw_before sw_after wall jitter busy
	local io_out="${TMP_DIR}/iostat.out"

	ws_before="$(windowserver_reports)"
	sw_before="$(swapins)"

	"$PROBE" >"${TMP_DIR}/uijitter.out" 2>/dev/null &
	probe_pid="$!"
	iostat -c 100000 -w 1 >"$io_out" 2>/dev/null &
	iostat_pid="$!"

	wall="$(run_workload "$prefix" "$jobs")"

	kill -TERM "$probe_pid" 2>/dev/null || true
	wait "$probe_pid" 2>/dev/null || true
	probe_pid=""
	kill -TERM "$iostat_pid" 2>/dev/null || true
	wait "$iostat_pid" 2>/dev/null || true
	iostat_pid=""

	jitter="$(cat "${TMP_DIR}/uijitter.out")"
	busy="$(mean_busy_pct "$io_out")"
	ws_after="$(windowserver_reports)"
	sw_after="$(swapins)"

	printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$prefix" "$trial" "$wall" "$jitter" \
		"$(( sw_after - sw_before ))|$(( ws_after - ws_before ))" "$busy"
}

# field extracts a key=value field from the probe's report line.
field() {
	printf '%s' "$1" | tr ' ' '\n' | awk -F= -v k="$2" '$1 == k { print $2 }'
}

main() {
	trap cleanup EXIT INT TERM

	if [[ "$(uname -s)" != "Darwin" ]]; then
		printf 'validate-throttle: macOS only\n' >&2
		return 2
	fi
	if [[ ! -x "$PROBE" ]]; then
		printf 'validate-throttle: building %s\n' "$PROBE"
		require_cmd clang "https://clang.llvm.org/get_started.html"
		mkdir -p "$(dirname "$PROBE")"
		clang -O2 -o "$PROBE" "$PROBE_SRC"
	fi

	mkdir -p "$TMP_DIR"

	local jobs
	jobs="$("${REPO_ROOT}/scripts/local-throttle.sh" jobs)"
	[[ -n "$jobs" ]] || jobs="$(sysctl -n hw.physicalcpu)"

	if [[ "$WORKLOAD" == "lint" && ! -x "$GOLANGCI_LINT" ]]; then
		printf 'validate-throttle: %s missing — build it with: make golangci-lint\n' \
			"$GOLANGCI_LINT" >&2
		return 2
	fi

	printf '==> workload=%s%s trials=%s jobs=%s\n' "$WORKLOAD" \
		"$([[ "$WORKLOAD" == "lint" ]] && printf ' (cold cache, %s)' "$LINT_MODULE")" \
		"$TRIALS" "$jobs"
	printf '==> idle jitter floor: %s\n' "$("$PROBE" 16.667 5)"

	# No warmup for the lint workload: every trial deliberately starts from a cold
	# GOCACHE, so there is no shared first-run cost for a warmup to absorb. The
	# race tier does need one, since its first run compiles instrumented deps.
	if [[ "$WORKLOAD" == "race" ]]; then
		printf '==> warming the build cache (unmeasured, several minutes)\n'
		run_workload '' "$jobs" >/dev/null
	fi

	printf '\n%-36s %5s %7s %5s %8s %8s %8s %7s %7s %s\n' \
		'PREFIX' 'TRIAL' 'WALL_S' 'CPU%' 'p50_ms' 'p99_ms' 'max_ms' '>50ms' '>250ms' 'SWAPIN|WS'
	printf '%s\n' '-------------------------------------------------------------------------------------------------------------------'

	local trial pfx row jit busy peak note
	local rows=()
	for (( trial = 1; trial <= TRIALS; trial++ )); do
		# Collect the whole trial before printing: validity is a property of the
		# trial (did ANY candidate load the machine?), not of an individual row.
		rows=()
		peak=0
		for pfx in "${CANDIDATES[@]}"; do
			row="$(measure "$pfx" "$jobs" "$trial")"
			rows+=("$row")
			busy="$(printf '%s' "$row" | cut -f6)"
			(( busy > peak )) && peak="$busy"
		done

		note=''
		(( peak < MIN_SATURATION_PCT )) && note='  <-- INVALID TRIAL: no candidate loaded the machine'

		for row in "${rows[@]}"; do
			jit="$(printf '%s' "$row" | cut -f4)"
			printf '%-36s %5s %7s %4s%% %8s %8s %8s %7s %7s %s%s\n' \
				"$(printf '%s' "$row" | cut -f1)" \
				"$(printf '%s' "$row" | cut -f2)" \
				"$(printf '%s' "$row" | cut -f3)" \
				"$(printf '%s' "$row" | cut -f6)" \
				"$(field "$jit" p50_ms)" "$(field "$jit" p99_ms)" \
				"$(field "$jit" max_ms)" "$(field "$jit" over_50ms)" \
				"$(field "$jit" over_250ms)" \
				"$(printf '%s' "$row" | cut -f5)" "$note"
		done
	done

	printf '\nCPU%% = mean busy (100 - idle) over the trial. A trial is INVALID only when NO\n'
	printf 'candidate reached %s%%: the workload was then too small to load the machine.\n' "$MIN_SATURATION_PCT"
	printf 'A low CPU%% on one candidate alone is the throttle working, not a bad measurement.\n'
	printf 'SWAPIN|WS = swapins during the trial | new WindowServer reports (must be 0).\n'
	printf 'Candidates are interleaved within each trial so thermal drift hits them equally.\n'
}

# Run main only when executed directly, so validate-throttle-test.sh can source
# this file to exercise the pure parsers against recorded instrument output —
# the measurement paths are macOS-only, the text-to-number parsers are not.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
