#!/usr/bin/env bash
#
# local-throttle.sh — emit resource-throttle settings for heavy local builds.
#
# A full `make check` runs golangci-lint per module (each fanning out to one
# worker per logical CPU — golangci-lint ignores GOMAXPROCS) plus `go test`
# across every module. On a small machine that saturates every core and starves
# the desktop:
#   * macOS: the WindowServer compositor misses its kernel watchdog deadline and
#     gets restarted — the entire GUI freezes (visible as
#     `WindowServer ... userspace_watchdog_timeout` in the crash reports).
#   * Linux/WSL desktops: no watchdog kill, but the session goes sluggish —
#     input lag and compositor stutter while the build runs.
#
# To keep the machine usable, an interactive GUI dev shell throttles the heavy
# phases two ways:
#   1. Demote both the disk I/O and the CPU priority of the build below the
#      desktop, so foreground apps (the compositor included) preempt it. This is
#      the root-cause fix. Why I/O and not just CPU: an unthrottled build already
#      runs at a *lower* QoS than WindowServer yet still triggers the watchdog,
#      so CPU priority alone is not the fix — the I/O demotion is load-bearing.
#        - macOS: `nice -n 10 taskpolicy -d throttle`. `-d throttle` sets the
#          disk I/O policy alone (taskpolicy is the only macOS way to express
#          that — there is no ionice); `nice -n 10` supplies the CPU-priority
#          demotion separately.
#        - Linux: `nice -n 19` (lowest CPU priority), plus `ionice -c 3` (idle
#          I/O class) when ionice is installed — the same CPU+I/O demotion, via
#          Linux's separate knobs.
#   2. Cap parallelism to (physical cores - 2), leaving cores for the UI. This
#      is the only lever that reaches golangci-lint, which takes `-j` but reads
#      no GOMAXPROCS/GOFLAGS env.
#
# The macOS prefix was `taskpolicy -c utility` until Q441. That is a QoS *clamp*,
# and it did far more than deprioritize: it confined the whole build to a single
# performance cluster — 21% of an M5 Max's compute on synthetic load, 37-39% mean
# CPU on a real cold-cache lint — with no higher tier selectable, since `-c` only
# ever ratchets QoS down. Splitting the two demotions apart (`-d throttle` for
# I/O, `nice` for CPU) returns the idle clusters: measured 3.6x faster on the
# cold-cache lint that dominates `make check`, for +1.4 ms of p99 desktop
# scheduling latency, with zero stutter events past 50 ms, zero swapins and zero
# WindowServer reports across nine runs. The variant keeping a CPU demotion was
# both faster and lower-jitter than bare `-d throttle`, so there was no
# speed-versus-safety trade to adjudicate. Evidence and method:
# docs/plan/archive/local-gate-throughput.md; instruments: scripts/qos-cluster-probe.sh
# (compute ceiling) and scripts/validate-throttle.sh (desktop cost).
#
# Throttling is auto-detected and applies ONLY to an interactive, GUI-bearing
# dev machine that is not CI:
#   * the CI env var must be unset (GitHub Actions et al. set it), and
#   * macOS — always (Macs have a GUI worth protecting), or
#   * Linux — only when a graphical session is present (DISPLAY or
#     WAYLAND_DISPLAY set). Headless servers, plain SSH sessions, and CI runners
#     have neither, so they are NOT throttled and build at full speed.
#   * any other OS (native Windows Git Bash/MSYS, etc.) — no-op. Windows
#     developers use WSL2, which reports as Linux and follows the Linux rule
#     (WSLg sets DISPLAY, so a WSL desktop session is throttled; a headless WSL
#     shell is not).
#
# Memory is not a throttle input: the failure mode is CPU/scheduling contention,
# not memory pressure (builds here ran with RAM to spare). Sizing by cores
# addresses the actual binding constraint.
#
# Usage (consumed by the root Makefile):
#   scripts/local-throttle.sh jobs       # parallelism cap, or empty when off
#   scripts/local-throttle.sh prefix     # command priority wrapper, or empty when off
#   scripts/local-throttle.sh lockfile [N] # Nth cross-session lock path (default 1)
#   scripts/local-throttle.sh slots      # how many concurrent heavy runs are allowed
#
# Capping parallelism (jobs) bounds ONE run's fan-out, but nothing stops three
# concurrent worktree/session `make check` runs from each launching that many
# workers and collectively saturating a small core count — at which point every
# phase stretches and golangci-lint blows its deadline (it counts the wait for
# its own parallel-runner lock against that budget too). `lockfile` names a
# shared advisory lock the heavy phases hold (see serialize_heavy_build in
# scripts/lib/common.sh) so sibling runs queue and each runs at full throttle in
# turn instead of trampling each other.
#
# That lock started out EXCLUSIVE — one heavy run machine-wide. On a box running
# several worktree sessions that made the gate, not the work, set the pace: one
# run used `jobs` of the machine's threads while every sibling blocked for its
# whole duration (waits up to 5 h were observed). It is now an N-slot semaphore
# (`slots`): N runs proceed at once, the rest queue. N holders at `jobs` each can
# oversubscribe the physical cores, deliberately — the desktop-safety property is
# the priority demotion (CPU *and* I/O below the compositor), which every holder
# still carries; the parallelism cap is a secondary bound. Set
# GAG_HEAVY_BUILD_SLOTS=1 to restore strict serialization.
#
# N=2 is measured, not guessed (Q441). Aggregate throughput of M concurrent
# cold-cache lints, relative to one: 2 holders 1.25x, 3 holders 1.30x, 4 holders
# 1.31x — the second slot is the only one that pays, and the tail keeps growing
# past it (worst single wake 16.8 ms at 4 holders, past a 60 Hz frame). One
# holder already reaches 81-85% CPU on this machine, which is why there is so
# little left for a second to claim. Slot 2 still earns its place on LATENCY as
# much as throughput: two holders finish in 36.7 s where strict serialization
# needs 23.0 + 23.0 = 46 s, and the sibling starts immediately instead of
# blocking for a full run.
set -euo pipefail

# Physical cores left for the GUI/foreground apps when throttling.
readonly GUI_CORE_HEADROOM=2

# Concurrent heavy runs allowed on a machine with enough cores to overlap two of
# them. Below that there is nothing to overlap: 3 physical cores means jobs=1,
# and a second holder would just thrash the one core the UI is not using.
readonly DEFAULT_HEAVY_BUILD_SLOTS=2
readonly MIN_CORES_FOR_MULTIPLE_SLOTS=4

# os_kind prints a normalized platform tag: darwin | linux | other.
os_kind() {
	case "$(uname -s)" in
		Darwin) printf 'darwin' ;;
		Linux) printf 'linux' ;;
		*) printf 'other' ;;
	esac
}

# linux_has_gui returns success when a graphical session is present, i.e. there
# is a desktop to keep responsive. False on headless servers, plain SSH, and CI.
linux_has_gui() {
	[[ -n "${DISPLAY:-}" || -n "${WAYLAND_DISPLAY:-}" ]]
}

# throttle_active returns success only on an interactive, GUI-bearing dev shell
# that is not CI.
throttle_active() {
	[[ -n "${CI:-}" ]] && return 1
	case "$(os_kind)" in
		darwin) return 0 ;;
		linux) linux_has_gui ;;
		*) return 1 ;;
	esac
}

# physical_cores prints a best-effort physical (not logical) core count,
# defaulting to 1 when it cannot be determined.
physical_cores() {
	local n=""
	case "$(os_kind)" in
		darwin)
			n="$(sysctl -n hw.physicalcpu 2>/dev/null || true)"
			;;
		linux)
			# Count distinct (socket, core) pairs so hyperthreads count once.
			if command -v lscpu >/dev/null 2>&1; then
				n="$(lscpu -p=socket,core 2>/dev/null | grep -v '^#' | sort -u | wc -l | tr -d '[:space:]' || true)"
			fi
			# Fall back to logical CPUs when lscpu is unavailable or returned 0.
			if [[ -z "$n" || "$n" == "0" ]] && command -v nproc >/dev/null 2>&1; then
				n="$(nproc 2>/dev/null || true)"
			fi
			;;
	esac
	[[ "$n" =~ ^[0-9]+$ ]] || n=1
	(( n < 1 )) && n=1
	printf '%s' "$n"
}

# compute_jobs prints max(1, physical_cores - GUI_CORE_HEADROOM).
compute_jobs() {
	local cores jobs
	cores="$(physical_cores)"
	jobs=$(( cores - GUI_CORE_HEADROOM ))
	(( jobs < 1 )) && jobs=1
	printf '%s\n' "$jobs"
}

# has_ionice returns success when ionice(1) is installed. A named function rather
# than an inline `command -v` so local-throttle-test.sh can assert BOTH Linux
# branches on a machine that has neither tool.
has_ionice() {
	command -v ionice >/dev/null 2>&1
}

# qos_prefix prints the per-OS command wrapper that drops the build to
# background/idle scheduling priority.
qos_prefix() {
	local prefix
	case "$(os_kind)" in
		darwin)
			# `-d throttle` demotes disk I/O only (taskpolicy is the only macOS
			# knob that throttles I/O — there is no ionice); `nice -n 10` adds the
			# CPU demotion. Deliberately NOT a `-c` QoS clamp: that confines the
			# build to one performance cluster. See the header note on Q441.
			printf '%s\n' "nice -n 10 taskpolicy -d throttle"
			;;
		linux)
			prefix="nice -n 19"
			# ionice (util-linux) is usually present but not guaranteed; idle I/O
			# class further protects the desktop from build I/O storms.
			if has_ionice; then
				prefix="${prefix} ionice -c 3"
			fi
			printf '%s\n' "$prefix"
			;;
	esac
}

# compute_slots prints how many heavy runs may proceed concurrently.
# GAG_HEAVY_BUILD_SLOTS overrides it (1 restores the old strict serialization);
# a non-numeric or zero override is ignored rather than breaking a build.
compute_slots() {
	local override="${GAG_HEAVY_BUILD_SLOTS:-}"
	if [[ "$override" =~ ^[0-9]+$ ]] && (( override >= 1 )); then
		printf '%s\n' "$override"
		return
	fi
	if (( $(physical_cores) >= MIN_CORES_FOR_MULTIPLE_SLOTS )); then
		printf '%s\n' "$DEFAULT_HEAVY_BUILD_SLOTS"
	else
		printf '1\n'
	fi
}

# lock_file [N] prints the path of the Nth advisory lock (default 1) bounding the
# heavy local build phases across concurrent worktrees/sessions on one machine.
# They live in the per-user cache dir — OUTSIDE any worktree — so every checkout
# of this repo (the main tree and each .claude/worktrees/* clone) coordinates on
# the SAME files. Printed only when throttling is active (the same GUI-dev-shell
# gate as jobs/prefix); empty on CI/headless so those runs stay fully parallel.
#
# Slot 1 keeps the original single-lock filename on purpose: a worktree still on
# the pre-semaphore code takes exactly that path, so it contends with slot 1 here
# rather than running unbounded alongside us until every checkout has this change.
lock_file() {
	local index="${1:-1}"
	[[ "$index" =~ ^[0-9]+$ ]] && (( index >= 1 )) || index=1
	local base
	case "$(os_kind)" in
		darwin) base="$HOME/Library/Caches" ;;
		linux) base="${XDG_CACHE_HOME:-$HOME/.cache}" ;;
		*) return 0 ;;
	esac
	local dir="$base/github-actions-gateway"
	# A missing cache dir or unwritable home should never break a build — fall
	# back to no lock (unserialized) rather than failing.
	mkdir -p "$dir" 2>/dev/null || return 0
	if (( index == 1 )); then
		printf '%s\n' "$dir/local-heavy-build.lock"
	else
		printf '%s\n' "$dir/local-heavy-build.$index.lock"
	fi
}

main() {
	local want="${1:-}"

	# Off-switch and non-GUI/CI: print nothing so the Makefile runs unthrottled.
	if ! throttle_active; then
		return 0
	fi

	case "$want" in
		jobs) compute_jobs ;;
		prefix) qos_prefix ;;
		lockfile) lock_file "${2:-1}" ;;
		slots) compute_slots ;;
		*)
			printf 'usage: %s {jobs|prefix|lockfile [N]|slots}\n' "$0" >&2
			return 2
			;;
	esac
}

# Run main only when executed directly, so local-throttle-test.sh can source
# this file to exercise the pure sizing helpers without shelling out per case.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
