#!/usr/bin/env bash
#
# check-withheld-runs.sh - name every open PR whose checks are withheld at
# action_required on its current head (Q1052).
#
# WHAT IS WITHHELD AND WHY IT IS INVISIBLE
# When one of this repo's own workflows force-pushes to a PR branch with the
# default GITHUB_TOKEN - dependabot-go-sync.yml (Q111) and
# dependabot-rebase-stale.yml (Q427) both do - GitHub creates every run for that
# push and then holds it at action_required until a maintainer clicks "Approve
# and run". Nothing surfaces that: statusCheckRollup is empty, which is the same
# answer it gives for a branch pushed seconds ago, and mergeStateStatus reads
# BLOCKED, which is also what a failing required check reads. So the PR ages with
# no signature of its own. Measured 2026-09-04: 10 of 40 recent Dependabot PRs
# were held, from 1m35s to 4d01h03m; #1877's own hold ran 2026-09-10 to
# 2026-09-16, six days, and #1825 kept a HIGH-severity CVE live on main's
# dependencies for 31h35m.
#
# WHY THE CURRENT HEAD AND NOTHING ELSE
# Approving overwrites action_required with the real result, so the conclusion
# survives only on commits that were never released. Measured 2026-09-16:
# `actions/runs?status=action_required` returned 96 runs across 6 head SHAs, and
# four of those heads belong to PRs that are running fine now - superseded
# commits whose withheld runs will read action_required forever. Two belong to no
# open PR at all. Selecting on that query fires constantly and gets ignored,
# which is the failure mode this check exists to avoid. Selection is therefore
# scoped to each open PR's CURRENT head, where a withheld run is a live block and
# approving it clears the finding.
#
# WHAT IT DELIBERATELY CANNOT SEE
# A PR carrying NO runs at all is not reported. That is genuinely ambiguous - a
# branch pushed seconds ago reads identically - and firing on it would report
# every PR in its first minute. A hold that is already released is not reported
# either; its durable fingerprint is run_attempt >= 2 with run_started_at well
# after created_at, which is a different query and a different question (how
# often does this happen) than this one (what is stuck right now).
#
# Exit: 0 nothing withheld, 1 findings printed, 2 could not measure.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# Exit 2, not common.sh's die(), for every read that failed: the watch workflow
# reads 1 as "these PRs are withheld" and anything above it as "this check is
# broken", and a measurement failure reported as a finding would open an issue
# naming no PRs.
cannot_measure() {
	echo "ERROR: $*" >&2
	exit 2
}

# The conclusion is the whole discriminator, and it is enough on its own: a run
# carries no conclusion until it completes, so `action_required` cannot appear on
# a queued or in-progress run. Pairing it with a status check reads as defensive
# and is not - measured by deleting it, the suite stays green, because no fixture
# or API response can separate the two.
readonly WITHHELD_CONCLUSION='action_required'
# A PR's checks arrive on the pull_request event. Scoping to it keeps a
# deployment-protection hold on the github-pages environment - which fires on
# push to main and can also read action_required - out of a PR-scoped finding.
readonly PR_EVENT='pull_request'

usage() {
	cat <<'EOF'
Usage: scripts/ci/check-withheld-runs.sh
       scripts/ci/check-withheld-runs.sh --list
       scripts/ci/check-withheld-runs.sh --select < PR_RUNS_JSON

Report every open PR whose workflow runs on its current head are withheld at
action_required, waiting on a maintainer's "Approve and run".

  (no args)   query the repo, print a report, exit 1 if anything is withheld
  --list      print the withheld PR numbers, one per line
  --select    filter recorded PR+runs JSON on stdin; the pure half, unit-tested

--select input is an array of {number, headRefOid, runs: [{event, status,
conclusion}]}; it prints "<number> <headRefOid> <withheld>/<total>" per finding.

Env: GH_TOKEN (required for the querying forms).
EOF
}

# select_withheld - read the PR+runs JSON described above on stdin and print one
# line per PR with at least one withheld run on its head.
#
# ANY withheld run, not all of them. The push holds a head's runs as a set, so
# all-or-nothing is what the mechanism produces; but a partial hold - one lane
# approved by hand, the rest still held - blocks the PR just as completely and
# has no other reporter. Requiring all would drop it silently.
#
# `total` counts only pull_request runs, so the ratio a finding prints is
# readable against the number of checks the PR expects.
select_withheld() {
	jq -r \
		--arg event "$PR_EVENT" \
		--arg conclusion "$WITHHELD_CONCLUSION" '
		.[]
		| . as $pr
		| [.runs[]? | select(.event == $event)] as $runs
		| [$runs[] | select(.conclusion == $conclusion)] as $held
		| select(($held | length) > 0)
		| "\($pr.number) \($pr.headRefOid) \($held | length)/\($runs | length)"
	'
}

# collect_pr_runs - print the --select input for every open PR, reading each
# PR's current head from the PR itself and its runs from that exact SHA.
collect_pr_runs() {
	local prs sha runs
	prs="$(gh pr list --state open --limit 100 --json number,headRefOid)"
	# An empty or malformed read here would widen every later query instead of
	# narrowing it, so refuse rather than report "nothing withheld".
	jq -e 'type == "array"' >/dev/null <<<"$prs" ||
		cannot_measure "could not read the open PR list"

	echo '['
	local first=1 number
	while read -r number sha; do
		# A head SHA is 40 hex characters. Checking the form rather than
		# emptiness refuses a truncated read and an error string both, either of
		# which would otherwise reach --head_sha and list the whole repo's runs.
		[[ "$sha" =~ ^[0-9a-f]{40}$ ]] ||
			cannot_measure "PR #$number: head SHA is not a 40-character object name: '$sha'"
		runs="$(gh api --paginate \
			"repos/{owner}/{repo}/actions/runs?head_sha=$sha&per_page=100" \
			--jq '[.workflow_runs[] | {event, status, conclusion}]' |
			jq -s 'add // []')"
		((first)) || echo ','
		first=0
		jq -n --argjson n "$number" --arg sha "$sha" --argjson runs "$runs" \
			'{number: $n, headRefOid: $sha, runs: $runs}'
	done < <(jq -r '.[] | "\(.number) \(.headRefOid)"' <<<"$prs")
	echo ']'
}

# report - print the findings in the form the watch workflow quotes into its
# issue, and return 1 when there are any.
report() {
	local findings="$1" number sha ratio count=0

	while read -r number sha ratio; do
		[[ -n "$number" ]] || continue
		count=$((count + 1))
		printf 'PR #%s: %s runs withheld on %s\n' "$number" "$ratio" "${sha:0:12}"
		printf '  https://github.com/%s/pull/%s\n' "${GITHUB_REPOSITORY:-actions-gateway/github-actions-gateway}" "$number"
	done <<<"$findings"

	if ((count == 0)); then
		echo 'No open PR has withheld checks on its current head.'
		return 0
	fi
	cat <<-EOF

		$count open PR(s) are waiting on a maintainer.

		Release them with "Approve and run" on the PR's checks tab, which starts
		the existing runs in place. Close + reopen also works and queues a second
		full set of runs instead, so it is the costlier click rather than a
		different one.

		A GITHUB_TOKEN push creates these runs and GitHub holds them; see
		docs/development/go-workspaces.md, section "Both bot pushes leave the
		checks withheld pending approval".
	EOF
	return 1
}

main() {
	case "${1:---report}" in
	--select)
		select_withheld
		;;
	--list)
		require_cmd gh jq
		select_withheld <<<"$(collect_pr_runs)" | cut -d' ' -f1
		;;
	--report)
		require_cmd gh jq
		local collected findings rc=0
		# Collected first, then selected, deliberately NOT as one pipeline. Under
		# pipefail a pipeline yields its RIGHTMOST failing status, so a refusal
		# from collect_pr_runs arrived as jq's exit 5 on the truncated JSON it had
		# left behind, with jq's parse error printed over the refusal's own
		# message. The workflow reads anything >= 2 the same way, so the code was
		# survivable and the buried message was not.
		collected="$(collect_pr_runs)"
		findings="$(select_withheld <<<"$collected")"
		report "$findings" || rc=$?
		exit "$rc"
		;;
	-h | --help)
		usage
		;;
	*)
		usage >&2
		exit 2
		;;
	esac
}

main "$@"
