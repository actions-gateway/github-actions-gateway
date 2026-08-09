#!/usr/bin/env bash
#
# Heartbeat for a running e2e suite (Q608).
#
# At --procs 6 the Ginkgo log is close to silent: spec starts are suppressed
# entirely in parallel mode, a passing spec is a bare dot, and two measured runs
# went 98 s and 78 s between any output at all. This prints one append-only line
# per interval naming what is running, what has finished, and how many specs are
# left.
#
# It runs OUTSIDE the suite on purpose. Ginkgo intercepts stdout for the
# duration of each spec, so anything the suite itself prints is buffered until
# that spec ends — which is the silence being fixed. The suite writes events to
# a file instead (cmd/gmc/test/e2e/progress_report_test.go) and this renders
# them. Full rationale: docs/development/testing.md § Watching an e2e run in
# progress.
#
# Usage:
#   scripts/e2e/progress-watch.sh &          # started by the root Makefile's `e2e`
#   E2E_PROGRESS_FILE=... TEST_PROGRESS_INTERVAL=15 scripts/e2e/progress-watch.sh
#
# Sourcing this file defines its helpers without starting the loop, which is how
# progress-watch-test.sh asserts the rendering.
set -euo pipefail
shopt -s inherit_errexit

PROGRESS_FILE="${E2E_PROGRESS_FILE:-tmp/e2e-progress.jsonl}"
# Shared with the unit tier's renderer (devtools/gotest/progress), so one knob
# paces every tier — including 0, which turns progress off rather than spinning.
PROGRESS_INTERVAL="${TEST_PROGRESS_INTERVAL:-30}"

# Longest spec text shown per running spec. The full text runs to ~200 chars
# (container hierarchy + leaf); the leading chars are what identify it.
PROGRESS_SPEC_WIDTH="${E2E_PROGRESS_SPEC_WIDTH:-50}"

# render_progress FILE NOW — write the heartbeat line for FILE as of NOW (unix
# seconds) to stdout. Pure: no clock, no filesystem beyond FILE. Prints nothing
# until the suite's `total` event lands, so the long cluster bring-up before the
# first spec does not emit a stream of 0/0 lines.
#
# Each parallel process runs one spec at a time, so a process's most recent
# event decides its state: a trailing `start` means it is still in that spec, a
# trailing `end` means it is between specs.
render_progress() {
	local file="$1" now="$2"
	[[ -r "$file" ]] || return 0

	# A jq failure here is a torn or truncated read, not a suite problem — skip
	# the tick rather than killing the watcher mid-run.
	jq -rs --argjson now "$now" --argjson width "$PROGRESS_SPEC_WIDTH" '
		def clip(s): if (s | length) > $width then s[0:$width] + "..." else s end;
		def pad(n): if n < 10 then "0\(n)" else "\(n)" end;
		def dur(s):
			(s | floor) as $x
			| if $x >= 60 then "\(($x / 60) | floor)m\(pad($x % 60))s" else "\($x)s" end;
		def clock(s):
			(s | floor) as $x
			| "\(($x / 60) | floor):\(pad($x % 60))";

		. as $evs
		| ($evs | map(select(.kind == "total")) | last) as $total
		| if $total == null then empty else
			($evs | map(select(.kind == "end"))) as $ends
			| ($evs | map(.t) | min) as $started
			| ($evs
				| map(select(.kind == "start" or .kind == "end"))
				| group_by(.proc)
				| map(last)
				| map(select(.kind == "start"))) as $running
			| ($ends | map(select(.state == "passed")) | length) as $ok
			| ($ends | map(select(.state == "skipped" or .state == "pending")) | length) as $skipped
			| ($ends | length) as $done
			| ($done - $ok - $skipped) as $failed
			| "[e2e t+\(clock($now - $started))] \($done)/\($total.total) specs"
			+ " | \($ok) ok, \($failed) failed, \($skipped) skipped"
			+ (if ($running | length) == 0 then " | running: none"
				else " | running: " + ($running
					| map("\(clip(.spec)) (\(dur($now - .t)))")
					| join(", "))
				end)
		end
	' "$file" 2>/dev/null || return 0
}

main() {
	local last_tick=0
	[[ "$PROGRESS_INTERVAL" == 0 ]] && return 0
	# A final render on the way out: the Makefile stops the watcher as soon as
	# ginkgo exits, and without this the last interval's worth of completions
	# never appears.
	trap 'render_progress "$PROGRESS_FILE" "$(date +%s)"; exit 0' TERM INT

	while true; do
		# Backgrounded sleep + wait, not a bare sleep: bash defers a trapped
		# signal until the running command finishes, so a bare `sleep 30` would
		# hold teardown for up to a full interval after ginkgo exits. `wait` is
		# interrupted by the signal immediately.
		sleep "$PROGRESS_INTERVAL" &
		wait $! || true
		last_tick=$(date +%s)
		render_progress "$PROGRESS_FILE" "$last_tick"
	done
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
