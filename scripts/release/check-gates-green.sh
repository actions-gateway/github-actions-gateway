#!/usr/bin/env bash
# check-gates-green.sh — did the release gates actually run, and pass, on this commit?
#
#   scripts/release/check-gates-green.sh [ref]        # default: origin/main
#
# Exit 0 when every required gate ran in full and succeeded for the commit, 1 when
# one failed, is missing, or path-skipped its heavy job, 2 on a usage or `gh` error.
#
# Ask by commit, never by branch. `gh run list --branch main` filters to runs whose
# head *branch* is main, which excludes merge-queue runs — and the merge queue is
# where a commit is validated before it lands. A push-lane run for the same commit
# sits behind the previous push's per-ref concurrency group and can read `pending`
# for a long time while the identical tree is already green from the queue. Reading
# only the push lane says "not validated" about a commit that is (measured
# 2026-08-15, twice in one release cut). So a gate passes when *some* lane
# succeeded on this SHA, and the lane is reported so the reader can see which one
# answered.
#
# A successful run is NOT a gate that ran. Every gate workflow here ends in a
# lightweight `<name>-gate` job carrying `if: always()`, so when the heavy job is
# path-skipped the run still completes `success` — a skipped job does not fail its
# run. Measured 2026-08-15 on 050f1e80e: all nine required workflows reported
# success and all nine had skipped their real job, so a run-level check called a
# commit fully validated when nothing had run on it. This reads job conclusions
# for that reason, and a skip is reported rather than swallowed.
#
# A skip is not automatically a failure — it is the normal shape of a docs-only
# commit sitting on top of a validated one. It is not automatically fine either,
# so it exits non-zero and names the remedy: prove the released surface is
# unchanged since a commit that did run, with check-artifact-unchanged.sh.
#
# The gate list is derived from the repository's own branch ruleset, so a newly
# required check is covered with nothing to maintain here. A context that does not
# map to a workflow is reported as unmapped and counted against the run, never
# dropped: silently under-covering is the failure this script exists to prevent.
set -euo pipefail
shopt -s inherit_errexit

# Used only when the ruleset cannot be read (no permission, or offline). Kept
# deliberately short: it is a degraded mode that says so, not a second source of
# truth to maintain alongside the ruleset.
FALLBACK_WORKFLOWS=(unit-test.yml integration-test.yml security-scan.yml e2e-test.yml e2e-calico.yml)

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") [ref]

		  ref  commit to check (default: origin/main)
	EOF
	exit 2
}

# required_contexts — the required status check contexts across every ruleset that
# targets the default branch. Prints one per line; empty output means the rulesets
# could not be read, which callers treat as "fall back", never as "none required".
required_contexts() {
	local ids id
	ids="$(gh api 'repos/{owner}/{repo}/rulesets' --jq '.[].id' 2>/dev/null || true)"
	[[ -n "$ids" ]] || return 0
	for id in $ids; do
		gh api "repos/{owner}/{repo}/rulesets/${id}" \
			--jq '.rules[]? | select(.type == "required_status_checks")
			      | .parameters.required_status_checks[].context' 2>/dev/null || true
	done | sort -u
}

# workflow_for CONTEXT — the workflow file defining the job that reports CONTEXT.
#
# Found by locating the job key, not by a name convention: a check's context is its
# job id, and the two need not share a stem. `e2e-gate` lives in `e2e-test.yml`, so
# deriving the filename from the context would drop the e2e lane — measured here
# when it did. Prints nothing when no workflow declares the job; the caller
# surfaces that rather than skipping it.
workflow_for() {
	local ctx="$1" f
	for f in "${REPO_ROOT}"/.github/workflows/*.yml; do
		# Job keys sit at two-space indent under `jobs:`; anchor to that so a
		# `with:` or `env:` entry sharing the name cannot match.
		if grep -qE "^  ${ctx}:[[:space:]]*$" "$f"; then
			basename "$f"
			return 0
		fi
	done
}

# successful_runs RUNS_JSON — every successful run, newest first, as "event<TAB>id"
# lines. Empty when no lane succeeded.
#
# All of them, not just the newest: the lanes disagree about what they execute. A
# `merge_group` run path-gates its heavy job, while the `push` lane for the same
# commit runs unconditionally, so one lane can skip what another ran in full. The
# caller prefers a lane that ran in full instead of trusting list order — today the
# push run happens to sort first, which would make the right answer an accident.
successful_runs() {
	printf '%s' "$1" | jq -r '.[] | select(.conclusion == "success") | "\(.event)\t\(.databaseId)"'
}

# run_state RUNS_JSON — a human-readable summary of why no lane succeeded.
run_state() {
	printf '%s' "$1" | jq -r 'if length == 0 then "no run found for this commit"
		else (map("\(.event):\(.status)/\(.conclusion // "-")") | join("  ")) end'
}

# skipped_jobs JOBS_JSON — comma-joined names of jobs that were skipped, empty when
# the run executed in full.
skipped_jobs() {
	printf '%s' "$1" | jq -r '[.jobs[] | select(.conclusion == "skipped") | .name] | join(", ")'
}

# Stop here when sourced for its helpers, so the test drives them offline instead
# of through the network.
if [[ "${CHECK_GATES_GREEN_LIB:-}" == "1" ]]; then
	# shellcheck disable=SC2317  # reached only via `source`, which shellcheck cannot see
	return 0
fi

[[ $# -le 1 ]] || usage
[[ "${1:-}" == -h || "${1:-}" == --help ]] && usage

ref="${1:-origin/main}"
if ! sha="$(git rev-parse --verify --quiet "${ref}^{commit}")"; then
	echo "check-gates-green: not a commit: ${ref}" >&2
	exit 2
fi
short="${sha:0:9}"

contexts="$(required_contexts)"
workflows=()
unmapped=()
if [[ -n "$contexts" ]]; then
	while IFS= read -r ctx; do
		[[ -n "$ctx" ]] || continue
		wf="$(workflow_for "$ctx")"
		if [[ -n "$wf" ]]; then
			workflows+=("$wf")
		else
			unmapped+=("$ctx")
		fi
	done <<<"$contexts"
	source_note="${#workflows[@]} required check(s) from the branch ruleset"
else
	workflows=("${FALLBACK_WORKFLOWS[@]}")
	source_note="${#workflows[@]} fallback workflow(s) — the ruleset could not be read, so this list may under-cover"
fi

echo "check-gates-green: ${short} (${ref})"
echo "  gates: ${source_note}"

failed=0
skipped=0
for wf in "${workflows[@]}"; do
	runs="$(gh run list --workflow="$wf" --commit "$sha" -L 20 \
		--json event,status,conclusion,databaseId 2>/dev/null || echo '[]')"

	if [[ -z "$(successful_runs "$runs")" ]]; then
		printf '  %-22s NOT GREEN — %s\n' "$wf" "$(run_state "$runs")"
		failed=$((failed + 1))
		continue
	fi

	# Take the first lane that ran in full; fall back to reporting the skips of
	# the last one examined, so a wholly path-gated commit still says what it is.
	full_event=""
	skip_event=""
	skip_names=""
	while IFS=$'\t' read -r event run_id; do
		[[ -n "$run_id" ]] || continue
		# per_page over --paginate: paginating emits one JSON document per page, and
		# the jq below would then run once per page and print a line each. 100
		# comfortably covers the largest gate workflow (unit-test.yml, 13 jobs).
		jobs="$(gh api "repos/{owner}/{repo}/actions/runs/${run_id}/jobs?per_page=100" 2>/dev/null || echo '{"jobs":[]}')"
		names="$(skipped_jobs "$jobs")"
		if [[ -z "$names" ]]; then
			full_event="$event"
			break
		fi
		skip_event="$event"
		skip_names="$names"
	done < <(successful_runs "$runs")

	if [[ -n "$full_event" ]]; then
		printf '  %-22s ok (%s)\n' "$wf" "$full_event"
		continue
	fi
	printf '  %-22s SKIPPED (%s) — did not run: %s\n' "$wf" "$skip_event" "$skip_names"
	skipped=$((skipped + 1))
done

for ctx in ${unmapped[@]+"${unmapped[@]}"}; do
	printf '  %-22s UNMAPPED — required, but names no workflow in this tree\n' "$ctx"
	failed=$((failed + 1))
done

if ((failed == 0 && skipped == 0)); then
	echo "check-gates-green: all ${#workflows[@]} gate(s) ran and passed on ${short}"
	exit 0
fi

{
	echo
	printf 'check-gates-green: %d gate(s) not green, %d path-skipped on %s.\n' \
		"$failed" "$skipped" "$short"
	if ((skipped > 0)); then
		cat <<-EOF

			A path-skipped gate is the normal shape of a docs-only commit stacked on a
			validated one, and it is not a failure by itself — but nothing validated
			THIS commit. Prove the released surface is unchanged since a commit that did
			run in full, and rely on that commit's verdict:
			  scripts/release/check-artifact-unchanged.sh <that-commit> ${ref}
			Say which commit you are relying on.
		EOF
	fi
} >&2
exit 1
