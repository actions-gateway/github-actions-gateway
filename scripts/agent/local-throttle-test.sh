#!/usr/bin/env bash
#
# Unit tests for the pure sizing helpers in scripts/agent/local-throttle.sh:
# compute_slots (how many heavy runs may overlap, and the
# GAG_HEAVY_BUILD_SLOTS override), compute_workers (how many parallel-dispatch
# sessions a machine should host) and lock_file (slot 1 keeps the original
# filename so a pre-semaphore checkout still contends with us). These decide how
# much of the machine a `make check` may take while siblings are running, so they
# are asserted here rather than discovered when a desktop wedges. Runs under
# `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its functions; the BASH_SOURCE guard there
# keeps main() from running on source.
# shellcheck source=scripts/agent/local-throttle.sh
source "$REPO_ROOT/scripts/agent/local-throttle.sh"

fails=0

expect_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-34s -> %s\n' "$name" "$got"
	else
		printf 'FAIL %-34s want=[%s] got=[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# --- compute_slots ---------------------------------------------------------
# physical_cores is stubbed per case so the assertions hold on any dev machine
# and on CI, whatever the runner's core count is.

physical_cores() { printf '8'; }
expect_eq slots-many-cores 2 "$(unset GAG_HEAVY_BUILD_SLOTS; compute_slots)"
expect_eq slots-exactly-min 2 "$(physical_cores() { printf '4'; }; unset GAG_HEAVY_BUILD_SLOTS; compute_slots)"

# Below the minimum there is nothing to overlap: jobs is already 1-2, and a
# second holder would only thrash the core the UI is not using.
expect_eq slots-few-cores 1 "$(physical_cores() { printf '3'; }; unset GAG_HEAVY_BUILD_SLOTS; compute_slots)"
expect_eq slots-single-core 1 "$(physical_cores() { printf '1'; }; unset GAG_HEAVY_BUILD_SLOTS; compute_slots)"

# The override wins in both directions — 1 restores the old strict
# serialization, a larger value opts into more overlap.
expect_eq slots-override-serial 1 "$(GAG_HEAVY_BUILD_SLOTS=1 compute_slots)"
expect_eq slots-override-wide 5 "$(GAG_HEAVY_BUILD_SLOTS=5 compute_slots)"
# A bad override must not fail a build or produce a zero-slot semaphore that
# could never be acquired: fall back to the core-count default.
expect_eq slots-override-empty 2 "$(GAG_HEAVY_BUILD_SLOTS='' compute_slots)"
expect_eq slots-override-zero 2 "$(GAG_HEAVY_BUILD_SLOTS=0 compute_slots)"
expect_eq slots-override-junk 2 "$(GAG_HEAVY_BUILD_SLOTS=lots compute_slots)"
expect_eq slots-override-negative 2 "$(GAG_HEAVY_BUILD_SLOTS=-1 compute_slots)"

# --- compute_workers -------------------------------------------------------
# total_ram_mb and physical_cores are stubbed per case, so these assert the
# sizing arithmetic rather than whatever machine happens to run the suite.

# A wide machine is answered by the ceiling, not by its hardware: 128 GB leaves
# room for ~138 sessions, and past the ceiling the binding constraints are
# dispatcher review throughput and GitHub Actions concurrency, neither of which
# this script can see.
expect_eq workers-wide-machine 12 \
	"$(total_ram_mb() { printf '131072'; }; physical_cores() { printf '18'; }; unset GAG_DISPATCH_WORKERS; compute_workers)"

# Cores bind below the ceiling even when RAM is abundant.
expect_eq workers-core-bound 6 \
	"$(total_ram_mb() { printf '131072'; }; physical_cores() { printf '6'; }; unset GAG_DISPATCH_WORKERS; compute_workers)"

# RAM binds on a small machine: 16 GB is entirely spoken for by the desktop
# reserve and two gate holders, so one worker is all it can host.
expect_eq workers-ram-bound 1 \
	"$(total_ram_mb() { printf '16384'; }; physical_cores() { printf '8'; }; unset GAG_DISPATCH_WORKERS; compute_workers)"

# 32 GB clears the reserve plus both gate holders with ~10 sessions' headroom,
# so the 8-core term is what decides it.
expect_eq workers-midsize 8 \
	"$(total_ram_mb() { printf '32768'; }; physical_cores() { printf '8'; }; unset GAG_DISPATCH_WORKERS; compute_workers)"

# An unreadable RAM figure must size DOWN, never open the machine up.
expect_eq workers-unknown-ram 1 \
	"$(total_ram_mb() { printf '0'; }; physical_cores() { printf '18'; }; unset GAG_DISPATCH_WORKERS; compute_workers)"

# The override is the documented way past the ceiling — the constraints above it
# are not ones the machine can measure.
expect_eq workers-override-wide 20 \
	"$(total_ram_mb() { printf '131072'; }; physical_cores() { printf '18'; }; GAG_DISPATCH_WORKERS=20 compute_workers)"
expect_eq workers-override-serial 1 \
	"$(total_ram_mb() { printf '131072'; }; physical_cores() { printf '18'; }; GAG_DISPATCH_WORKERS=1 compute_workers)"
# A bad override falls back to the computed value rather than yielding zero
# workers, which would stall a dispatch outright.
expect_eq workers-override-empty 12 \
	"$(total_ram_mb() { printf '131072'; }; physical_cores() { printf '18'; }; GAG_DISPATCH_WORKERS='' compute_workers)"
expect_eq workers-override-zero 12 \
	"$(total_ram_mb() { printf '131072'; }; physical_cores() { printf '18'; }; GAG_DISPATCH_WORKERS=0 compute_workers)"
expect_eq workers-override-junk 12 \
	"$(total_ram_mb() { printf '131072'; }; physical_cores() { printf '18'; }; GAG_DISPATCH_WORKERS=lots compute_workers)"

# --- qos_prefix ------------------------------------------------------------
# os_kind is stubbed per case so both platforms' prefixes are asserted wherever
# this runs. These strings ARE the throttle: the wrong one either freezes the
# GUI or throws away most of the machine.

expect_eq prefix-darwin 'nice -n 10 taskpolicy -d throttle' \
	"$(os_kind() { printf 'darwin'; }; qos_prefix)"

# The regression this pins is a return to `taskpolicy -c utility` (or any other
# `-c` band). `-c` is a QoS CLAMP, not a nice level: on Apple Silicon it confines
# the whole build to a single CPU cluster — 21% of an M5 Max — and since it only
# ever ratchets QoS down there is no higher tier to select back. Q441 measured
# the split demotion 3.6x faster for +1.4 ms of p99 desktop latency, so a change
# back here is a 3.6x regression that no test would otherwise notice.
expect_eq prefix-darwin-no-qos-clamp '' \
	"$(os_kind() { printf 'darwin'; }; qos_prefix | grep -o -- '-c [a-z]*' || true)"
# Both demotions must survive: dropping `taskpolicy -d` loses the disk I/O
# demotion that `nice` cannot express on macOS, and that is the one the
# WindowServer watchdog actually needs.
expect_eq prefix-darwin-has-io-demotion 'taskpolicy -d throttle' \
	"$(os_kind() { printf 'darwin'; }; qos_prefix | grep -o 'taskpolicy -d throttle' || true)"

# Linux expresses the same CPU+I/O demotion through two separate tools, and
# ionice is not guaranteed to be installed — the CPU half must still apply.
expect_eq prefix-linux-with-ionice 'nice -n 19 ionice -c 3' \
	"$(os_kind() { printf 'linux'; }; has_ionice() { return 0; }; qos_prefix)"
expect_eq prefix-linux-without-ionice 'nice -n 19' \
	"$(os_kind() { printf 'linux'; }; has_ionice() { return 1; }; qos_prefix)"

# An unsupported OS gets no prefix at all, so the Makefile runs the command bare
# rather than through a wrapper that does not exist there.
expect_eq prefix-other '' "$(os_kind() { printf 'other'; }; qos_prefix)"

# --- lock_file -------------------------------------------------------------
# Slot 1 must keep the pre-semaphore filename: a worktree still running the old
# single-lock code opens exactly that path, so it contends with slot 1 here
# instead of running unbounded beside us.
lock1="$(lock_file)"
expect_eq lock-default-is-slot-1 "$lock1" "$(lock_file 1)"
expect_eq lock-slot-1-legacy-name local-heavy-build.lock "$(basename "$lock1")"
expect_eq lock-slot-2-name local-heavy-build.2.lock "$(basename "$(lock_file 2)")"
expect_eq lock-slots-share-a-dir "$(dirname "$lock1")" "$(dirname "$(lock_file 2)")"
# A junk index must resolve to a real lock rather than a path with an empty or
# garbage suffix that two runs could disagree on.
expect_eq lock-junk-index "$lock1" "$(lock_file junk)"
expect_eq lock-zero-index "$lock1" "$(lock_file 0)"

if (( fails > 0 )); then
	printf '\n%d local-throttle assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall local-throttle assertions passed\n'
