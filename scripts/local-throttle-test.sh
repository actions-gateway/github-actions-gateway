#!/usr/bin/env bash
#
# Unit tests for the pure sizing helpers in scripts/local-throttle.sh:
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
# shellcheck source=scripts/local-throttle.sh
source "$REPO_ROOT/scripts/local-throttle.sh"

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
