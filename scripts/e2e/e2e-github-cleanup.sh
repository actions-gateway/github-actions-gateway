#!/usr/bin/env bash
#
# e2e-github-cleanup.sh — clear live-GitHub e2e state stranded on the fixture repo.
#
# A live-GitHub run stopped with `kill -9` skips Ginkgo's AfterAll, so the
# ActionsGateway CR is never deleted and the agentpool-cleanup finalizer never gets
# its window to deregister that tenant's runners. Those registrations survive the
# cluster that owned them and keep accepting job assignments, so the NEXT run's job
# goes in_progress against a runner that no longer exists and no worker pod is ever
# provisioned. A job already assigned that way never completes either, which is why
# the in-flight workflow run is cleared alongside the runner (Q511).
#
# The suite's BeforeAll preflight refuses to start while either is present and names
# this script as the remedy. The runner-name rule below must stay in step with
# suiteRunnerPrefixes in cmd/gmc/test/e2e/github_e2e_test.go: the preflight blocks on
# exactly the state this clears, so a narrower filter here wedges the next run.
#
# DESTRUCTIVE, against real GitHub. A peer session's live runners and workflow runs
# look exactly like wreckage from here — there is no way to tell them apart — so run
# it only once you have confirmed no live-GitHub run is in flight anywhere.
#
# Usage:
#   e2e-github-cleanup.sh              # report, confirm, then clear
#   e2e-github-cleanup.sh --dry-run    # report only, change nothing
#
# Environment:
#   GITHUB_E2E_ORG      org owning the fixture repo (required)
#   GITHUB_E2E_REPO     fixture repo name (required)
#   GITHUB_E2E_GATEWAY  gateway name whose runners to clear (default: real-ag,
#                       the name the live-GitHub suite applies)
#   ASSUME_YES=1        skip the confirmation prompt

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

readonly DEFAULT_GATEWAY='real-ag'
# Budget for waiting out GitHub's post-cancel release of a runner it still reports as
# mid-job. Roughly the AGC's own recycle budget (agentpool: 6 attempts, 2s→15s).
readonly DELETE_MAX_ATTEMPTS=6
readonly DELETE_RETRY_DELAY=10

usage() {
	awk '/^# Usage:/,/^$/ { sub(/^#[[:space:]]?/, ""); print }' "$0"
}

# select_suite_runners GATEWAY — read "<id> <name>" lines on stdin, print the ones
# naming a runner the live-GitHub suite owns.
#
# agentpool names an agent "<runnerGroup>-<index>", or "rs-<runnerSet>-<index>" under
# the v2 scheme, and the group name derives from the gateway name — so the suite's
# runners are exactly those two prefixes and nothing else in the repo matches. Matching
# on the gateway name alone would be looser than the preflight and could delete a
# registration this suite never made.
select_suite_runners() {
	local gateway="$1"
	awk -v prefix="$gateway-" -v rs_prefix="rs-$gateway-" '
		{
			name = $2
			if (index(name, prefix) == 1 || index(name, rs_prefix) == 1) { print }
		}
	'
}

# suite_runners REPO GATEWAY — "<id> <name> <status> busy=<bool>" per stranded runner.
suite_runners() {
	local repo="$1" gateway="$2"
	gh api "repos/$repo/actions/runners" \
		--jq '.runners[] | "\(.id) \(.name) \(.status) busy=\(.busy)"' |
		select_suite_runners "$gateway"
}

# active_runs REPO — "<id> <workflow> <status>" per workflow run not yet completed.
# Unscoped by workflow: the fixture repo serves this suite alone, so anything still
# in flight there belongs to a run that did not finish cleanly.
active_runs() {
	local repo="$1"
	gh api "repos/$repo/actions/runs?per_page=50" \
		--jq '.workflow_runs[] | select(.status != "completed") | "\(.id) \(.path) \(.status)"'
}

# delete_runner REPO ID NAME — deregister one runner, waiting out the window in which
# GitHub still considers it mid-job (422, "runner is currently running a job").
#
# Cancelling the run above starts that release but does not complete it, so the first
# DELETE after a cancel usually refuses. The AGC waits out the same window on its own
# recycle path (agentpool's registerAgentWithBusyRetry) on a comparable budget. Any
# other error, or a runner still busy at the end of it, is reported rather than fatal —
# one stuck runner must not skip the rest of the sweep.
delete_runner() {
	local repo="$1" id="$2" name="$3" out attempt
	for ((attempt = 1; attempt <= DELETE_MAX_ATTEMPTS; attempt++)); do
		if out=$(gh api --method DELETE "repos/$repo/actions/runners/$id" 2>&1); then
			echo "  deregistered $name (id $id)"
			return 0
		fi
		[[ "$out" == *'currently running a job'* ]] || break
		((attempt < DELETE_MAX_ATTEMPTS)) || break
		echo "  $name (id $id) still mid-job; waiting ${DELETE_RETRY_DELAY}s (attempt $attempt/$DELETE_MAX_ATTEMPTS)"
		sleep "$DELETE_RETRY_DELAY"
	done
	echo "  FAILED to deregister $name (id $id): $out" >&2
	return 1
}

# cancel_run REPO ID — cancel one workflow run. A run that concluded between the
# listing and here is already what we wanted, so its refusal is not a failure.
cancel_run() {
	local repo="$1" id="$2" out
	if out=$(gh api --method POST "repos/$repo/actions/runs/$id/cancel" 2>&1); then
		echo "  cancelled run $id"
		return 0
	fi
	echo "  FAILED to cancel run $id: $out" >&2
	return 1
}

main() {
	local dry_run=0

	while (($# > 0)); do
		case "$1" in
		--dry-run) dry_run=1 && shift ;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown argument: $1" ;;
		esac
	done

	require_cmd gh https://cli.github.com/
	require_cmd jq https://jqlang.github.io/jq/download/

	local org="${GITHUB_E2E_ORG:-}" repo_name="${GITHUB_E2E_REPO:-}"
	[[ -n "$org" && -n "$repo_name" ]] ||
		die 'set GITHUB_E2E_ORG and GITHUB_E2E_REPO to name the fixture repo'
	local repo="$org/$repo_name"
	local gateway="${GITHUB_E2E_GATEWAY:-$DEFAULT_GATEWAY}"

	step "Inspecting $repo (gateway: $gateway)"
	local runners runs
	runners=$(suite_runners "$repo" "$gateway")
	runs=$(active_runs "$repo")

	if [[ -z "$runners" && -z "$runs" ]]; then
		echo "Nothing to clear — no suite runners registered, no run in flight."
		return 0
	fi

	if [[ -n "$runners" ]]; then
		echo
		echo 'Registered runners owned by this suite:'
		echo "$runners" | awk '{ print "  " $0 }'
	fi
	if [[ -n "$runs" ]]; then
		echo
		echo 'Workflow runs not yet completed:'
		echo "$runs" | awk '{ print "  " $0 }'
	fi

	if ((dry_run == 1)); then
		echo
		echo "--dry-run: nothing changed."
		return 0
	fi

	echo
	confirm_or_exit "About to deregister the runners and cancel the runs listed above on $repo.
This is irreversible, and a peer session's live run is indistinguishable from wreckage."

	local failures=0 id name
	# Runs first: a runner GitHub considers mid-job is the one state that refuses
	# deregistration, and cancelling its run is what starts the release.
	if [[ -n "$runs" ]]; then
		step 'Cancelling workflow runs'
		while read -r id _; do
			cancel_run "$repo" "$id" || failures=$((failures + 1))
		done <<<"$runs"
	fi

	if [[ -n "$runners" ]]; then
		step 'Deregistering runners'
		while read -r id name _; do
			delete_runner "$repo" "$id" "$name" || failures=$((failures + 1))
		done <<<"$runners"
	fi

	echo
	((failures == 0)) || die "$failures operation(s) failed — re-run after resolving the errors above"
	echo "Fixture repo cleared. The live-GitHub preflight will pass."
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
