#!/usr/bin/env bash
#
# pr-requeue-eligible.sh — decide whether a session may re-enqueue an open PR
# that the merge queue evicted, without waking the maintainer (Q692).
#
# The maintainer reviews and enqueues; no agent does either. This is the one
# carve-out: restoring a state the maintainer already chose, after the queue
# dropped the PR for a reason that has nothing to do with the change. It never
# performs a *first* enqueue, so review still gates every merge.
#
# Why a script instead of a rule in the session prompt: the decision is four
# checks against two APIs and a driver-off merge probe, re-derived on every
# eviction. Prose gets it wrong silently, and the failure mode is an unattended
# merge of something nobody read.
#
# Eligible means all of:
#   1. The PR is OPEN and not a draft.
#   2. A human enqueued it before — `added_to_merge_queue` by a non-bot actor.
#      This is what makes a re-enqueue a restoration rather than a decision.
#   3. It is not in the queue right now, so this cannot double-enqueue.
#   4. The rebase it is about to take resolves conflicts ONLY in the files the
#      repo's merge drivers own (docs/STATUS.md, docs/plan/README.md). A
#      conflict in code, tests or workflows is a human's to read, because the
#      rebase changes what the maintainer approved. Backlog rows are the
#      exception the maintainer signed off on: `status-isolation-check` keeps
#      them in their own commit, so the reviewed code is untouched by that
#      resolution, and lint-backlog/roadmap-check gate the result.
#
# Check 4 has to run BEFORE the rebase, but the enqueue happens after it and
# after CI. So `--assess` records its verdict and `--confirm` reads it back:
# a missing or stale record fails closed to "wake the maintainer", which is the
# safe direction when a session loses context mid-flight.
#
# Usage:
#   pr-requeue-eligible.sh --assess  <pr>   # before rebasing; records a verdict
#   pr-requeue-eligible.sh --confirm <pr>   # after CI is green; gates the enqueue
# Options (for the test suite; all default to the real thing):
#   --state-dir PATH   where verdicts are recorded (default tmp/requeue)
#   --repo OWNER/NAME  the repo to query
# Exit: 0 eligible, 1 not eligible (reason on stdout), 2 usage error.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# The files whose rebase conflicts a merge driver resolves by row ID rather than
# by line position. Keep in step with .gitattributes: a file listed there with a
# `merge=` attribute belongs here, and nothing else does.
DRIVER_OWNED=(
	docs/STATUS.md
	docs/plan/README.md
)

STATE_DIR="tmp/requeue"
REPO=""
MODE=""
PR=""

while (($# > 0)); do
	case "$1" in
	--assess | --confirm)
		MODE="${1#--}"
		;;
	--state-dir)
		STATE_DIR="$2"
		shift
		;;
	--repo)
		REPO="$2"
		shift
		;;
	-*)
		printf 'pr-requeue-eligible.sh: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	*)
		PR="$1"
		;;
	esac
	shift
done

if [[ -z "$MODE" ]]; then
	printf 'pr-requeue-eligible.sh: one of --assess or --confirm is required\n' >&2
	exit 2
fi
if [[ ! "$PR" =~ ^[0-9]+$ ]]; then
	printf 'pr-requeue-eligible.sh: a numeric PR number is required\n' >&2
	exit 2
fi

VERDICT_FILE="$STATE_DIR/$PR.verdict"

# wake REASON — not eligible. Always exit 1; the caller hands back to a human.
wake() {
	printf 'WAKE: %s\n' "$1"
	exit 1
}

gh_pr() {
	if [[ -n "$REPO" ]]; then
		gh pr view "$PR" --repo "$REPO" "$@"
	else
		gh pr view "$PR" "$@"
	fi
}

gh_api() {
	if [[ -n "$REPO" ]]; then
		gh api "repos/$REPO/$1" "${@:2}"
	else
		gh api "repos/{owner}/{repo}/$1" "${@:2}"
	fi
}

# in_queue — true when GitHub reports a live merge-queue entry. `gh pr view`
# exposes no queue field at all (checked against gh's --json list), so this is
# the GraphQL one; it is null for a PR that is not queued.
in_queue() {
	local owner_repo entry
	owner_repo="${REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"
	# shellcheck disable=SC2016 # $owner/$name/$pr are GraphQL variables, not shell
	entry=$(gh api graphql \
		-f owner="${owner_repo%%/*}" -f name="${owner_repo#*/}" -F pr="$PR" \
		-f query='query($owner:String!,$name:String!,$pr:Int!){
			repository(owner:$owner,name:$name){
				pullRequest(number:$pr){ mergeQueueEntry { state } }
			}
		}' --jq '.data.repository.pullRequest.mergeQueueEntry')
	[[ -n "$entry" && "$entry" != "null" ]]
}

# human_enqueued — true when a non-bot actor has added this PR to the queue.
# GitHub marks app actors with type "Bot"; the merge queue's own bot removals
# are irrelevant here, only additions count.
human_enqueued() {
	local n
	n=$(gh_api "issues/$PR/timeline" --paginate \
		--jq '[.[] | select(.event == "added_to_merge_queue")
		        | select((.actor.type // "User") != "Bot")] | length')
	((n > 0))
}

# conflicting_paths BASE — the files that conflict merging BASE into HEAD with
# the repo's merge drivers disabled. Disabled deliberately: the drivers are
# per-clone config that GitHub's servers never run, so this measures what the
# merge queue will see, not what this clone can quietly resolve.
#
# merge-tree exits 1 both when it finds conflicts and when a ref does not
# resolve ("origin/x - not something we can merge"), so its status cannot tell a
# real result from a probe that never ran — and a probe that never ran yields no
# CONFLICT lines, which reads as a clean merge and hands back ELIGIBLE. The ref
# is therefore verified first, and the output is required to open with the
# merged tree's OID, which merge-tree prints only when it actually merged.
conflicting_paths() {
	local out rc=0
	if ! git rev-parse --verify --quiet "$1^{commit}" >/dev/null; then
		printf 'pr-requeue-eligible.sh: %s does not resolve to a commit\n' "$1" >&2
		return 2
	fi
	out=$(git -c merge.backlog.driver=false -c merge.planindex.driver=false \
		merge-tree --write-tree "$1" HEAD 2>&1) || rc=$?
	if ((rc > 1)) || [[ ! "$(head -n 1 <<<"$out")" =~ ^[0-9a-f]{40}$ ]]; then
		printf 'pr-requeue-eligible.sh: merge-tree against %s did not run (rc=%s): %s\n' \
			"$1" "$rc" "$out" >&2
		return 2
	fi
	awk '/^CONFLICT/ { if (match($0, / in .+$/)) print substr($0, RSTART + 4) }' <<<"$out" |
		sort -u
}

is_driver_owned() {
	local path candidate
	path="$1"
	for candidate in "${DRIVER_OWNED[@]}"; do
		[[ "$path" == "$candidate" ]] && return 0
	done
	return 1
}

read -r state is_draft base <<<"$(gh_pr --json state,isDraft,baseRefName \
	--jq '[.state, (.isDraft|tostring), .baseRefName] | @tsv')"

[[ "$state" == "OPEN" ]] || wake "the PR is $state, not OPEN"
[[ "$is_draft" == "false" ]] || wake "the PR is a draft"

if [[ "$MODE" == "confirm" ]]; then
	[[ -f "$VERDICT_FILE" ]] ||
		wake "no recorded assessment for PR $PR; re-enqueue only follows an --assess that ran before the rebase"
	read -r recorded_verdict recorded_base <"$VERDICT_FILE"
	[[ "$recorded_verdict" == "ELIGIBLE" ]] ||
		wake "the recorded assessment was '$recorded_verdict', not ELIGIBLE"
	[[ "$recorded_base" == "$base" ]] ||
		wake "the PR's base changed from $recorded_base to $base since the assessment"
	in_queue && wake "the PR is already in the merge queue; nothing to restore"
	human_enqueued || wake "no human has enqueued this PR, so there is nothing to restore"
	printf "ELIGIBLE: re-enqueue restores the maintainer's own earlier enqueue\n"
	exit 0
fi

# --assess. Ordered cheapest-first so a common ineligibility does not pay for a
# paginated timeline read or a merge probe.
in_queue && wake "the PR is already in the merge queue; nothing to restore"
human_enqueued || wake "no human has enqueued this PR, so a re-enqueue would be a first enqueue"

# Best-effort refresh. A failure here is not fatal on its own — the ref may
# already be local — so the merge probe below stays the single place a base that
# cannot be resolved is reported, rather than a raw `git fetch` fatal.
git fetch origin "$base" --quiet 2>/dev/null || true
probe_rc=0
conflicts="$(conflicting_paths "origin/$base")" || probe_rc=$?
if ((probe_rc != 0)); then
	printf 'pr-requeue-eligible.sh: could not measure the conflict set; refusing to guess\n' >&2
	exit 2
fi

not_owned=""
while IFS= read -r path; do
	[[ -n "$path" ]] || continue
	is_driver_owned "$path" || not_owned+="$path "
done <<<"$conflicts"

if [[ -n "$not_owned" ]]; then
	mkdir -p "$STATE_DIR"
	printf 'WAKE %s\n' "$base" >"$VERDICT_FILE"
	wake "the rebase resolves conflicts outside the merge-driver-owned files: ${not_owned% }"
fi

mkdir -p "$STATE_DIR"
printf 'ELIGIBLE %s\n' "$base" >"$VERDICT_FILE"
if [[ -n "$conflicts" ]]; then
	joined="$(tr '\n' ' ' <<<"$conflicts")"
	printf 'ELIGIBLE: conflicts confined to merge-driver-owned files (%s)\n' "${joined% }"
else
	printf 'ELIGIBLE: the rebase resolves no conflicts at all\n'
fi
