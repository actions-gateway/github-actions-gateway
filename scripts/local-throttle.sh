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
#   1. Run them at a low-priority QoS tier so foreground apps (the compositor
#      included) preempt them, and — critically on macOS — so their disk I/O is
#      throttled too. This is the root-cause fix.
#        - macOS: `taskpolicy -c utility` (utility QoS — demotes CPU *and* disk
#          I/O via the QoS band, the only way to throttle I/O on macOS since it
#          has no ionice). The gentler `utility` tier is used rather than the
#          lowest `-b`/background band: it still clamps aggregate build CPU and
#          stays scheduled below the UI, but builds keep making real progress
#          (measured 2–4× faster than `-b`). Why I/O and not just CPU: an
#          unthrottled build already runs at a *lower* QoS than WindowServer yet
#          still triggers the watchdog, so CPU priority alone is not the fix —
#          the I/O demotion that `nice` cannot express on macOS is load-bearing.
#        - Linux: `nice -n 19` (lowest CPU priority), plus `ionice -c 3` (idle
#          I/O class) when ionice is installed — the same CPU+I/O demotion, via
#          Linux's separate knobs.
#   2. Cap parallelism to (physical cores - 2), leaving cores for the UI. This
#      is the only lever that reaches golangci-lint, which takes `-j` but reads
#      no GOMAXPROCS/GOFLAGS env.
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
#   scripts/local-throttle.sh jobs     # parallelism cap, or empty when off
#   scripts/local-throttle.sh prefix   # command priority wrapper, or empty when off
#   scripts/local-throttle.sh lockfile # shared cross-session lock path, or empty when off
#
# Capping parallelism (jobs) bounds ONE run's fan-out, but nothing stops three
# concurrent worktree/session `make check` runs from each launching that many
# workers and collectively saturating a small core count — at which point every
# phase stretches and golangci-lint blows its deadline (it counts the wait for
# its own parallel-runner lock against that budget too). `lockfile` names a
# shared advisory lock the heavy phases hold (see serialize_heavy_build in
# scripts/lib/common.sh) so sibling runs queue and each runs at full throttle in
# turn instead of trampling each other.
set -euo pipefail

# Physical cores left for the GUI/foreground apps when throttling.
readonly GUI_CORE_HEADROOM=2

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

# qos_prefix prints the per-OS command wrapper that drops the build to
# background/idle scheduling priority.
qos_prefix() {
	local prefix
	case "$(os_kind)" in
		darwin)
			# utility QoS: demotes CPU + disk I/O below the UI (taskpolicy is the
			# only macOS knob that throttles I/O — there is no ionice). Gentler
			# than `-b`/background so the build keeps progressing (~2–4× faster)
			# while still leaving headroom for the compositor.
			printf '%s\n' "taskpolicy -c utility"
			;;
		linux)
			prefix="nice -n 19"
			# ionice (util-linux) is usually present but not guaranteed; idle I/O
			# class further protects the desktop from build I/O storms.
			if command -v ionice >/dev/null 2>&1; then
				prefix="${prefix} ionice -c 3"
			fi
			printf '%s\n' "$prefix"
			;;
	esac
}

# lock_file prints the path of the shared advisory lock that serializes the
# heavy local build phases across concurrent worktrees/sessions on one machine.
# It lives in the per-user cache dir — OUTSIDE any worktree — so every checkout
# of this repo (the main tree and each .claude/worktrees/* clone) coordinates on
# the SAME file. Printed only when throttling is active (the same GUI-dev-shell
# gate as jobs/prefix); empty on CI/headless so those runs stay fully parallel.
lock_file() {
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
	printf '%s\n' "$dir/local-heavy-build.lock"
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
		lockfile) lock_file ;;
		*)
			printf 'usage: %s {jobs|prefix|lockfile}\n' "$0" >&2
			return 2
			;;
	esac
}

main "$@"
