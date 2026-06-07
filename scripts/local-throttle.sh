#!/usr/bin/env bash
#
# local-throttle.sh — emit resource-throttle settings for heavy local builds.
#
# A full `make check` runs golangci-lint per module (each fanning out to one
# worker per logical CPU — golangci-lint ignores GOMAXPROCS) plus `go test`
# across every module. On a small machine that saturates every core. On macOS
# the casualty is the WindowServer compositor: starved of CPU, it misses its
# kernel watchdog deadline and gets restarted, freezing the entire GUI (visible
# as `WindowServer ... userspace_watchdog_timeout` in the crash reports).
#
# To keep the machine usable, an interactive macOS shell throttles the heavy
# phases two ways:
#   1. Run them at background QoS via `taskpolicy -b`, so the scheduler always
#      preempts them in favour of foreground apps (WindowServer included). This
#      is the root-cause fix — the GUI stays responsive even under full load.
#   2. Cap parallelism to (physical cores − 2), leaving cores for the UI. This
#      is the only lever that reaches golangci-lint, which takes `-j` but reads
#      no GOMAXPROCS/GOFLAGS env.
#
# Throttling is auto-detected and applies ONLY when both hold:
#   * not running under CI (the CI env var is unset — GitHub Actions et al. set
#     it), and
#   * the OS is macOS (Darwin), the only platform with the WindowServer issue
#     and the `taskpolicy` tool.
# Otherwise every subcommand prints nothing, so the Makefile runs at full speed
# (e.g. on the Linux CI runners that mirror this gate).
#
# Memory is not a throttle input: the failure mode is CPU/scheduling contention
# started the WindowServer watchdog, not memory pressure (builds here ran with
# RAM to spare). Sizing by cores addresses the actual binding constraint.
#
# Usage (consumed by the root Makefile):
#   scripts/local-throttle.sh jobs     # parallelism cap, or empty when off
#   scripts/local-throttle.sh prefix   # command QoS prefix, or empty when off
#
# The minimum headroom left for the GUI/foreground apps, in physical cores.
set -euo pipefail

readonly GUI_CORE_HEADROOM=2

# throttle_active returns success only on an interactive macOS shell (not CI).
throttle_active() {
	[[ -z "${CI:-}" ]] && [[ "$(uname -s)" == "Darwin" ]]
}

# compute_jobs prints max(1, physical_cores - GUI_CORE_HEADROOM).
compute_jobs() {
	local cores jobs
	cores="$(sysctl -n hw.physicalcpu 2>/dev/null || echo 1)"
	jobs=$(( cores - GUI_CORE_HEADROOM ))
	(( jobs < 1 )) && jobs=1
	printf '%s\n' "$jobs"
}

main() {
	local want="${1:-}"

	# Off-switch and non-macOS/CI: print nothing so the Makefile runs unthrottled.
	if ! throttle_active; then
		return 0
	fi

	case "$want" in
		jobs) compute_jobs ;;
		prefix) printf '%s\n' "taskpolicy -b" ;;
		*)
			printf 'usage: %s {jobs|prefix}\n' "$0" >&2
			return 2
			;;
	esac
}

main "$@"
