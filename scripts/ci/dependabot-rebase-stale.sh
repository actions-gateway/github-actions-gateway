#!/usr/bin/env bash
#
# dependabot-rebase-stale.sh - rebase a stranded Dependabot Go-module PR onto
# current main by replaying its version bumps (Q427, Q1118).
#
# WHY THIS EXISTS INSTEAD OF `@dependabot recreate`
# dependabot-go-sync.yml (Q111) pushes a `chore(deps): sync ...` commit onto the
# bot's branch. That marks the branch as modified by someone else, so Dependabot
# disowns it and never rebases it again; once main moves under the branch the PR
# goes permanently conflicting. The documented manual remedy is a maintainer
# commenting `@dependabot recreate`, and that CANNOT be automated: Dependabot
# accepts comment commands only from *users* with push access and rejects
# GitHub Apps / bots with "Sorry, only users with push access can use that
# command" (dependabot/dependabot-core#9147, still open). This repo deliberately
# stores no Personal Access Token, so the workflow performs the rebase itself.
#
# WHY A REPLAY AND NOT A MERGE
# Hand-merging a stale Go bump can silently DOWNGRADE a module main has since
# moved forward: PRs #733/#734/#735 each conflicted only in api/go.mod + go.sum,
# yet merging any as-is would have reverted golang.org/x/text v0.39.0 back to
# v0.38.0 across three modules. So the conflicted tree is discarded outright:
# the branch is reset to current main, each version bump the PR introduced is
# re-applied with `go get`, and every generated artifact (vendor trees,
# go.work.sum, THIRD-PARTY-NOTICES) is rebuilt by `make deps-sync`. Each bump
# is additionally guarded - a `go get` that Go itself reports as a downgrade is
# rolled back and skipped - and a PR left with no applicable bumps is pushed
# nothing at all, for a human to close.
#
# WHAT IT DELIBERATELY DOES NOT DO
# The force-push uses the workflow's GITHUB_TOKEN. GitHub creates the runs for
# that push and then holds every one at action_required until a maintainer
# approves, so the required PR checks do not start on the rebased commit until
# then. Clearing that is the same one-click step every synced Dependabot PR
# already needs, and the click is "Approve and run" rather than close + reopen.
# See docs/development/go-workspaces.md, section "Both bot pushes leave the
# checks withheld pending approval".
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

BASE_BRANCH="${BASE_BRANCH:-main}"
REMOTE="${REMOTE:-origin}"
MERGEABLE_POLL_ATTEMPTS="${MERGEABLE_POLL_ATTEMPTS:-6}"
MERGEABLE_POLL_DELAY="${MERGEABLE_POLL_DELAY:-10}"
# Each replay runs a full `make deps-sync` (tidy across every workspace
# module, re-vendor, regenerate notices), so a weekly Dependabot wave that
# strands several PRs at once would otherwise run for an hour. Cap the run and
# name what it deferred; the next main push or the daily schedule takes the rest.
MAX_PRS="${MAX_PRS:-3}"

# Dependabot derives its branch prefix from the ecosystem's internal name, so
# gomod bumps land on dependabot/go_modules/* - NOT the 'gomod' key used in
# .github/dependabot.yml. Same gotcha as dependabot-go-sync.yml.
readonly BRANCH_PREFIX='dependabot/go_modules/'
# gh spells a GitHub App actor two ways and the PR author is the field that
# changed: `gh pr list`/`gh pr view --json author` return the `app/dependabot`
# slug, while the REST user object and every commit author keep
# `dependabot[bot]`. Compare against both, or selection silently matches nothing
# and the workflow reports "No open Dependabot Go-module PRs" while exiting 0.
readonly DEPENDABOT_LOGIN='dependabot[bot]'
readonly DEPENDABOT_APP_LOGIN='app/dependabot'

usage() {
	cat <<'EOF'
Usage: scripts/ci/dependabot-rebase-stale.sh [--dry-run] [PR_NUMBER ...]
       scripts/ci/dependabot-rebase-stale.sh --list
       scripts/ci/dependabot-rebase-stale.sh --bumps BASE_GO_MOD TIP_GO_MOD
       scripts/ci/dependabot-rebase-stale.sh --select < GH_PR_LIST_JSON
       scripts/ci/dependabot-rebase-stale.sh --checks < GH_ROLLUP_JSON
       scripts/ci/dependabot-rebase-stale.sh --verdict STATE BEHIND CHECKS

Rebase every stranded Dependabot Go-module PR onto the base branch by replaying
its version bumps there, then regenerating the vendor trees and notices. A PR is
stranded when it conflicts, or when it is behind the base branch with failing
checks. See the header of this script for why it replays instead of merging.

  (no PR numbers)  discover every open PR and act on the eligible ones
  --dry-run        analyse and rebase locally, push and comment nothing
  --list           print the eligible PR numbers, one per line, and exit
  --bumps          print the bumps between two go.mod files, then exit
  --select         filter `gh pr list` JSON on stdin to candidate PR numbers
  --checks         classify a `statusCheckRollup` array on stdin, then exit
  --verdict        print the rescue/skip decision for three readings, then exit

Env: BASE_BRANCH (default main), REMOTE (default origin), MAX_PRS (default 3 -
     each replay runs a full deps-sync, so a run is capped and names what it
     deferred), MERGEABLE_POLL_ATTEMPTS / MERGEABLE_POLL_DELAY - GitHub computes
     mergeability asynchronously, so a PR reports UNKNOWN for a few seconds
     after main moves (default 6 attempts, 10s apart).
EOF
}

# ---------------------------------------------------------------------------
# Bump extraction
# ---------------------------------------------------------------------------

# gomod_requires FILE - print "<module> <version>" for every require directive
# in FILE. Uses `go mod edit -json` rather than parsing the file, so block and
# single-line require forms, comments, and `// indirect` markers all behave.
gomod_requires() {
	go mod edit -json "$1" | jq -r '.Require[]? | "\(.Path) \(.Version)"'
}

# bumps_between BASE_GO_MOD TIP_GO_MOD - print "<module> <wanted-version>" for
# every module both files require at a DIFFERENT version, i.e. the bumps TIP
# introduces.
#
# Requires present on only one side are deliberately ignored. A require that
# appears or disappears is `go mod tidy` bookkeeping rather than a dependency
# bump, and the final `make deps-sync` redoes that bookkeeping anyway; trying
# to replay it would fight tidy over indirect requires main has since dropped.
bumps_between() {
	local base_file="$1" tip_file="$2"
	local -A base_ver=()
	local module version
	while read -r module version; do
		base_ver["$module"]="$version"
	done < <(gomod_requires "$base_file")
	while read -r module version; do
		if [[ -n "${base_ver[$module]:-}" && "${base_ver[$module]}" != "$version" ]]; then
			printf '%s %s\n' "$module" "$version"
		fi
	done < <(gomod_requires "$tip_file")
}

# collect_bumps BASE_SHA TIP_SHA SCRATCH_DIR - print "<module-dir> <module>
# <wanted-version>" for every bump the commit range introduces, across every
# go.mod it touches. A grouped Dependabot PR ("bump the go-deps group across 1
# directory with 5 updates") yields one line per module, which is exactly what
# the replay needs - the branch name carries only the group's hash and cannot be
# parsed for this.
collect_bumps() {
	local base_sha="$1" tip_sha="$2" scratch="$3"
	local path dir pair
	while read -r path; do
		[[ -n "$path" ]] || continue
		dir="$(dirname "$path")"
		# A go.mod added on the branch has no base side, so it contributes no
		# bumps (every require is an addition). Skip rather than diff against
		# nothing.
		git cat-file -e "$base_sha:$path" 2>/dev/null || continue
		mkdir -p "$scratch/base/$dir" "$scratch/tip/$dir"
		git show "$base_sha:$path" >"$scratch/base/$path"
		git show "$tip_sha:$path" >"$scratch/tip/$path"
		while read -r pair; do
			[[ -n "$pair" ]] || continue
			printf '%s %s\n' "$dir" "$pair"
		done < <(bumps_between "$scratch/base/$path" "$scratch/tip/$path")
	done < <(git diff --name-only "$base_sha" "$tip_sha" | grep -E '(^|/)go\.mod$' || true)
}

# ---------------------------------------------------------------------------
# PR selection
# ---------------------------------------------------------------------------

# pr_json PR_NUMBER - print the PR fields this script reasons about. Kept in one
# place so the eligibility check and the rebase agree on what they read.
pr_json() {
	gh pr view "$1" --json number,headRefName,headRefOid,commits,isCrossRepository,author,url
}

# mergeable_state PR_NUMBER - print MERGEABLE or CONFLICTING, polling while
# GitHub still reports UNKNOWN. GitHub computes mergeability lazily and the
# request itself kicks the computation off, so the first read right after a push
# to main is nearly always UNKNOWN. Prints UNKNOWN if it never settles.
mergeable_state() {
	local pr="$1" attempt state
	for ((attempt = 1; attempt <= MERGEABLE_POLL_ATTEMPTS; attempt++)); do
		state="$(gh pr view "$pr" --json mergeable --jq .mergeable)"
		if [[ "$state" != "UNKNOWN" ]]; then
			printf '%s\n' "$state"
			return 0
		fi
		if ((attempt < MERGEABLE_POLL_ATTEMPTS)); then
			sleep "$MERGEABLE_POLL_DELAY"
		fi
	done
	printf 'UNKNOWN\n'
}

# behind_by HEAD_REF - print how many commits the base branch has that HEAD_REF
# does not. No `gh pr view` field answers this: mergeStateStatus reports BEHIND
# only where a branch must be up to date to merge, and this repo's merge queue
# deliberately does not require that, so a stale branch here reads BLOCKED or
# UNSTABLE like any other. The compare API reports it directly. Prints 0 when
# the comparison cannot be made, so an API hiccup skips the PR rather than
# rebasing it on a guess.
behind_by() {
	local head_ref="$1" out
	if ! out="$(gh api "repos/{owner}/{repo}/compare/$BASE_BRANCH...$head_ref" \
		--jq '.behind_by' 2>/dev/null)"; then
		printf '0\n'
		return 0
	fi
	[[ "$out" =~ ^[0-9]+$ ]] || out=0
	printf '%s\n' "$out"
}

# checks_verdict - read a `statusCheckRollup` JSON array on stdin and print
# FAILING, PENDING, PASSING or NONE. Split from checks_state so the
# classification can be tested against recorded rollups.
#
# A WITHHELD HEAD READS NONE, WHICH IS WHAT STOPS THIS SCRIPT LOOPING ON ITS OWN
# FORCE-PUSH. GitHub withholds the runs a GITHUB_TOKEN push creates, and a
# withheld head carries check SUITES at action_required with zero check RUNS
# under them - so the rollup is built from nothing and comes back null. Measured
# 2026-09-18 on 88ebcaa9 and 8a1c6c4c: 16 suites each at completed/
# action_required with latest_check_runs_count 0, 0 check runs, 0 statuses, and
# `statusCheckRollup` null over GraphQL. Q1052 owns that measurement; the
# rescue arm needs FAILING, so NONE skips.
#
# ACTION_REQUIRED is classified PENDING as defence in depth only. It is not the
# loop guard and cannot be: that conclusion never reaches the rollup today. It
# is here so that if GitHub ever does surface a withheld run, a run that has not
# run is still not a failure.
#
# CANCELLED, NEUTRAL, SKIPPED and STALE are terminal and are not evidence of a
# broken tree, so they count as neither failing nor pending.
checks_verdict() {
	jq -r '
		[ .[]?
		  | if .__typename == "StatusContext" then (.state // "PENDING")
		    elif .status == "COMPLETED" then (.conclusion // "PENDING")
		    else "PENDING" end
		] as $s
		| if ($s | length) == 0 then "NONE"
		  elif ($s | any(. == "FAILURE" or . == "ERROR" or . == "TIMED_OUT" or . == "STARTUP_FAILURE")) then "FAILING"
		  elif ($s | any(. == "PENDING" or . == "EXPECTED" or . == "ACTION_REQUIRED")) then "PENDING"
		  else "PASSING" end'
}

# checks_state PR_NUMBER - print the checks verdict for one PR. UNKNOWN when the
# rollup cannot be read, which no arm treats as stranded.
checks_state() {
	local pr="$1" rollup
	if ! rollup="$(gh pr view "$pr" --json statusCheckRollup --jq .statusCheckRollup 2>/dev/null)"; then
		printf 'UNKNOWN\n'
		return 0
	fi
	checks_verdict <<<"$rollup"
}

# rescue_verdict STATE BEHIND CHECKS - decide whether a PR is stranded. Prints
# `rescue <arm>` or `skip <reason>` and always exits 0, so the decision is a
# pure function of three readings and is testable without a live PR.
#
# Two arms, because a Dependabot branch strands two ways and they are different
# states wanting the same remedy:
#
#   conflicting     the branch cannot merge at all - Q427's original case.
#   behind-and-red  the branch merges cleanly, but it is behind the base branch
#                   and its checks are failing. A required gate whose verdict
#                   reads state that moves on its own fails a branch whose own
#                   tree is fine: release-pins-check compares the install pins
#                   against the newest stable tag, so tagging a release reddens
#                   every branch based before it. Nothing heals it - Dependabot
#                   disowned the branch when the sync commit landed (Q1118).
#
# Behind ALONE is not enough: a behind-but-green PR merges through the merge
# queue, which rebases it there, so force-pushing it would be noise nobody
# asked for. Red alone is not enough either: a bump that genuinely breaks the
# build is red on its own tree, and replaying it onto current main reproduces
# the same red.
#
# Requiring FAILING is also what stops this script rescuing a head it pushed
# itself: that head's checks are withheld, so checks_verdict reads NONE and this
# arm declines. The PR stays declined until a maintainer releases the runs and
# they fail on their own merits.
rescue_verdict() {
	local state="$1" behind="$2" checks="$3"
	[[ "$behind" =~ ^[0-9]+$ ]] || behind=0

	if [[ "$state" == "CONFLICTING" ]]; then
		printf 'rescue conflicting\n'
		return 0
	fi
	if [[ "$state" != "MERGEABLE" ]]; then
		printf 'skip mergeable state is %s\n' "$state"
		return 0
	fi
	if ((behind == 0)); then
		printf 'skip mergeable and level with %s\n' "$BASE_BRANCH"
		return 0
	fi
	if [[ "$checks" != "FAILING" ]]; then
		printf 'skip %s commits behind %s but its checks are %s, not failing\n' \
			"$behind" "$BASE_BRANCH" "$checks"
		return 0
	fi
	printf 'rescue behind-and-red\n'
}

# is_dependabot_author LOGIN - true when LOGIN is either spelling gh uses for
# the Dependabot app. Only for a PR *author*; a commit author is always the
# bracket form, which is what keeps the tip-author check below honest.
is_dependabot_author() {
	[[ "$1" == "$DEPENDABOT_LOGIN" || "$1" == "$DEPENDABOT_APP_LOGIN" ]]
}

# eligible PR_JSON - true when the PR is a Dependabot Go-module PR this script
# may rebase; otherwise print why it is skipped and return 1. Four gates:
#
#   1. Same-repo Dependabot gomod PR - never a human's, never a fork (a
#      GITHUB_TOKEN cannot push to a fork anyway).
#   2. Its tip commit is NOT authored by Dependabot. A branch Dependabot still
#      owns rebases itself, and racing that with a force-push would clobber the
#      bot mid-flight. Only a branch carrying the sync commit on top is
#      stranded - and that is exactly the branch whose tip is github-actions.
#   3. It is stranded - CONFLICTING, or mergeable-but-behind with failing
#      checks. rescue_verdict carries the reasoning for both arms; an UNKNOWN
#      mergeable state is left to the next run.
eligible() {
	local json="$1" number author head_ref tip_author state behind checks verdict reason
	number="$(jq -r .number <<<"$json")"
	author="$(jq -r .author.login <<<"$json")"
	head_ref="$(jq -r .headRefName <<<"$json")"

	if ! is_dependabot_author "$author"; then
		echo "  PR #$number: skip - not authored by $DEPENDABOT_LOGIN ($author)" >&2
		return 1
	fi
	if [[ "$(jq -r .isCrossRepository <<<"$json")" == "true" ]]; then
		echo "  PR #$number: skip - fork head, GITHUB_TOKEN cannot push to it" >&2
		return 1
	fi
	if [[ "$head_ref" != "$BRANCH_PREFIX"* ]]; then
		echo "  PR #$number: skip - not a Go-module branch ($head_ref)" >&2
		return 1
	fi
	tip_author="$(jq -r '.commits[-1].authors[0].login // ""' <<<"$json")"
	if [[ "$tip_author" == "$DEPENDABOT_LOGIN" ]]; then
		echo "  PR #$number: skip - branch tip is still Dependabot's; it rebases itself" >&2
		return 1
	fi
	state="$(mergeable_state "$number")"
	# Only the mergeable arm needs these two reads, and each is an API call, so a
	# conflicting PR still costs exactly what it did before.
	behind=0
	checks=NONE
	if [[ "$state" == "MERGEABLE" ]]; then
		behind="$(behind_by "$head_ref")"
		checks="$(checks_state "$number")"
	fi
	read -r verdict reason <<<"$(rescue_verdict "$state" "$behind" "$checks")"
	if [[ "$verdict" != "rescue" ]]; then
		echo "  PR #$number: skip - $reason" >&2
		return 1
	fi
	echo "  PR #$number: rescue - $reason ($state, $behind behind $BASE_BRANCH, checks $checks)" >&2
	return 0
}

# list_candidate_prs - print the number of every open PR whose author and branch
# look like a Dependabot Go-module bump. Cheap pre-filter; eligible() does the
# real work, including the mergeability poll.
list_candidate_prs() {
	gh pr list --state open --limit 100 --json number,headRefName,author |
		select_candidates
}

# select_candidates - read `gh pr list --json number,headRefName,author` output
# on stdin and print the number of every PR that looks like a Dependabot
# Go-module bump. Split out of list_candidate_prs so the filter can be tested
# against recorded gh output, which is the only half of the selection a unit
# test can reach: whether gh still spells the author the way this expects is a
# live question, and --list against the real repo is what answers it.
select_candidates() {
	jq -r --arg bot "$DEPENDABOT_LOGIN" --arg app "$DEPENDABOT_APP_LOGIN" \
		--arg prefix "$BRANCH_PREFIX" '
		.[]
		| select(.author.login == $bot or .author.login == $app)
		| select(.headRefName | startswith($prefix))
		| .number'
}

# ---------------------------------------------------------------------------
# Rebase
# ---------------------------------------------------------------------------

# apply_bump DIR MODULE WANT SCRATCH - re-apply one bump on top of the base
# branch. Writes an audit line to fd 3 and returns 0 when the bump landed, 1
# when it was deliberately skipped.
#
# The downgrade guard is Go's own: `go get` prints "go: downgraded <mod> vA =>
# vB" whenever minimal version selection has to move something backwards, so
# there is no hand-rolled semver comparison to get wrong, and transitive
# downgrades a direct comparison would miss are caught too. A bump that trips it
# is rolled back file-for-file and skipped.
apply_bump() {
	local dir="$1" module="$2" want="$3" scratch="$4" cur out
	cur="$(cd "$dir" && go mod edit -json | jq -r --arg m "$module" '.Require[]? | select(.Path == $m) | .Version')"
	if [[ -z "$cur" ]]; then
		echo "  - $module: skipped, $dir no longer requires it on $BASE_BRANCH" >&3
		return 1
	fi
	if [[ "$cur" == "$want" ]]; then
		echo "  - $module: skipped, $BASE_BRANCH is already at $want" >&3
		return 1
	fi
	cp "$dir/go.mod" "$scratch/go.mod.bak"
	cp "$dir/go.sum" "$scratch/go.sum.bak"
	if ! out="$( (cd "$dir" && go get "$module@$want") 2>&1 )"; then
		cp "$scratch/go.mod.bak" "$dir/go.mod"
		cp "$scratch/go.sum.bak" "$dir/go.sum"
		echo "  - $module: skipped, 'go get $module@$want' failed: $out" >&3
		return 1
	fi
	if grep -q ' downgraded ' <<<"$out"; then
		cp "$scratch/go.mod.bak" "$dir/go.mod"
		cp "$scratch/go.sum.bak" "$dir/go.sum"
		echo "  - $module: skipped, $want would downgrade $BASE_BRANCH's $cur" >&3
		return 1
	fi
	echo "  - $module: $cur => $want in $dir" >&3
	return 0
}

# rebase_pr PR_JSON SCRATCH - replay one PR's bumps onto the base branch and
# force-push the result. Pushes nothing when no bump is applicable.
rebase_pr() {
	local json="$1" scratch="$2"
	local number head_ref head_oid url base_sha subject line dir module want
	local applied=0 audit
	number="$(jq -r .number <<<"$json")"
	head_ref="$(jq -r .headRefName <<<"$json")"
	head_oid="$(jq -r .headRefOid <<<"$json")"
	url="$(jq -r .url <<<"$json")"
	audit="$scratch/audit-$number.txt"
	: >"$audit"

	step "PR #$number ($head_ref): replaying bumps onto $BASE_BRANCH"
	git fetch --quiet "$REMOTE" "$BASE_BRANCH" "$head_ref"
	base_sha="$(git merge-base "$REMOTE/$BASE_BRANCH" "$head_oid")"

	local bumps=()
	while read -r line; do
		[[ -n "$line" ]] && bumps+=("$line")
	done < <(collect_bumps "$base_sha" "$head_oid" "$scratch")
	if ((${#bumps[@]} == 0)); then
		echo "  no version bumps between $base_sha and $head_oid - leaving the PR alone"
		return 0
	fi

	# Dependabot's own subject stays the PR's story; the replay is explained in
	# the body. Take the OLDEST commit on the branch - the bot's - not the tip,
	# which is the vendor sync commit.
	subject="$(git log --reverse --format=%s "$base_sha..$head_oid" | head -1)"

	# --force discards the previous PR's replay residue; the targeted clean
	# drops vendor files it created that main does not track. Both are scoped to
	# generated trees, and the entry point refuses to run on a dirty tree.
	git checkout --quiet --force -B "$head_ref" "$REMOTE/$BASE_BRANCH"
	git clean -qfd -- vendor tools/vendor

	for line in "${bumps[@]}"; do
		read -r dir module want <<<"$line"
		if apply_bump "$dir" "$module" "$want" "$scratch" 3>>"$audit"; then
			applied=$((applied + 1))
		fi
	done

	echo "  bump audit:"
	cat "$audit"
	if ((applied == 0)); then
		echo "  every bump is already on $BASE_BRANCH or would downgrade it - the PR is obsolete, leaving it alone"
		return 0
	fi

	step "PR #$number: regenerating vendor trees, notices and generated output (make deps-sync)"
	make deps-sync

	if git diff --quiet "$REMOTE/$BASE_BRANCH" -- .; then
		echo "  the replay produced no diff against $BASE_BRANCH - the PR is obsolete, leaving it alone"
		return 0
	fi

	# Commit as github-actions[bot] - honest provenance for a GITHUB_TOKEN
	# commit, matching dependabot-go-sync.yml. Impersonating Dependabot would
	# not restore its ownership of the branch anyway: the pusher is
	# github-actions regardless of who the commit names as author.
	git config user.name 'github-actions[bot]'
	git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
	git add -A
	git commit --quiet --file - <<-EOF
		$subject

		Rebased onto $BASE_BRANCH by dependabot-rebase-stale.sh (Q427). Dependabot
		disowned this branch when the vendor sync commit landed on it, so it could
		not rebase itself once $BASE_BRANCH moved. The bumps were replayed on
		current $BASE_BRANCH with 'go get' and every generated artifact rebuilt
		with 'make deps-sync'; the conflicted tree was discarded rather than
		merged, because merging a stale Go bump can silently downgrade a module.

		Bump audit:
		$(cat "$audit")
	EOF

	if [[ "$DRY_RUN" == "true" ]]; then
		echo "  --dry-run: not pushing $(git rev-parse --short HEAD) to $REMOTE/$head_ref"
		return 0
	fi

	# --force-with-lease pinned to the head the eligibility check read: if
	# Dependabot or a human pushed in between, this run's view is stale and the
	# push must fail rather than clobber them.
	git push --force-with-lease="$head_ref:$head_oid" "$REMOTE" "HEAD:$head_ref"
	echo "  pushed $(git rev-parse --short HEAD) to $REMOTE/$head_ref"

	{
		echo "Rebased onto \`$BASE_BRANCH\` automatically (Q427). This branch could no longer rebase itself, because the vendor sync commit made Dependabot disown it."
		echo
		echo "The bumps were **replayed** on current \`$BASE_BRANCH\` and the vendor trees, \`go.work.sum\`, and \`THIRD-PARTY-NOTICES\` regenerated. The conflicted tree was discarded rather than merged: merging a stale Go bump can silently downgrade a module \`$BASE_BRANCH\` has moved forward."
		echo
		echo "Bump audit:"
		echo
		sed 's/^/    /' "$audit"
		echo
		echo "**The checks are withheld and need one approval.** The rebase was pushed with the workflow's \`GITHUB_TOKEN\`, so GitHub created this commit's runs and is holding them at \`action_required\`. Release them with **Approve and run** on the checks tab, which starts the runs already on this commit; closing and reopening queues a second full set instead, so it is the costlier click rather than a broken one."
	} >"$scratch/comment-$number.md"
	gh pr comment "$number" --body-file "$scratch/comment-$number.md" >/dev/null
	echo "  commented on $url"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

DRY_RUN=false
LIST_ONLY=false
requested=()
while (($#)); do
	case "$1" in
	--dry-run) DRY_RUN=true ;;
	--list) LIST_ONLY=true ;;
	--bumps)
		[[ $# -ge 3 ]] || die "--bumps needs BASE_GO_MOD and TIP_GO_MOD"
		require_cmd go https://go.dev/dl/
		require_cmd jq https://jqlang.github.io/jq/download/
		bumps_between "$2" "$3"
		exit 0
		;;
	--select)
		require_cmd jq https://jqlang.github.io/jq/download/
		select_candidates
		exit 0
		;;
	--checks)
		require_cmd jq https://jqlang.github.io/jq/download/
		checks_verdict
		exit 0
		;;
	--verdict)
		[[ $# -ge 4 ]] || die "--verdict needs STATE BEHIND CHECKS"
		rescue_verdict "$2" "$3" "$4"
		exit 0
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*) die "unknown flag: $1 (try --help)" ;;
	*) requested+=("$1") ;;
	esac
	shift
done

require_cmd gh https://cli.github.com/
require_cmd jq https://jqlang.github.io/jq/download/
require_cmd go https://go.dev/dl/

if ((${#requested[@]} == 0)); then
	while read -r n; do
		[[ -n "$n" ]] && requested+=("$n")
	done < <(list_candidate_prs)
fi

if ((${#requested[@]} == 0)); then
	echo "No open Dependabot Go-module PRs." >&2
	exit 0
fi

eligible_prs=()
for pr in "${requested[@]}"; do
	if eligible "$(pr_json "$pr")"; then
		eligible_prs+=("$pr")
	fi
done

if [[ "$LIST_ONLY" == "true" ]]; then
	((${#eligible_prs[@]} > 0)) && printf '%s\n' "${eligible_prs[@]}"
	exit 0
fi

if ((${#eligible_prs[@]} == 0)); then
	echo "No stranded Dependabot Go-module PR needs a rebase." >&2
	exit 0
fi

# The replay resets the working tree to the base branch, so refuse to eat
# someone's uncommitted work when this is run by hand.
[[ -z "$(git status --porcelain)" ]] ||
	die "working tree is dirty - the replay resets it to $REMOTE/$BASE_BRANCH. Commit or stash first."
original_ref="$(git rev-parse --abbrev-ref HEAD)"

SCRATCH="$REPO_ROOT/tmp/dependabot-rebase-stale.$$"
mkdir -p "$SCRATCH"
cleanup() {
	rm -rf "$SCRATCH"
	git checkout --quiet "$original_ref" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

cd "$REPO_ROOT"
processed=0
for pr in "${eligible_prs[@]}"; do
	if ((processed >= MAX_PRS)); then
		echo
		echo "Deferring PR #$pr: this run's cap of MAX_PRS=$MAX_PRS is reached." \
			"The next $BASE_BRANCH push or the daily schedule picks it up."
		continue
	fi
	rebase_pr "$(pr_json "$pr")" "$SCRATCH"
	processed=$((processed + 1))
done
