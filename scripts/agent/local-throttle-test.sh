#!/usr/bin/env bash
#
# Unit tests for the pure sizing helpers in scripts/agent/local-throttle.sh:
# compute_slots (how many heavy runs may overlap, and the
# GAG_HEAVY_BUILD_SLOTS override) and lock_file (slot 1 keeps the original
# filename so a pre-semaphore checkout still contends with us). These decide how
# much of the machine a `make check` may take while siblings are running, so they
# are asserted here rather than discovered when a desktop wedges. Runs under
# `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

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
