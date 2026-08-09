#!/usr/bin/env bash
#
# Unit tests for the parsers in scripts/agent/qos-cluster-probe.sh (Q447): summarize
# (a powermetrics capture -> per-cluster active residency and effective compute
# in GHz-cores) and pct_of_max (one candidate's GHz-cores as a share of the
# unthrottled ceiling).
#
# GHz-cores is the whole output of that probe — it is the number that said
# `taskpolicy -c utility` confines a build to ~21% of the machine while
# `taskpolicy -d throttle` reaches ~96%, which is the evidence the throttle
# prefix is chosen on. Collecting a capture needs macOS AND root; turning the
# capture into the number needs neither, so the parsers are asserted here on
# every platform.
#
# Because a capture cannot be taken without root, the fixtures below are built
# from powermetrics' OWN printf format strings rather than transcribed by hand.
# Read them back out of the shipped binary with:
#
#   strings -a /usr/bin/powermetrics | grep -E 'Cluster|CPU %u|active (freq|resid)'
#
# which is where each line shape here comes from:
#
#   %s-Cluster / %s%u-Cluster          -> "E-Cluster", "P0-Cluster", "P1-Cluster"
#   %s %s active frequency: %0.0f MHz  -> "E-Cluster HW active frequency: 1000 MHz"
#   %s %s active residency: %6.2f%% (  -> "E-Cluster HW active residency:  50.00% ("
#   CPU %u frequency: %0.0f MHz        -> "CPU 0 frequency: 1000 MHz"
#   CPU %u active residency: %6.2f%% ( -> "CPU 0 active residency:  40.00% ("
#
# The %6.2f width is why "100.00%" carries one leading space and "50.00%" two;
# awk splits on whitespace runs, so the parser is indifferent, but keeping the
# real spacing means these fixtures stay diffable against a genuine capture.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its parsers; the BASH_SOURCE guard there keeps
# main() from running (and macOS, root, and a spin load from being required).
# shellcheck source=scripts/agent/qos-cluster-probe.sh
source "$REPO_ROOT/scripts/agent/qos-cluster-probe.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/qos-cluster-probe-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# expect_eq NAME WANT GOT — assert an exact string match. summarize emits a TSV
# line, so tabs are shown as <TAB> to keep a failure readable.
expect_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-42s -> %s\n' "$name" "${got//$'\t'/<TAB>}"
	else
		printf 'FAIL %-42s want=[%s] got=[%s]\n' "$name" \
			"${want//$'\t'/<TAB>}" "${got//$'\t'/<TAB>}" >&2
		fails=$((fails + 1))
	fi
}

# --- summarize: powermetrics -> residency + GHz-cores ------------------------
#
# The parser pairs three line kinds, and every case below is about what happens
# when that pairing is incomplete:
#
#   "<name>-Cluster HW active frequency: <MHz> MHz"   -> $5 is the clock
#   "<name>-Cluster HW active residency:  <pct>% ..." -> $5 is the residency
#   "CPU <n> frequency: ..."                          -> $2 counts cores
#
# GHz-cores = sum over clusters of (residency/100 x cores x MHz/1000), so a
# cluster that is registered but missing any ONE of the three contributes zero
# to a total that still prints as a confident number.

# Two 1-second samples over an E cluster (4 cores) and a P cluster (2 cores).
# E: freq (1000+1200)/2 = 1100 MHz, residency (50+60)/2 = 55%
#    -> 0.55 x 4 x 1.100 = 2.42
# P0: freq 3000 MHz, residency 100% -> 1.00 x 2 x 3.000 = 6.00
#    -> total 8.42 -> 8.4
#
# Every non-cluster line powermetrics interleaves is present, because each one
# is a chance to over-count: "GPU HW active frequency:" is one word away from
# the cluster pattern, "E-Cluster idle residency:" is the complement of the line
# being summed, and "CPU 0 active residency:" shares its prefix with the line
# that counts cores. None of them may register a cluster or a core.
cat >"$FIXTURE_DIR/two-samples.txt" <<'EOF'
*** Sampled system activity (1000.13ms elapsed) ***

**** Processor usage ****

E-Cluster HW active frequency: 1000 MHz
E-Cluster HW active residency:  50.00% (600 MHz:  20% 1000 MHz:  30%)
E-Cluster idle residency:  50.00%
CPU 0 frequency: 1000 MHz
CPU 0 active residency:  50.00% (600 MHz:  20% 1000 MHz:  30%)
CPU 1 frequency: 1000 MHz
CPU 1 active residency:  50.00% (600 MHz:  20% 1000 MHz:  30%)
CPU 2 frequency: 1000 MHz
CPU 2 active residency:  50.00% (600 MHz:  20% 1000 MHz:  30%)
CPU 3 frequency: 1000 MHz
CPU 3 active residency:  50.00% (600 MHz:  20% 1000 MHz:  30%)

P0-Cluster HW active frequency: 3000 MHz
P0-Cluster HW active residency: 100.00% (3000 MHz: 100%)
P0-Cluster idle residency:   0.00%
CPU 4 frequency: 3000 MHz
CPU 4 active residency: 100.00% (3000 MHz: 100%)
CPU 5 frequency: 3000 MHz
CPU 5 active residency: 100.00% (3000 MHz: 100%)

GPU HW active frequency: 400 MHz
GPU HW active residency:  12.00% (400 MHz:  12%)
GPU idle residency:  88.00%

*** Sampled system activity (1000.09ms elapsed) ***

**** Processor usage ****

E-Cluster HW active frequency: 1200 MHz
E-Cluster HW active residency:  60.00% (600 MHz:  10% 1200 MHz:  50%)
E-Cluster idle residency:  40.00%
CPU 0 frequency: 1200 MHz
CPU 0 active residency:  60.00% (600 MHz:  10% 1200 MHz:  50%)
CPU 1 frequency: 1200 MHz
CPU 2 frequency: 1200 MHz
CPU 3 frequency: 1200 MHz

P0-Cluster HW active frequency: 3000 MHz
P0-Cluster HW active residency: 100.00% (3000 MHz: 100%)
CPU 4 frequency: 3000 MHz
CPU 5 frequency: 3000 MHz

GPU HW active frequency: 400 MHz
GPU HW active residency:  12.00% (400 MHz:  12%)
EOF
# The GPU rows must contribute nothing: "GPU HW active frequency:" differs from
# a cluster line only by the "-Cluster" suffix the pattern requires, and a GPU
# counted as a third cluster would inflate the ceiling every candidate is
# measured against.
expect_eq summarize/averages-samples "$(printf 'E=55%% P0=100%% \t8.4')" \
	"$(summarize "$FIXTURE_DIR/two-samples.txt")"

# Cores are DISTINCT CPU indices, not CPU lines: powermetrics reprints every
# core every sample, so counting lines would multiply the machine's width by the
# sample count and report a ceiling several times the real one. Three samples,
# two cores, 100% at 1000 MHz -> 1.00 x 2 x 1.000 = 2.0 (not 6.0).
cat >"$FIXTURE_DIR/repeated-cores.txt" <<'EOF'
E-Cluster HW active frequency: 1000 MHz
E-Cluster HW active residency: 100.00%
CPU 0 frequency: 1000 MHz
CPU 1 frequency: 1000 MHz
E-Cluster HW active frequency: 1000 MHz
E-Cluster HW active residency: 100.00%
CPU 0 frequency: 1000 MHz
CPU 1 frequency: 1000 MHz
E-Cluster HW active frequency: 1000 MHz
E-Cluster HW active residency: 100.00%
CPU 0 frequency: 1000 MHz
CPU 1 frequency: 1000 MHz
EOF
expect_eq summarize/dedupes-cores-across-samples "$(printf 'E=100%% \t2.0')" \
	"$(summarize "$FIXTURE_DIR/repeated-cores.txt")"

# Cluster report order is first appearance, tracked by hand because macOS ships
# BWK awk with no asorti(). Without it the columns reorder between candidates
# and the sweep table compares different clusters row to row.
cat >"$FIXTURE_DIR/order.txt" <<'EOF'
P1-Cluster HW active frequency: 3000 MHz
P1-Cluster HW active residency: 100.00%
CPU 8 frequency: 3000 MHz
P0-Cluster HW active frequency: 3000 MHz
P0-Cluster HW active residency: 100.00%
CPU 4 frequency: 3000 MHz
E-Cluster HW active frequency: 1000 MHz
E-Cluster HW active residency: 100.00%
CPU 0 frequency: 1000 MHz
EOF
expect_eq summarize/first-appearance-order "$(printf 'P1=100%% P0=100%% E=100%% \t7.0')" \
	"$(summarize "$FIXTURE_DIR/order.txt")"

# A capture truncated mid-sample (powermetrics interrupted, or the `tee` cut
# short) leaves a cluster registered from its frequency line with no residency
# line to pair it with. Dividing by that zero count is FATAL in awk, which would
# abort the sweep and discard every candidate already measured; report the
# missing half as 0 instead. Only the intact P0 contributes: 1.00 x 1 x 3.000.
cat >"$FIXTURE_DIR/truncated.txt" <<'EOF'
P0-Cluster HW active frequency: 3000 MHz
P0-Cluster HW active residency: 100.00%
CPU 4 frequency: 3000 MHz
E-Cluster HW active frequency: 1000 MHz
EOF
expect_eq summarize/truncated-missing-residency "$(printf 'P0=100%% E=0%% \t3.0')" \
	"$(summarize "$FIXTURE_DIR/truncated.txt")"

# The mirror case: residency arrived, the frequency line did not.
cat >"$FIXTURE_DIR/no-frequency.txt" <<'EOF'
E-Cluster HW active residency:  75.00%
CPU 0 frequency: 1000 MHz
EOF
expect_eq summarize/missing-frequency "$(printf 'E=75%% \t0.0')" \
	"$(summarize "$FIXTURE_DIR/no-frequency.txt")"

# No per-CPU lines at all: residency and clock are known but the core count is
# not, so GHz-cores collapses to 0.0 for every candidate. This is the failure
# worth recognising in the field — a sweep whose GHz-cores column is 0.0 top to
# bottom has measured nothing, and pct_of_max then divides by that zero base.
cat >"$FIXTURE_DIR/no-cpu-lines.txt" <<'EOF'
E-Cluster HW active frequency: 1000 MHz
E-Cluster HW active residency: 100.00%
P0-Cluster HW active frequency: 3000 MHz
P0-Cluster HW active residency: 100.00%
EOF
expect_eq summarize/no-cpu-lines-yields-zero "$(printf 'E=100%% P0=100%% \t0.0')" \
	"$(summarize "$FIXTURE_DIR/no-cpu-lines.txt")"

# A CPU line before any cluster header has no cluster to belong to and must be
# dropped rather than attributed to whichever cluster appears next.
cat >"$FIXTURE_DIR/leading-cpu.txt" <<'EOF'
CPU 0 frequency: 1000 MHz
CPU 1 frequency: 1000 MHz
E-Cluster HW active frequency: 1000 MHz
E-Cluster HW active residency: 100.00%
CPU 0 frequency: 1000 MHz
EOF
expect_eq summarize/ignores-cpu-before-cluster "$(printf 'E=100%% \t1.0')" \
	"$(summarize "$FIXTURE_DIR/leading-cpu.txt")"

# GPU and ANE blocks use the same "<name> HW active frequency/residency" shape
# as a CPU cluster and differ only in lacking the "-Cluster" suffix. Isolated
# here so the exclusion is asserted on its own rather than only as a side effect
# of the full-sample fixture: counting either would add a phantom cluster to the
# unthrottled baseline and shrink every candidate's share of it.
cat >"$FIXTURE_DIR/gpu-only.txt" <<'EOF'
GPU HW active frequency: 400 MHz
GPU HW active residency:  12.00% (400 MHz:  12%)
GPU idle residency:  88.00%
ANE 0 SW frequency: 0 MHz
ANE 0 SW active residency:   0.00%
EOF
expect_eq summarize/ignores-gpu-and-ane "$(printf '\t0.0')" \
	"$(summarize "$FIXTURE_DIR/gpu-only.txt")"

# A capture that never opened (powermetrics refused for lack of root) must read
# as an empty measurement, not an error and not a fabricated ceiling.
: >"$FIXTURE_DIR/empty.txt"
expect_eq summarize/empty-capture "$(printf '\t0.0')" \
	"$(summarize "$FIXTURE_DIR/empty.txt")"

# Non-integer residency is the normal case and the trailing % must be stripped
# before arithmetic, or the whole cluster silently reads 0.
cat >"$FIXTURE_DIR/fractional.txt" <<'EOF'
E-Cluster HW active frequency: 1024 MHz
E-Cluster HW active residency:  45.31% (600 MHz: 12.5%)
CPU 0 frequency: 1024 MHz
CPU 1 frequency: 1024 MHz
CPU 2 frequency: 1024 MHz
CPU 3 frequency: 1024 MHz
EOF
# 0.4531 x 4 x 1.024 = 1.856 -> 1.9, and residency prints rounded to 45%.
expect_eq summarize/fractional-residency "$(printf 'E=45%% \t1.9')" \
	"$(summarize "$FIXTURE_DIR/fractional.txt")"

# --- pct_of_max: GHz-cores as a share of the unthrottled ceiling -------------
#
# The sweep's first row is the unthrottled baseline every later row divides by,
# so a baseline that measured nothing must not take the whole table with it.

expect_eq pct_of_max/half 50 "$(pct_of_max 5.0 10.0)"
expect_eq pct_of_max/identity 100 "$(pct_of_max 10.0 10.0)"
# The real finding this probe produced: utility reaches ~21% of the ceiling.
expect_eq pct_of_max/utility-clamp 21 "$(pct_of_max 12.6 60.0)"
expect_eq pct_of_max/rounds-to-integer 96 "$(pct_of_max 57.5 60.0)"
# A candidate that beats the baseline is reported above 100 rather than clamped:
# the sweep is measuring, not scoring, and >100% is real information (thermal
# drift, or a baseline row that was itself contended).
expect_eq pct_of_max/above-baseline 110 "$(pct_of_max 11.0 10.0)"
# Zero and absent bases: 0, never a divide-by-zero that kills the sweep after
# the measurements are already spent.
expect_eq pct_of_max/zero-base 0 "$(pct_of_max 5.0 0.0)"
expect_eq pct_of_max/empty-base 0 "$(pct_of_max 5.0 '')"
expect_eq pct_of_max/zero-over-zero 0 "$(pct_of_max 0.0 0.0)"

# --- summary -----------------------------------------------------------------

if (( fails > 0 )); then
	printf '\n%s assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nqos-cluster-probe parsers: all assertions passed\n'
