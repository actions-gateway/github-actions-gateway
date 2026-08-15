#!/usr/bin/env bash
# check-gates-green.sh — are the release gates green on this exact commit?
#
#   scripts/release/check-gates-green.sh [ref]        # default: origin/main
#
# Exit 0 when every required workflow has a successful run for the commit, 1 when
# one is missing, pending or failed, 2 on a usage or `gh` error.
#
# Ask by commit, never by branch. `gh run list --branch main` filters to runs
# whose head *branch* is main, which excludes merge-queue runs — and the merge
# queue is where a commit is validated before it lands. A push-lane run for the
# same commit sits behind the previous push's per-ref concurrency group and can
# read `pending` for a long time while the identical tree is already green from
# the queue. Reading only the push lane says "not validated" about a commit that
# is (measured 2026-08-15, twice in one release cut).
#
# So a gate passes when *some* lane succeeded on this SHA, and the lane is
# reported so the reader can see which one answered.
set -euo pipefail
shopt -s inherit_errexit

WORKFLOWS=(unit-test.yml integration-test.yml security-scan.yml e2e-test.yml)

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") [ref]

		  ref  commit to check (default: origin/main)
	EOF
	exit 2
}

[[ $# -le 1 ]] || usage
[[ "${1:-}" == -h || "${1:-}" == --help ]] && usage

ref="${1:-origin/main}"
if ! sha="$(git rev-parse --verify --quiet "${ref}^{commit}")"; then
	echo "check-gates-green: not a commit: ${ref}" >&2
	exit 2
fi
short="${sha:0:9}"

echo "check-gates-green: ${short} (${ref})"
missing=0
for wf in "${WORKFLOWS[@]}"; do
	# -L 20 rather than a filter: the API has no head-SHA query, so the runs are
	# fetched and matched locally. A busy lane can push a run past 20, which shows
	# up as "no run found" rather than a false pass.
	runs="$(gh run list --workflow="$wf" -L 20 \
		--json headSha,event,status,conclusion \
		--jq "[.[] | select(.headSha == \"${sha}\")]" 2>/dev/null || echo '[]')"

	ok="$(printf '%s' "$runs" | jq -r '[.[] | select(.conclusion == "success")] | .[0].event // ""')"
	if [[ -n "$ok" ]]; then
		printf '  %-22s ok (%s)\n' "$wf" "$ok"
		continue
	fi

	state="$(printf '%s' "$runs" | jq -r 'if length == 0 then "no run found for this commit"
		else (map("\(.event):\(.status)/\(.conclusion // "-")") | join("  ")) end')"
	printf '  %-22s NOT GREEN — %s\n' "$wf" "$state"
	missing=$((missing + 1))
done

if ((missing > 0)); then
	cat >&2 <<-EOF

		check-gates-green: ${missing} gate(s) not green on ${short}.

		A path-gated gate that skipped is not automatically a failure: prove the
		shipped surface is unchanged since a commit that did run it, with
		  scripts/release/check-artifact-unchanged.sh <that-commit> ${ref}
	EOF
	exit 1
fi

echo "check-gates-green: all ${#WORKFLOWS[@]} gate(s) green on ${short}"
