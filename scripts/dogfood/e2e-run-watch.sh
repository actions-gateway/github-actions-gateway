#!/usr/bin/env bash
#
# Watch a dispatched e2e run and relay its spec-level progress locally (Q615).
#
# Replaces `gh run watch` in the release-validation gate. Not because gh is
# broken — it degrades to plain append-only text when stdout is not a TTY, so a
# background run captures cleanly — but because it BLOCKS. The e2e job already
# prints a spec heartbeat every 30 s (scripts/e2e/progress-watch.sh); sitting
# inside gh's loop means the operator sees job-level status only, while the
# spec-level detail they want stays in a log they have to leave the terminal to
# read. Watching the run ourselves lets that heartbeat come through.
#
# The in-flight log is reachable: `gh run view --log` refuses on a run that is
# still going, but the jobs/<id>/logs REST endpoint returns partial logs for a
# running job.
#
# Poll cadence defaults to the heartbeat's own 30 s — the log grows to a few
# hundred KB and each fetch returns all of it, so polling faster costs transfer
# and buys nothing.
#
# Usage:
#   REPO=owner/repo scripts/dogfood/e2e-run-watch.sh <run-id>
#
# Exits with the run's conclusion: 0 for success, non-zero otherwise. A run that
# never concludes exits 124 once E2E_RUN_WATCH_TIMEOUT elapses (Q629), so the
# gate fails and its EXIT trap tears the cluster back down instead of the watch
# holding billable nodes for as long as the process lives.
#
# Each relayed heartbeat is also folded into the gate's event stream (Q616), so
# the status file answers "how far into the e2e leg" rather than only "in the
# e2e phase, 18 minutes". A standalone run writes nothing: progress_heartbeat
# appends only to a stream the gate already started.
#
# Sourcing this file defines its helpers without watching anything, which is how
# e2e-run-watch-test.sh asserts them.
set -euo pipefail
shopt -s inherit_errexit

E2E_RUN_WATCH_REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/dogfood/lib/progress.sh
source "${E2E_RUN_WATCH_REPO_ROOT}/scripts/dogfood/lib/progress.sh"

# Jobs whose logs are scanned for heartbeat lines. A substring match, so it
# survives the `e2e / e2e` vs `e2e` naming the reusable workflow produces; extra
# matches (the gate job) simply carry no heartbeats.
E2E_JOB_FILTER="${E2E_JOB_FILTER:-e2e}"
E2E_RUN_WATCH_INTERVAL="${E2E_RUN_WATCH_INTERVAL:-30}"

# Upper bound on one watch (Q629). The loop's only other exit is `completed`, so
# a run that never gets there — the rc.5 case, stuck `queued` because the AGC
# stopped provisioning — holds the gate, and its nodes, indefinitely.
#
# 90 minutes = the 60-minute job ceiling (`timeout_minutes` in e2e-calico.yml,
# the largest any gate-dispatchable e2e workflow passes; GitHub cancels the job
# past it, which concludes the run and ends the loop on its own) plus the 30
# minutes validate-release.sh already allows an e2e run to move in
# (E2E_WAIT_TIMEOUT). A healthy leg measures 25-33 minutes end to end, so this
# cannot fire on a run that is merely slow — the failure #1171 fixed.
E2E_RUN_WATCH_TIMEOUT="${E2E_RUN_WATCH_TIMEOUT:-5400}"

# Exit status for a watch that hit its deadline, distinct from a run that
# concluded non-success. Matches timeout(1).
E2E_RUN_WATCH_TIMEOUT_RC=124

now() { date +%s; }

# heartbeat_lines — filter a job log on stdin to the heartbeat lines the e2e
# suite emits, stripping the runner's leading ISO timestamp. Matching the
# literal marker rather than parsing the log's structure keeps this indifferent
# to everything else in it.
heartbeat_lines() {
	grep -o '\[e2e t+.*' || true
}

# lines_after N — print the lines of stdin beyond the first N. The log endpoint
# returns the whole log every call, so this is what makes the relay emit each
# heartbeat exactly once.
lines_after() {
	local seen="$1"
	awk -v seen="$seen" 'NR > seen'
}

# relay_heartbeats LINES — print the unseen heartbeat lines for the operator and
# record the newest one in the gate's event stream. Only the newest is recorded:
# the status object answers "where is the run now", and the terminal above
# already carries the whole history.
relay_heartbeats() {
	local lines="$1"
	[[ -n "$lines" ]] || return 0
	printf '%s\n' "$lines"
	progress_heartbeat "$(printf '%s\n' "$lines" | tail -n1)"
}

# conclusion_rc CONCLUSION — the exit status a run conclusion maps to. Anything
# that is not an outright success fails the gate: `cancelled` and `timed_out`
# are as fatal to a release as `failure`, and treating an unrecognized
# conclusion as success would let a new GitHub state silently pass it.
conclusion_rc() {
	case "$1" in
	success) echo 0 ;;
	*) echo 1 ;;
	esac
}

# e2e_job_ids RUN_ID — ids of the run's jobs whose name matches E2E_JOB_FILTER.
# Empty while the run is still queued; the caller keeps polling.
e2e_job_ids() {
	gh api "repos/${REPO}/actions/runs/$1/jobs" \
		--jq ".jobs[] | select(.name | contains(\"${E2E_JOB_FILTER}\")) | .id" 2>/dev/null || true
}

# collect_heartbeats JOB_IDS — every heartbeat line currently in those jobs'
# logs, in order. A fetch failure yields nothing rather than aborting: a
# transient API error must not kill an hour-long gate.
#
# The `|| true` is what makes that true. The logs endpoint 404s for a job that
# is queued — the normal state for the minutes between the job appearing and a
# runner picking it up — and under `pipefail` that status propagates out of the
# pipe, out of the `all="$(…)"` assignment, and into `set -e`. Redirecting
# stderr hides the message, not the status.
collect_heartbeats() {
	local job_id
	for job_id in $1; do
		{ gh api "repos/${REPO}/actions/jobs/${job_id}/logs" 2>/dev/null || true; } | heartbeat_lines
	done
}

# run_state RUN_ID — "<status> <conclusion>".
run_state() {
	gh run view "$1" --repo "${REPO}" --json status,conclusion \
		--jq '"\(.status) \(.conclusion // "")"' 2>/dev/null || echo "unknown "
}

watch_run() {
	local run_id="$1"
	local seen=0 job_ids="" status conclusion state all new total deadline

	echo "  watching https://github.com/${REPO}/actions/runs/${run_id}"
	# The sentinel consults this run's own status to tell a quiet-but-healthy leg
	# from a wedged gate (Q630). The heartbeat relay below cannot carry that: when
	# the log endpoint serves nothing the stream stays silent for the whole leg,
	# which reads exactly like a wedge.
	progress_run "${REPO}" "${run_id}"
	deadline=$(($(now) + E2E_RUN_WATCH_TIMEOUT))

	while true; do
		state="$(run_state "${run_id}")"
		status="${state%% *}"
		conclusion="${state#* }"

		[[ -n "${job_ids}" ]] || job_ids="$(e2e_job_ids "${run_id}")"

		if [[ -n "${job_ids}" ]]; then
			all="$(collect_heartbeats "${job_ids}")"
			total="$(printf '%s' "${all}" | grep -c . || true)"
			# A shrinking log means a re-fetch raced a rotation; re-sync rather
			# than replaying the whole stream.
			((total < seen)) && seen="${total}"
			if ((total > seen)); then
				new="$(printf '%s\n' "${all}" | lines_after "${seen}")"
				relay_heartbeats "${new}"
				seen="${total}"
			fi
		fi

		[[ "${status}" == "completed" ]] && break

		# Checked after the completion break, so a run that concludes on the
		# same poll the deadline lands on still reports its conclusion.
		if (($(now) >= deadline)); then
			echo "error: run ${run_id} was still '${status}' after ${E2E_RUN_WATCH_TIMEOUT}s — giving up." >&2
			echo "  The run keeps going on GitHub; the gate fails here so its teardown" >&2
			echo "  releases the cluster. Raise E2E_RUN_WATCH_TIMEOUT (currently" >&2
			echo "  ${E2E_RUN_WATCH_TIMEOUT}s) if the leg is legitimately slower than that." >&2
			echo "  https://github.com/${REPO}/actions/runs/${run_id}" >&2
			return "${E2E_RUN_WATCH_TIMEOUT_RC}"
		fi

		sleep "${E2E_RUN_WATCH_INTERVAL}" &
		wait $! || true
	done

	echo "  run ${run_id} completed: ${conclusion}"
	return "$(conclusion_rc "${conclusion}")"
}

main() {
	local run_id="${1:-}"
	[[ -n "${run_id}" ]] || {
		echo "usage: REPO=owner/repo $0 <run-id>" >&2
		exit 2
	}
	: "${REPO:?REPO must be set}"
	watch_run "${run_id}"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
