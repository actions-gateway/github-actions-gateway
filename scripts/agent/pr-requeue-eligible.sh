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
#      repo's merge drivers own (docs/STATUS.md, docs/plan/README.md,
#      docs/roadmap.md). A conflict in code, tests or workflows is a human's
#      to read, because the rebase changes what the maintainer approved.
#      Both sides of that merge are named by the PR, not by the checkout: the
#      head comes from `headRefOid` (Q834). Read as local `git rev-parse HEAD`
#      it answered about whatever the caller happened to have checked out, so
#      --assess on two different PRs from one worktree returned the same
#      verdict, and a checkout that merges clean reported ELIGIBLE for a PR
#      whose own head conflicts in code.
#      Backlog rows are the exception the maintainer signed off on:
#      `status-isolation-check` keeps them in their own commit, so the reviewed
#      code is untouched by that resolution, and lint-backlog/roadmap-check
#      gate the result.
#
# Check 4 has to run BEFORE the rebase, but the enqueue happens after it and
# after CI. So `--assess` records its verdict and `--confirm` reads it back:
# a missing or stale record fails closed to "wake the maintainer", which is the
# safe direction when a session loses context mid-flight.
#
# That assessment is also the only contemporaneous record of *why* the queue
# evicted a PR (Q810). The rebase heals the branch, and the same probe against
# current refs then reports a clean merge, so a later read can neither confirm
# nor refute what the worker measured. `--assess` therefore records the two
# commits it merged, not just its verdict: `git merge-tree --write-tree
# <base_oid> <head_oid>` re-derives the conflict set from those objects at any
# later time, whatever the branches have since become. The record stays in the
# gitignored tmp/ tree — session-local evidence, not a registry to reconcile.
#
# Every probe has to tell a measured negative from a read that never happened,
# because a failed `gh` leaves an empty string and every check reads emptiness
# as an answer: no state as "not OPEN", no queue entry as "not queued", no
# timeline as "nobody enqueued it". The verdict then carries a reason no read
# supports, and that reason is the only account of the eviction that outlives
# the rebase. So an unmeasurable probe exits 2 rather than deciding (Q805).
#
# Usage:
#   pr-requeue-eligible.sh --assess  <pr>   # before rebasing; records a verdict
#   pr-requeue-eligible.sh --confirm <pr>   # after CI is green; gates the enqueue
# Options (for the test suite; all default to the real thing):
#   --state-dir PATH   where verdicts are recorded (default tmp/requeue)
#   --repo OWNER/NAME  the repo to query
# Exit: 0 eligible, 1 not eligible (reason on stdout), 2 usage error or a probe
# that could not run (reason on stderr).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# The files whose rebase conflicts a merge driver resolves by row ID rather than
# by line position. Keep in step with .gitattributes: a file listed there with a
# `merge=` attribute belongs here, and nothing else does.
DRIVER_OWNED=(
	docs/STATUS.md
	docs/plan/README.md
	docs/roadmap.md
	mk/gate-lists.mk
	scripts/README.md
)

# The driver names behind those paths, switched off for the merge probe below.
# Every `merge=` value in .gitattributes belongs here: a name missing from this
# list stays live in a clone that ran `make merge-driver`, quietly resolves its
# file inside the probe, and drops it from the measured conflict set.
DRIVER_NAMES=(
	backlog
	gatelists
	planindex
	roadmap
	scriptindex
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

# The commits the probe merged. Empty until it runs, so a refusal that
# short-circuits ahead of it records a `-` rather than a stale OID.
PROBE_BASE_OID=""
PROBE_HEAD_OID=""

# record VERDICT REASON [CONFLICT_PATH...] — append the assessment as one
# key/value per line, so `--confirm` can read it back and a human can read it
# at all. A record starts at its `verdict` line and runs to the next one.
#
# Appended rather than overwritten: a second assessment that refuses before it
# probes would otherwise erase the measurement the first one took, which is the
# eviction's only evidence. `--confirm` reads the last record, so the fail-closed
# reading of the current state is unchanged by keeping the earlier ones.
record() {
	local verdict="$1" reason="$2" path
	shift 2
	mkdir -p "$STATE_DIR"
	{
		printf 'verdict %s\n' "$verdict"
		printf 'at %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		printf 'base %s\n' "${base:--}"
		printf 'base_oid %s\n' "${PROBE_BASE_OID:--}"
		printf 'head_oid %s\n' "${PROBE_HEAD_OID:--}"
		printf 'reason %s\n' "$reason"
		for path in "$@"; do
			printf 'conflict %s\n' "$path"
		done
	} >>"$VERDICT_FILE"
}

# wake REASON [CONFLICT_PATH...] — not eligible. Always exit 1; the caller hands
# back to a human. The refusal is recorded too: an absent file cannot tell "the
# assessment never ran" from "it ran and refused", and both `--confirm` and
# whoever reads the eviction afterwards need that apart.
wake() {
	printf 'WAKE: %s\n' "$1"
	if [[ "$MODE" == "assess" ]]; then
		record WAKE "$@"
	fi
	exit 1
}

# unmeasurable WHAT — the probe could not run, so there is no answer to report.
# Exit 2, matching the merge probe: 1 means a check was made and refused, and a
# refusal is a record of why. Reporting an unread probe as a refusal writes a
# reason nothing measured, which the rebase then makes unfalsifiable.
#
# Recorded like a refusal, for the reason wake() records one: an absent file
# would say the assessment never ran. The first read happens before the base is
# known, so a record taken from it carries `-` there.
unmeasurable() {
	printf 'pr-requeue-eligible.sh: could not measure %s; refusing to guess\n' "$1" >&2
	if [[ "$MODE" == "assess" ]]; then
		record UNMEASURABLE "$1"
	fi
	exit 2
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
# the GraphQL one; mergeQueueEntry is null for a PR that is not queued.
#
# gh's --jq prints *nothing* for a JSON null, where `jq -r` prints the string
# (measured against a live unqueued PR), so the answer this read gives for a PR
# outside the queue is indistinguishable from the answer it gives when it never
# ran. The PR's node id is therefore selected alongside the state: a successful
# read always carries one, the way merge-tree's tree OID marks a merge that
# actually happened below.
in_queue() {
	local owner_repo answer node_id queue_state
	if [[ -n "$REPO" ]]; then
		owner_repo="$REPO"
	else
		owner_repo="$(gh repo view --json nameWithOwner --jq .nameWithOwner)" ||
			unmeasurable "which repo PR $PR belongs to"
	fi
	# shellcheck disable=SC2016 # $owner/$name/$pr are GraphQL variables, not shell
	answer=$(gh api graphql \
		-f owner="${owner_repo%%/*}" -f name="${owner_repo#*/}" -F pr="$PR" \
		-f query='query($owner:String!,$name:String!,$pr:Int!){
			repository(owner:$owner,name:$name){
				pullRequest(number:$pr){ id mergeQueueEntry { state } }
			}
		}' --jq '.data.repository.pullRequest
			| "\(.id // "-") \(.mergeQueueEntry.state // "none")"') ||
		unmeasurable "whether PR $PR is in the merge queue"
	node_id="${answer%% *}"
	queue_state="${answer#* }"
	[[ "$answer" == *" "* && -n "$node_id" && "$node_id" != "-" ]] ||
		unmeasurable "whether PR $PR is in the merge queue: the query answered '$answer'"
	[[ "$queue_state" != "none" ]]
}

# human_enqueued — true when a non-bot actor has added this PR to the queue.
# GitHub marks app actors with type "Bot"; the merge queue's own bot removals
# are irrelevant here, only additions count.
#
# `--paginate` runs the --jq filter per page and prints one count per page
# (measured: a 290-event timeline answers "100 100 90"), so the counts are
# summed rather than read as a number. Unsummed, a PR past 100 timeline events
# fed `((n > 0))` a multi-line value, which is an arithmetic syntax error and
# reports as "no human enqueued this PR" on a healthy network.
human_enqueued() {
	local pages n
	pages=$(gh_api "issues/$PR/timeline" --paginate \
		--jq '[.[] | select(.event == "added_to_merge_queue")
		        | select((.actor.type // "User") != "Bot")] | length') ||
		unmeasurable "whether a human enqueued PR $PR"
	# An unread timeline leaves this empty, and empty is 0 in arithmetic, which
	# is the same answer as a PR nobody ever enqueued.
	[[ "$pages" =~ ^[0-9]+([[:space:]]+[0-9]+)*$ ]] ||
		unmeasurable "whether a human enqueued PR $PR: the timeline read answered '$pages'"
	n=$(awk '{ total += $1 } END { print total + 0 }' <<<"$pages")
	((n > 0))
}

# resolve_probe_commits BASE HEAD_OID — pin both sides of the probe to an OID,
# so what gets recorded is re-runnable: a ref pair is not a measurement anyone
# can repeat, because `origin/<base>` and the PR's head have both moved by the
# time the question is asked. merge-tree exits 1 both when it finds conflicts
# and when a ref does not resolve ("origin/x - not something we can merge"), so
# both sides are verified here rather than left to the probe's status.
#
# HEAD_OID is the PR's, so the answer does not depend on the checkout the caller
# runs from (Q834). That commit need not be local — a dispatcher assessing a
# worker's PR has never had the branch — so an absent one is fetched from the
# pull ref before it is called missing.
#
# Separate from conflicting_paths because that runs in a command substitution,
# where an assignment to either global would die with the subshell.
resolve_probe_commits() {
	if ! PROBE_BASE_OID="$(git rev-parse --verify --quiet "$1^{commit}")"; then
		printf 'pr-requeue-eligible.sh: %s does not resolve to a commit\n' "$1" >&2
		return 2
	fi
	if ! git rev-parse --verify --quiet "$2^{commit}" >/dev/null 2>&1; then
		git fetch origin --quiet "refs/pull/$PR/head" 2>/dev/null || true
	fi
	if ! PROBE_HEAD_OID="$(git rev-parse --verify --quiet "$2^{commit}")"; then
		printf 'pr-requeue-eligible.sh: PR %s head %s is not in this clone and refs/pull/%s/head did not fetch it\n' \
			"$PR" "$2" "$PR" >&2
		return 2
	fi
}

# conflicting_paths — the driver-owned files this merge cannot resolve without
# a driver, plus every other file that genuinely conflicts.
#
# Each declared driver is replaced by a failing command, not unset: `-c
# merge.<name>.driver=false` runs /usr/bin/false, so git records a conflict for
# any driver-owned path both sides touched rather than attempting the built-in
# merge. That is deliberate here and it is not a driverless merge — `-c` cannot
# express one (measured 2026-08-12; parallel-dispatch.md#conflict-policy). It
# errs toward reporting: a driver left live resolves its own file inside the
# probe and drops a real conflict from the record, which is the direction that
# cannot be detected afterwards. Every such path is discounted by
# is_driver_owned, so the verdict does not turn on either error.
#
# A probe that never ran yields no CONFLICT lines, which reads as a clean merge
# and hands back ELIGIBLE. The output is therefore required to open with the
# merged tree's OID, which merge-tree prints only when it actually merged.
conflicting_paths() {
	local out rc=0 name off=()
	for name in "${DRIVER_NAMES[@]}"; do
		off+=(-c "merge.$name.driver=false")
	done
	out=$(git "${off[@]}" \
		merge-tree --write-tree "$PROBE_BASE_OID" "$PROBE_HEAD_OID" 2>&1) || rc=$?
	if ((rc > 1)) || [[ ! "$(head -n 1 <<<"$out")" =~ ^[0-9a-f]{40}$ ]]; then
		printf 'pr-requeue-eligible.sh: merge-tree of %s into %s did not run (rc=%s): %s\n' \
			"$PROBE_BASE_OID" "$PROBE_HEAD_OID" "$rc" "$out" >&2
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

# A `read` takes the status of the read, not of the substitution feeding it, so
# a failed `gh pr view` leaves every field empty and the first check below
# refuses with "the PR is , not OPEN", a verdict on a PR nobody looked at.
# Measured 2026-08-11 under a transient TLS failure.
pr_fields=$(gh_pr --json state,isDraft,baseRefName,headRefOid \
	--jq '[.state, (.isDraft|tostring), .baseRefName, .headRefOid] | @tsv') ||
	unmeasurable "PR $PR's state"
read -r state is_draft base head_oid <<<"$pr_fields"
[[ -n "$state" && -n "$is_draft" && -n "$base" && -n "$head_oid" ]] ||
	unmeasurable "PR $PR's state: the read answered '$pr_fields'"
# Shape-checked rather than left to rev-parse: an OID the read mangled would
# otherwise surface as "the conflict set is unmeasurable", which names the probe
# instead of the read that broke it.
[[ "$head_oid" =~ ^[0-9a-f]{40}$ ]] ||
	unmeasurable "PR $PR's head commit: the read answered '$head_oid'"

[[ "$state" == "OPEN" ]] || wake "the PR is $state, not OPEN"
[[ "$is_draft" == "false" ]] || wake "the PR is a draft"

if [[ "$MODE" == "confirm" ]]; then
	[[ -f "$VERDICT_FILE" ]] ||
		wake "no recorded assessment for PR $PR; re-enqueue only follows an --assess that ran before the rebase"
	# Last record wins: the file accumulates every assessment, and only the most
	# recent one describes the state a re-enqueue would act on.
	recorded_verdict="" recorded_base="" recorded_base_oid="-" recorded_head_oid="-"
	recorded_conflicts=()
	while read -r key value; do
		case "$key" in
		verdict)
			recorded_verdict="$value"
			recorded_base="" recorded_base_oid="-" recorded_head_oid="-"
			recorded_conflicts=()
			;;
		base) recorded_base="$value" ;;
		base_oid) recorded_base_oid="$value" ;;
		head_oid) recorded_head_oid="$value" ;;
		conflict) recorded_conflicts+=("$value") ;;
		esac
	done <"$VERDICT_FILE"
	[[ "$recorded_verdict" == "ELIGIBLE" ]] ||
		wake "the recorded assessment was '$recorded_verdict', not ELIGIBLE"
	[[ "$recorded_base" == "$base" ]] ||
		wake "the PR's base changed from $recorded_base to $base since the assessment"
	in_queue && wake "the PR is already in the merge queue; nothing to restore"
	human_enqueued || wake "no human has enqueued this PR, so there is nothing to restore"
	printf "ELIGIBLE: re-enqueue restores the maintainer's own earlier enqueue\n"
	# Repeat the assessment's measurement so the transcript that carries the
	# enqueue also carries what it rests on, rebase or no rebase since.
	printf 'measured: git merge-tree --write-tree %s %s\n' "$recorded_base_oid" "$recorded_head_oid"
	printf 'conflicts: %s\n' "${recorded_conflicts[*]:-none}"
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
resolve_probe_commits "origin/$base" "$head_oid" || probe_rc=$?
if ((probe_rc == 0)); then
	probed="$(conflicting_paths)" || probe_rc=$?
fi
if ((probe_rc != 0)); then
	unmeasurable "the conflict set"
fi

conflicts=()
not_owned=()
while IFS= read -r path; do
	[[ -n "$path" ]] || continue
	conflicts+=("$path")
	is_driver_owned "$path" || not_owned+=("$path")
done <<<"$probed"

# Printed before the verdict, so the wake that reports the conflict carries the
# re-runnable measurement whichever way it goes.
printf 'measured: git merge-tree --write-tree %s %s\n' "$PROBE_BASE_OID" "$PROBE_HEAD_OID"
printf 'conflicts: %s\n' "${conflicts[*]:-none}"

if ((${#not_owned[@]} > 0)); then
	wake "the rebase resolves conflicts outside the merge-driver-owned files: ${not_owned[*]}" \
		"${conflicts[@]}"
fi

if ((${#conflicts[@]} > 0)); then
	record ELIGIBLE "conflicts confined to merge-driver-owned files" "${conflicts[@]}"
	printf 'ELIGIBLE: conflicts confined to merge-driver-owned files (%s)\n' "${conflicts[*]}"
else
	record ELIGIBLE "the rebase resolves no conflicts at all"
	printf 'ELIGIBLE: the rebase resolves no conflicts at all\n'
fi
