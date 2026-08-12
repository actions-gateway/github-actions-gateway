#!/usr/bin/env bash
#
# pr-mergeability-watch.sh — wake the dispatcher when a handed-off PR stops
# being mergeable.
#
# A worker's pr-sentinel watcher exits at `ready`, so a PR handed back for
# review sits open with nothing watching it. That is exactly the window a
# sibling merge turns it DIRTY, and no CI event fires when it happens
# (docs/development/parallel-dispatch.md#the-post-ready-gap).
#
# Deliberately narrower than pr-sentinel rather than a second copy of it:
#   - It reads `mergeStateStatus` and `state` ONLY. Never the PR body, review
#     comments or issue comments, so no text a third party can write reaches
#     the session that acts on the exit.
#   - It carries no CI output. Check failures stay with the worker that owns
#     the PR, so a dispatcher watching a whole batch does not accumulate logs
#     for work it is not fixing.
#   - It sleeps between polls, which relaunching pr-sentinel on `ready` cannot:
#     that re-evaluates at once, reports `ready` again, and spins.
#
# The budget counts time this script spent sleeping, not wall clock. One clock
# means a stubbed `sleep` advances the accounting deterministically, so the
# timeout is assertable without a second timebase to disagree with it — the
# flake class in testing.md#two-clocks-in-one-assertion.
#
# UNKNOWN is not a conflict. GitHub computes mergeability asynchronously and
# reports UNKNOWN while it does, so treating it as DIRTY would wake the
# dispatcher for every freshly pushed PR.
#
# Usage: pr-mergeability-watch.sh <pr>
# Env:
#   PR_MERGEABILITY_INTERVAL  seconds between polls (default 60)
#   PR_MERGEABILITY_TIMEOUT   polling budget in seconds (default 21600, 6h)
#   PR_MERGEABILITY_REPO      OWNER/NAME (default: whatever gh resolves)
# Exit: 0 having printed one event — conflict, closed, timeout or error.
#       2 on a usage error.
set -euo pipefail
shopt -s inherit_errexit

readonly MAX_CONSECUTIVE_FAILURES=5

usage() {
	echo "usage: pr-mergeability-watch.sh <pr>" >&2
	exit 2
}

# emit — print the event and why, then leave. The dispatcher reads this as the
# background task's output, so it names the next action rather than a status.
emit() {
	local event="$1" detail="$2"
	echo "event: ${event}"
	echo "pr: #${PR}"
	echo "${detail}"
	exit 0
}

main() {
	local pr="${1:-}"
	[[ -n "$pr" ]] || usage
	[[ "$pr" =~ ^[0-9]+$ ]] || usage

	PR="$pr"
	readonly PR

	local interval="${PR_MERGEABILITY_INTERVAL:-60}"
	local budget="${PR_MERGEABILITY_TIMEOUT:-21600}"
	local repo_args=()
	[[ -n "${PR_MERGEABILITY_REPO:-}" ]] && repo_args=(--repo "${PR_MERGEABILITY_REPO}")

	local slept=0 failures=0
	local raw state merge_state

	while true; do
		# Two fields, nothing else. A wider --json is how a comment stream
		# becomes an injection channel into whatever acts on this output.
		if ! raw=$(gh pr view "$PR" "${repo_args[@]}" \
			--json state,mergeStateStatus \
			--jq '"\(.state) \(.mergeStateStatus)"' 2>&1); then
			failures=$((failures + 1))
			if ((failures >= MAX_CONSECUTIVE_FAILURES)); then
				emit error "gh failed ${failures} times in a row; last output: ${raw}"
			fi
			sleep "$interval"
			slept=$((slept + interval))
			continue
		fi
		failures=0

		state="${raw%% *}"
		merge_state="${raw##* }"

		if [[ "$state" != "OPEN" ]]; then
			emit closed "The PR is ${state}. Nothing left to watch; drop it from the tracker."
		fi

		case "$merge_state" in
		DIRTY | BEHIND)
			emit conflict "mergeStateStatus is ${merge_state}. Wake the owning worker to rebase onto origin/main, re-run make check, and force-push with lease. State the condition that invalidates the instruction, since delivery timing is not bounded."
			;;
		esac

		if ((slept >= budget)); then
			emit timeout "Budget of ${budget}s of polling elapsed with the PR still OPEN and ${merge_state}. Relaunch if it is still awaiting merge."
		fi

		sleep "$interval"
		slept=$((slept + interval))
	done
}

main "$@"
