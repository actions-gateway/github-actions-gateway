#!/usr/bin/env bash
#
# validate-throttle.sh — decide a throttle prefix on evidence, not on a spin loop.
#
# scripts/agent/qos-cluster-probe.sh established what share of the machine each candidate
# prefix can reach (`taskpolicy -c utility` confines a build to one CPU cluster,
# ~21% of an M5 Max; `taskpolicy -d throttle` reaches ~96%). That measured
# compute ceiling with synthetic spin threads — no I/O, no memory pressure, no
# process churn — so it cannot answer the question that gates the change: does
# the faster prefix still leave the desktop responsive under a real build?
#
# This runs a real `make check` phase under each candidate while scripts/agent/uijitter.c
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
# baseline (the local-gate-throughput plan, indexed in docs/plan/README.md) puts
# `make check` cost in lint
# and coverage — `-race` is not part of `make check` at all.
#
# Read a VALID row as a trade: wall_s is what the prefix buys, p99/max/over_* are
# what it costs. A prefix that cuts wall_s while holding p99 near the idle floor
# is strictly better; one that pushes p99 into the hundreds of ms is producing
# visible stutter whatever the throughput column says.
#
# Usage:
#   scripts/agent/validate-throttle.sh                 # lint phase, 1 trial per candidate
#   scripts/agent/validate-throttle.sh 3               # 3 trials per candidate
#   scripts/agent/validate-throttle.sh 2 test          # unit tier instead (`go test -p`)
#   scripts/agent/validate-throttle.sh 1 race          # race tier instead (expect INVALID)
#
# GAG_THROTTLE_CANDIDATES (newline-separated) replaces the candidate list, so the
# same harness re-derives the OTHER two knobs once a prefix is settled. Each entry
# is a prefix optionally followed by `|jobs=N` and/or `|holders=M`:
#
#   jobs=N     parallelism for this row (golangci-lint -j / go test -p), instead
#              of whatever scripts/agent/local-throttle.sh currently sizes.
#   holders=M  run M copies of the workload CONCURRENTLY, as M sibling worktree
#              sessions holding M semaphore slots would. WALL_S is then the time
#              until the last one finishes, so throughput is M/WALL_S — a second
#              holder pays for itself only when WALL_S grows by less than 2x.
#
#   # sweep parallelism under one prefix, interleaved so drift hits rows equally
#   GAG_THROTTLE_CANDIDATES=$'nice -n 10 taskpolicy -d throttle|jobs=8
#   nice -n 10 taskpolicy -d throttle|jobs=16' scripts/agent/validate-throttle.sh 3
#
# Needs .build/golangci-lint (`make golangci-lint`). No sudo required.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
# Scratch (probe output, iostat captures, cold caches) stays in the gitignored
# tmp/; the built probe follows the .build/ convention used for golangci-lint.
readonly TMP_DIR="${REPO_ROOT}/tmp"
readonly PROBE="${REPO_ROOT}/.build/uijitter"
readonly PROBE_SRC="${REPO_ROOT}/scripts/agent/uijitter.c"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
readonly REPORTS_DIR="${HOME}/Library/Logs/DiagnosticReports"

readonly TRIALS="${1:-1}"
# Which phase to measure. `lint` is the default because it is the one that
# actually saturates: golangci-lint fans out one worker per logical CPU and
# ignores GOMAXPROCS (scripts/agent/local-throttle.sh), and `make check` is dominated
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
#
# GAG_THROTTLE_CANDIDATES (newline-separated) replaces the list — see the header
# for the `prefix|jobs=N|holders=M` entry format.
CANDIDATES=(
	'nice -n 10 taskpolicy -d throttle' # current: scripts/agent/local-throttle.sh
	'taskpolicy -d throttle'            # aggressive: I/O demoted only, no nice
	'taskpolicy -c utility'             # the pre-Q441 clamp, for contrast
)
if [[ -n "${GAG_THROTTLE_CANDIDATES:-}" ]]; then
	IFS=$'\n' read -r -d '' -a CANDIDATES <<<"$GAG_THROTTLE_CANDIDATES" || true
fi
readonly CANDIDATES

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
		# Not `(( count++ ))`: post-increment evaluates to the *old* value, so
		# the first match returns 1 and, as the last command in the `&&` list,
		# aborts the function under errexit (Q733).
		[[ -e "$f" ]] && count=$(( count + 1 ))
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
# scripts/go/go-test.sh: -count=1 defeats the result cache, -trimpath matches what
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

# run_test runs the whole unit tier as one invocation, matching scripts/go/go-test.sh
# minus -race. This is the OTHER half of what `jobs` reaches: go-lint.sh spends it
# on `golangci-lint -j`, while go-test.sh and coverage.sh spend it on `go test -p`
# plus GOMAXPROCS, and a lint-only sweep cannot speak for either.
#
# GOCACHE is COLD per run, for the same reason run_lint busts it. A warm-cache
# version of this workload was tried first and is the third null this harness has
# produced: 40 s at every parallelism level from 4 to 24 and only 18-36% mean CPU,
# INVALID under MIN_SATURATION_PCT. With the build cache warm, `-count=1` defeats
# only the RESULT cache, so what is left is test execution — a few slow suites
# waiting on timers and envtest, which no amount of `-p` makes faster. Cold, the
# run is compile-bound, which is both what actually saturates and the regime that
# matters: a fresh worktree is exactly when the local gate is expensive.
run_test() {
	local jobs="$2" gocache
	local pfx=() patterns=() dir
	[[ -n "$1" ]] && read -r -a pfx <<<"$1"
	for dir in $(workspace_modules); do patterns+=("$dir/..."); done
	gocache="$(mktemp -d "${TMP_DIR}/gocache.XXXXXX")"
	(
		cd "$REPO_ROOT" || exit 1
		GOCACHE="$gocache" GOMAXPROCS="$jobs" "${pfx[@]}" go test -trimpath -count=1 \
			-timeout 20m -p "$jobs" "${patterns[@]}" >/dev/null 2>&1
	) || true
	rm -rf "$gocache"
}

# run_lint lints one heavy module under the candidate prefix, mirroring
# scripts/go/go-lint.sh's invocation but with BOTH caches cold.
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

# candidate_prefix prints the command prefix of a candidate entry: everything
# before the first `|`, with surrounding whitespace trimmed. An entry with no
# options is returned unchanged, which is why the default candidates need no
# special-casing.
candidate_prefix() {
	local prefix="${1%%|*}"
	prefix="${prefix#"${prefix%%[![:space:]]*}"}"
	printf '%s' "${prefix%"${prefix##*[![:space:]]}"}"
}

# candidate_opt ENTRY KEY DEFAULT prints the value of `|KEY=V` in a candidate
# entry, or DEFAULT when the key is absent.
#
# A non-numeric or zero value falls back to DEFAULT rather than propagating: it
# reaches `golangci-lint -j` and the concurrency loop, where a typo would either
# fail the run outright or — worse — silently measure a configuration nobody
# asked for and publish it as the derived default.
candidate_opt() {
	local entry="$1" key="$2" default="$3" value
	value="$(printf '%s' "$entry" | tr '|' '\n' |
		awk -F= -v k="$key" '$1 == k { print $2; exit }')"
	[[ "$value" =~ ^[0-9]+$ ]] && (( value >= 1 )) || value="$default"
	printf '%s' "$value"
}

# run_once dispatches to the selected phase for a single holder.
run_once() {
	case "$WORKLOAD" in
		lint) run_lint "$1" "$2" ;;
		test) run_test "$1" "$2" ;;
		race) run_race "$1" "$2" ;;
		*) printf 'unknown workload: %s\n' "$WORKLOAD" >&2; exit 2 ;;
	esac
}

# run_workload runs $3 (default 1) concurrent copies of the phase and prints the
# seconds until the LAST one finishes — what a sibling session actually waits.
#
# Concurrency is the only way to measure the `slots` semaphore: one holder can
# never show whether a second one is buying throughput or just dividing a fixed
# ceiling. Each holder gets its own GOCACHE (run_lint mktemps per call), so they
# contend for the machine exactly as separate worktrees do rather than sharing
# warm artifacts and flattering the result.
run_workload() {
	local holders="${3:-1}" i started ended
	local pids=()
	started="$(date +%s)"
	for (( i = 0; i < holders; i++ )); do
		run_once "$1" "$2" &
		pids+=("$!")
	done
	for i in "${pids[@]}"; do
		wait "$i" || true
	done
	ended="$(date +%s)"
	printf '%s' "$(( ended - started ))"
}

# measure runs one candidate entry once and prints a TSV row. $2 is the harness's
# auto-sized jobs, used only when the entry does not pin its own.
measure() {
	local entry="$1" trial="$3"
	local prefix jobs holders
	prefix="$(candidate_prefix "$entry")"
	jobs="$(candidate_opt "$entry" jobs "$2")"
	holders="$(candidate_opt "$entry" holders 1)"
	local ws_before ws_after sw_before sw_after wall jitter busy
	local io_out="${TMP_DIR}/iostat.out"

	ws_before="$(windowserver_reports)"
	sw_before="$(swapins)"

	"$PROBE" >"${TMP_DIR}/uijitter.out" 2>/dev/null &
	probe_pid="$!"
	iostat -c 100000 -w 1 >"$io_out" 2>/dev/null &
	iostat_pid="$!"

	wall="$(run_workload "$prefix" "$jobs" "$holders")"

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

	printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$entry" "$trial" "$wall" "$jitter" \
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
	jobs="$("${REPO_ROOT}/scripts/agent/local-throttle.sh" jobs)"
	[[ -n "$jobs" ]] || jobs="$(sysctl -n hw.physicalcpu)"

	if [[ "$WORKLOAD" == "lint" && ! -x "$GOLANGCI_LINT" ]]; then
		printf 'validate-throttle: %s missing — build it with: make golangci-lint\n' \
			"$GOLANGCI_LINT" >&2
		return 2
	fi

	printf '==> workload=%s%s trials=%s jobs=%s (default; per-candidate |jobs= wins)\n' \
		"$WORKLOAD" \
		"$([[ "$WORKLOAD" == "lint" ]] && printf ' (cold cache, %s)' "$LINT_MODULE"
			[[ "$WORKLOAD" == "test" ]] && printf ' (cold cache, all modules)')" \
		"$TRIALS" "$jobs"
	printf '==> idle jitter floor: %s\n' "$("$PROBE" 16.667 5)"

	# No warmup for the lint and test workloads: both deliberately start each run
	# from a cold GOCACHE, so there is no shared first-run cost for a warmup to
	# absorb. The race tier does need one, since its first run compiles the
	# instrumented deps, which would otherwise land entirely on whichever
	# candidate happened to be measured first.
	if [[ "$WORKLOAD" == "race" ]]; then
		printf '==> warming the build cache (unmeasured, several minutes)\n'
		run_workload '' "$jobs" >/dev/null
	fi

	printf '\n%-52s %5s %7s %5s %8s %8s %8s %7s %7s %s\n' \
		'CANDIDATE' 'TRIAL' 'WALL_S' 'CPU%' 'p50_ms' 'p99_ms' 'max_ms' '>50ms' '>250ms' 'SWAPIN|WS'
	printf '%s\n' '-------------------------------------------------------------------------------------------------------------------------------------'

	local trial cand row jit busy peak note
	local rows=()
	for (( trial = 1; trial <= TRIALS; trial++ )); do
		# Collect the whole trial before printing: validity is a property of the
		# trial (did ANY candidate load the machine?), not of an individual row.
		rows=()
		peak=0
		for cand in "${CANDIDATES[@]}"; do
			row="$(measure "$cand" "$jobs" "$trial")"
			rows+=("$row")
			busy="$(printf '%s' "$row" | cut -f6)"
			(( busy > peak )) && peak="$busy"
		done

		note=''
		(( peak < MIN_SATURATION_PCT )) && note='  <-- INVALID TRIAL: no candidate loaded the machine'

		for row in "${rows[@]}"; do
			jit="$(printf '%s' "$row" | cut -f4)"
			printf '%-52s %5s %7s %4s%% %8s %8s %8s %7s %7s %s%s\n' \
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
	printf 'holders=M runs M copies concurrently and WALL_S is until the LAST finishes:\n'
	printf 'compare M/WALL_S against the holders=1 row to see what a slot actually buys.\n'
}

# Run main only when executed directly, so validate-throttle-test.sh can source
# this file to exercise the pure parsers against recorded instrument output —
# the measurement paths are macOS-only, the text-to-number parsers are not.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
