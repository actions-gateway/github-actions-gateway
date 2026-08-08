#!/usr/bin/env bash
#
# Unit tests for the instrument parsers in scripts/agent/validate-throttle.sh (Q447):
# mean_busy_pct (iostat -> mean CPU busy%), field (the uijitter report line ->
# one latency statistic), parse_swapins (vm_stat -> the swapin counter) and
# windowserver_reports (crash-report count). Every fixture below is recorded
# output from the real instrument on an Apple Silicon Mac, not a guess at its
# shape.
#
# These four numbers ARE the throttle decision: mean_busy_pct alone decides
# whether a trial is VALID, and a trial marked INVALID discards a real result
# while a trial wrongly marked VALID publishes a null one. The measurement paths
# need macOS (and in the probe's case a compiled binary); the text-to-number
# parsers need neither, so they are asserted here on every platform.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its parsers; the BASH_SOURCE guard there keeps
# main() from running (and macOS from being required) on source.
# shellcheck source=scripts/agent/validate-throttle.sh
source "$REPO_ROOT/scripts/agent/validate-throttle.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/validate-throttle-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# expect_eq NAME WANT GOT — assert an exact string match.
expect_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-42s -> %s\n' "$name" "${got:-<empty>}"
	else
		printf 'FAIL %-42s want=[%s] got=[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# --- mean_busy_pct: iostat -> mean CPU busy% ---------------------------------
#
# iostat's trailing columns are "us sy id 1m 5m 15m", so idle is the 4th field
# from the end however many disks the machine reports. Two properties of the
# real stream drive every case below:
#
#   1. The FIRST data row is iostat's since-boot average, not an interval
#      sample, and on a machine that has been up a while it reads near-idle. It
#      must be dropped or it drags the mean toward "the workload never loaded
#      the machine" — the exact false INVALID this script exists to avoid.
#   2. iostat REPRINTS its two-line header every 20 data rows. A real trial
#      samples for ~30 s at -w 1, so every capture worth reading contains at
#      least one mid-stream header block.

# Verbatim `iostat -c 25 -w 1` output, sample rows elided to keep the fixture
# readable — the mid-capture header block at the same offset iostat emits it,
# and the since-boot first row, are exactly as recorded.
cat >"$FIXTURE_DIR/iostat-real.txt" <<'EOF'
              disk0               disk4               disk5       cpu    load average
    KB/t  tps  MB/s     KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id   1m   5m   15m
   43.26  291 12.30   463.77    0  0.02   403.41    0  0.02   8  4 89  12.29 7.08 5.20
   45.07 7972 350.89     0.00    0  0.00     0.00    0  0.00  75 25  0  12.29 7.08 5.20
   68.86 4952 332.97     0.00    0  0.00     0.00    0  0.00  72 22  5  12.29 7.08 5.20
   30.10 8441 248.16     0.00    0  0.00     0.00    0  0.00  54 24 21  12.27 7.17 5.24
              disk0               disk4               disk5       cpu    load average
    KB/t  tps  MB/s     KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id   1m   5m   15m
    4.50 8490 37.34     0.00    0  0.00     0.00    0  0.00  48 12 41  11.10 7.16 5.27
    4.56 5366 23.91     0.00    0  0.00     0.00    0  0.00  45  7 49  11.10 7.16 5.27
EOF
# Interval samples are id=0,5,21,41,49 -> busy 100,95,79,59,51 -> 384/5 = 76.8.
# Counting the skipped since-boot row (id=89) instead gives 395/6 = 65.8 -> 66,
# so this assertion is what pins the row-1 skip, and 66 is the regression.
expect_eq mean_busy/real-capture 77 "$(mean_busy_pct "$FIXTURE_DIR/iostat-real.txt")"

# Both header lines must be rejected. The disk-name line puts "disk5" in the
# idle column and the units line puts "id" there; neither is numeric.
cat >"$FIXTURE_DIR/iostat-header-only.txt" <<'EOF'
              disk0               disk4               disk5       cpu    load average
    KB/t  tps  MB/s     KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id   1m   5m   15m
EOF
expect_eq mean_busy/header-only 0 "$(mean_busy_pct "$FIXTURE_DIR/iostat-header-only.txt")"

# Absent samples: the capture holds only the since-boot row, so after the row-1
# skip there is nothing to average. Must be 0 (-> INVALID trial), never a
# divide-by-zero and never a number inherited from the since-boot average.
cat >"$FIXTURE_DIR/iostat-sinceboot-only.txt" <<'EOF'
              disk0               disk4               disk5       cpu    load average
    KB/t  tps  MB/s     KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id   1m   5m   15m
   43.26  291 12.30   463.77    0  0.02   403.41    0  0.02   8  4 89  12.29 7.08 5.20
EOF
expect_eq mean_busy/since-boot-row-only 0 \
	"$(mean_busy_pct "$FIXTURE_DIR/iostat-sinceboot-only.txt")"

# A capture that never opened (iostat failed to start) reads 0, not an error.
: >"$FIXTURE_DIR/iostat-empty.txt"
expect_eq mean_busy/empty-capture 0 "$(mean_busy_pct "$FIXTURE_DIR/iostat-empty.txt")"

# Blank and short lines must be SKIPPED, not fatal. awk resolves $(NF-3) on a
# blank line to field -3, which is a hard error ("trying to access out of range
# field -3") rather than an empty string: before the NF guard, one blank line in
# the capture killed mean_busy_pct, and `set -e` then tore down the harness
# mid-trial, discarding every candidate already measured. The trailing partial
# row is what a capture truncated by the kill -TERM at the end of a trial looks
# like. Same interval samples as the real fixture, so the mean must not move.
printf '%s\n' \
	'              disk0       cpu    load average' \
	'    KB/t  tps  MB/s  us sy id   1m   5m   15m' \
	'   43.26  291 12.30   8  4 89  12.29 7.08 5.20' \
	'' \
	'   45.07 7972 350.89  75 25  0  12.29 7.08 5.20' \
	'   68.86 4952 332.97  72 22  5  12.29 7.08 5.20' \
	'   30.10 8441 248.16  54 24 21  12.27 7.17 5.24' \
	'' \
	'    4.50 8490 37.34  48 12 41  11.10 7.16 5.27' \
	'    4.56 5366 23.91  45  7 49  11.10 7.16 5.27' \
	'   4.51 47' >"$FIXTURE_DIR/iostat-ragged.txt"
expect_eq mean_busy/blank-and-short-lines 77 \
	"$(mean_busy_pct "$FIXTURE_DIR/iostat-ragged.txt")"

# Locale-ish formatting: a locale that renders the decimal separator as a comma
# changes how the disk and load-average columns LOOK but not how many fields
# there are, so indexing from the end still lands on idle. This is why the
# parser counts backwards instead of pinning an absolute column.
cat >"$FIXTURE_DIR/iostat-comma-decimals.txt" <<'EOF'
              disk0       cpu    load average
    KB/t  tps  MB/s  us sy id   1m   5m   15m
   43,26  291 12,30   8  4 89  12,29 7,08 5,20
   45,07 7972 350,89  75 25  0  12,29 7,08 5,20
   68,86 4952 332,97  72 22  4  12,29 7,08 5,20
EOF
# Interval samples id=0,4 -> busy 100,96 -> 196/2 = 98.
expect_eq mean_busy/comma-decimal-locale 98 \
	"$(mean_busy_pct "$FIXTURE_DIR/iostat-comma-decimals.txt")"

# An idle machine must read near 0 rather than being mistaken for absent data:
# 0 and "the workload did nothing" are the same INVALID verdict, but only one of
# them is a measurement, and the report prints the number.
cat >"$FIXTURE_DIR/iostat-idle.txt" <<'EOF'
              disk0       cpu    load average
    KB/t  tps  MB/s  us sy id   1m   5m   15m
   43.26  291 12.30   8  4 89  1.29 1.08 1.20
    0.00    0  0.00   0  1 99  1.29 1.08 1.20
    0.00    0  0.00   1  2 97  1.29 1.08 1.20
EOF
# busy 1,3 -> 2. Deliberately not a .5 boundary: half-way rounding is not worth
# pinning across awk implementations, and no real reading depends on it.
expect_eq mean_busy/idle-machine 2 "$(mean_busy_pct "$FIXTURE_DIR/iostat-idle.txt")"

# --- field: the uijitter report line -> one statistic ------------------------
#
# Recorded verbatim from `.build/uijitter 16.667 3` (the idle-floor invocation
# main() prints). Six of these values become table columns; a key that silently
# returns empty prints a blank cell where a latency number belongs.
readonly JITTER='samples=180 elapsed_s=3.0 p50_ms=2.74 p95_ms=4.02 p99_ms=4.12 max_ms=4.16 over_50ms=0 over_250ms=0'

expect_eq field/p50 2.74 "$(field "$JITTER" p50_ms)"
expect_eq field/p99 4.12 "$(field "$JITTER" p99_ms)"
expect_eq field/max 4.16 "$(field "$JITTER" max_ms)"
expect_eq field/first-key 180 "$(field "$JITTER" samples)"
expect_eq field/last-key 0 "$(field "$JITTER" over_250ms)"

# over_50ms and over_250ms are the two threshold-count columns and one key is a
# substring of the other. Matching must be on the whole key, so a prefix or
# substring query returns nothing rather than the wrong column's count.
readonly JITTER_BUSY='samples=1800 elapsed_s=30.0 p50_ms=3.11 p95_ms=48.60 p99_ms=214.77 max_ms=983.02 over_50ms=94 over_250ms=11'
expect_eq field/over-50-not-over-250 94 "$(field "$JITTER_BUSY" over_50ms)"
expect_eq field/over-250-distinct 11 "$(field "$JITTER_BUSY" over_250ms)"
expect_eq field/substring-key-no-match '' "$(field "$JITTER_BUSY" 50ms)"
expect_eq field/prefix-key-no-match '' "$(field "$JITTER_BUSY" p50)"

# A missing key yields empty, which is how a probe from a future/older build
# that dropped a statistic surfaces: a blank column, not a wrong number.
expect_eq field/absent-key '' "$(field "$JITTER" p90_ms)"
# The probe died before printing: every column blanks out together.
expect_eq field/empty-report '' "$(field '' p50_ms)"
# Runs of spaces (column-aligned output) must not be read as empty keys.
expect_eq field/multiple-spaces 4.12 "$(field 'p50_ms=2.74    p99_ms=4.12' p99_ms)"

# --- parse_swapins: vm_stat -> the cumulative swapin counter -----------------
#
# The delta across a trial is the memory-pressure signal in the SWAPIN|WS
# column. Recorded verbatim from `vm_stat` (leading and trailing lines elided).

cat >"$FIXTURE_DIR/vm_stat-real.txt" <<'EOF'
Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                  3593718.
Pages active:                                2049738.
Pages inactive:                              1138663.
Pages speculative:                           1230500.
Pages throttled:                                   0.
Pages wired down:                             246423.
Pageins:                                     8453596.
Pageouts:                                       1834.
Swapins:                                           0.
Swapouts:                                          0.
EOF
expect_eq swapins/real-capture 0 "$(parse_swapins <"$FIXTURE_DIR/vm_stat-real.txt")"

# Same capture with a machine that HAS swapped. "Swapouts" must not be picked up
# in place of "Swapins" — they are adjacent lines and one is a substring match
# away from the other.
cat >"$FIXTURE_DIR/vm_stat-swapped.txt" <<'EOF'
Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                  3593718.
Swapins:                                      412905.
Swapouts:                                    9990001.
EOF
expect_eq swapins/nonzero-not-swapouts 412905 \
	"$(parse_swapins <"$FIXTURE_DIR/vm_stat-swapped.txt")"

# The trailing "." must be stripped, or the delta arithmetic sees a non-number.
expect_eq swapins/strips-trailing-period 7 \
	"$(printf 'Swapins:%40s7.\n' '' | parse_swapins)"

# Absent counter (vm_stat failed, or a kernel that stopped reporting it): print
# 0 rather than nothing. The caller subtracts two readings, and an empty operand
# makes that subtraction silently produce a delta of 0 from garbage inputs.
expect_eq swapins/absent-counter 0 \
	"$(printf 'Pages free: 100.\nPages active: 200.\n' | parse_swapins)"
expect_eq swapins/empty-input 0 "$(printf '' | parse_swapins)"
# Exactly one line out, whatever came in — the caller substitutes the result
# straight into `$(( after - before ))`, which a second line would break.
expect_eq swapins/one-line-out 1 \
	"$(parse_swapins <"$FIXTURE_DIR/vm_stat-swapped.txt" | wc -l | tr -d ' ')"

# --- windowserver_reports: crash-report count --------------------------------
#
# Any increase across a trial is a hard fail — a WindowServer watchdog report is
# the desktop freeze the throttle exists to prevent. Under-counting here turns
# the one unambiguous failure signal into a silent pass.

REPORTS_FIXTURE="$FIXTURE_DIR/reports"
mkdir -p "$REPORTS_FIXTURE"
expect_eq ws-reports/empty-dir 0 "$(windowserver_reports "$REPORTS_FIXTURE")"

# A Mac that has never crashed WindowServer has no DiagnosticReports directory
# at all; that is 0 reports, not an error that aborts the trial.
expect_eq ws-reports/missing-dir 0 "$(windowserver_reports "$FIXTURE_DIR/nope")"

# Real report filenames. Case varies across macOS versions, which is why the
# glob is nocaseglob — matching only "WindowServer" would miss the lowercase
# spelling and report a clean trial through an actual freeze.
: >"$REPORTS_FIXTURE/WindowServer-2026-07-26-104233.ips"
: >"$REPORTS_FIXTURE/windowserver_watchdog-2026-07-26-104512.ips"
: >"$REPORTS_FIXTURE/kernel-2026-07-26-104233.panic"
expect_eq ws-reports/counts-both-cases 2 "$(windowserver_reports "$REPORTS_FIXTURE")"

# Unrelated reports alone must not count.
UNRELATED_FIXTURE="$FIXTURE_DIR/unrelated"
mkdir -p "$UNRELATED_FIXTURE"
: >"$UNRELATED_FIXTURE/kernel-2026-07-26-104233.panic"
: >"$UNRELATED_FIXTURE/Dock-2026-07-26-104233.ips"
expect_eq ws-reports/ignores-other-reports 0 "$(windowserver_reports "$UNRELATED_FIXTURE")"

# --- candidate_prefix / candidate_opt: the candidate-entry format -------------
#
# A candidate is `prefix` optionally followed by `|jobs=N` and/or `|holders=M`.
# These two parsers decide what command actually runs and how many copies of it,
# so a misread here does not fail loudly — it measures a configuration nobody
# asked for and publishes the number as the derived default.

# The default candidates carry no options and must survive unchanged, including
# the embedded `-n 10` that makes the prefix look like it has fields of its own.
expect_eq candidate/prefix-bare 'nice -n 10 taskpolicy -d throttle' \
	"$(candidate_prefix 'nice -n 10 taskpolicy -d throttle')"
expect_eq candidate/prefix-strips-options 'nice -n 10 taskpolicy -d throttle' \
	"$(candidate_prefix 'nice -n 10 taskpolicy -d throttle|jobs=8|holders=2')"
# Whitespace around the separator is trimmed: `run_workload` splits the prefix on
# whitespace into a command array, where a trailing blank becomes an empty argv
# entry and the exec fails.
expect_eq candidate/prefix-trims-space 'taskpolicy -d throttle' \
	"$(candidate_prefix '  taskpolicy -d throttle |jobs=8')"
# The unthrottled row is a legitimate candidate and must stay empty, not become
# a literal that the shell would try to exec.
expect_eq candidate/prefix-empty '' "$(candidate_prefix '')"
expect_eq candidate/prefix-empty-with-opts '' "$(candidate_prefix '|jobs=4')"

expect_eq candidate/opt-jobs 8 "$(candidate_opt 'p|jobs=8' jobs 16)"
expect_eq candidate/opt-holders 3 "$(candidate_opt 'p|jobs=8|holders=3' holders 1)"
expect_eq candidate/opt-order-independent 8 "$(candidate_opt 'p|holders=3|jobs=8' jobs 16)"
# An absent key takes the caller's default — that is how an entry inherits the
# jobs value scripts/agent/local-throttle.sh sizes.
expect_eq candidate/opt-absent 16 "$(candidate_opt 'p|holders=2' jobs 16)"
expect_eq candidate/opt-none 1 "$(candidate_opt 'taskpolicy -d throttle' holders 1)"
# `jobs` must not answer a `holders` query: the keys sit in the same entry and
# swapping them silently runs N concurrent copies at the wrong parallelism.
expect_eq candidate/opt-distinct-keys 1 "$(candidate_opt 'p|jobs=24' holders 1)"
# Junk and zero fall back to the default rather than reaching `golangci-lint -j`
# (which would fail the run) or the holder loop (where 0 measures nothing at all
# and reports it as a wall time).
expect_eq candidate/opt-junk 16 "$(candidate_opt 'p|jobs=lots' jobs 16)"
expect_eq candidate/opt-zero 1 "$(candidate_opt 'p|holders=0' holders 1)"
expect_eq candidate/opt-negative 16 "$(candidate_opt 'p|jobs=-4' jobs 16)"
expect_eq candidate/opt-empty-value 16 "$(candidate_opt 'p|jobs=' jobs 16)"

# --- summary -----------------------------------------------------------------

if (( fails > 0 )); then
	printf '\n%s assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nvalidate-throttle parsers: all assertions passed\n'
