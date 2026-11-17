#!/usr/bin/env bash
# check-vuln-policy.sh — may this commit be tagged under the vulnerability policy?
#
#   scripts/release/check-vuln-policy.sh [--max-age-hours N] [--now EPOCH] [ref]
#
# The policy (docs/operations/release.md § The vulnerability policy): no release
# ships a fixable HIGH or CRITICAL finding in a component this project chooses,
# and findings inherited from the upstream actions-runner base are accepted only
# while that base is the newest stable runner release. Three checks, all against
# the commit about to be tagged (default origin/main):
#
#   1. No open `security-scan` issue. The scheduled scan opens one when an advisory
#      published since the last merge reddens main.
#   2. A security-scan.yml run on this exact commit succeeded within the last N
#      hours (default 24), with govulncheck and every trivy leg having run rather
#      than path-skipped. A merge-time verdict ages: check-gates-green.sh accepts
#      a scan that ran when the commit merged, which can predate an advisory.
#   3. The Dockerfile's actions-runner base is the newest stable release, which is
#      the condition that makes the report-only legs' findings inherited rather
#      than ours (check-runner-window.sh --warn-days 0).
#
# It then prints the report-only legs' fixable HIGH/CRITICAL totals from that run,
# for the release notes' Security section. They are trivy's per-target totals
# summed, so a CVE present in two binaries counts twice.
#
# Exit 0 when the commit may be tagged, 1 when a check fails, 2 when one could not
# measure. 2 is never 0: a scan that could not be read is not a clean scan.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

usage() {
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' >&2
	exit 2
}

# fresh_runs RUNS_JSON NOW MAX_AGE_H — successful runs created within the window,
# newest first, as "id<TAB>event<TAB>createdAt" lines.
fresh_runs() {
	local runs="$1" now="$2" max_age_h="$3"
	printf '%s' "$runs" | jq -r --argjson now "$now" --argjson max "$((max_age_h * 3600))" '
		map(select(.conclusion == "success"
			and ($now - (.createdAt | fromdateiso8601)) <= $max))
		| sort_by(.createdAt) | reverse | .[]
		| "\(.databaseId)\t\(.event)\t\(.createdAt)"'
}

# scan_gaps JOBS_JSON — comma-joined scan jobs that did not succeed (skipped,
# failed, or absent), empty when govulncheck and at least one trivy leg ran and
# every trivy leg succeeded.
scan_gaps() {
	printf '%s' "$1" | jq -r '
		.jobs as $j
		| ([$j[] | select(.name == "govulncheck" or (.name | startswith("trivy")))
			| select(.conclusion != "success") | "\(.name): \(.conclusion // .status)"])
		+ (if any($j[]; .name == "govulncheck") then [] else ["govulncheck: absent"] end)
		+ (if any($j[]; .name | startswith("trivy")) then [] else ["trivy: absent"] end)
		| join(", ")'
}

# report_only_legs JOBS_JSON — "jobId<TAB>image" for each trivy leg whose matrix
# exit-code is 0. The leg's name carries it: `trivy (worker, 0)`.
report_only_legs() {
	printf '%s' "$1" | jq -r '.jobs[]
		| (.name | capture("^trivy \\((?<image>[^,]+), 0\\)$")?) as $m
		| select($m != null) | "\(.id)\t\($m.image)"'
}

# fixable_total LOG — sum of trivy's per-target `Total: N` lines in a job log.
fixable_total() {
	awk '{ if (match($0, /Total: [0-9]+ \(/)) { s += substr($0, RSTART + 7, RLENGTH - 9) } } END { print s + 0 }' <<<"$1"
}

# dockerfile_runner_pin DOCKERFILE — the X.Y.Z of the actions-runner base image.
dockerfile_runner_pin() {
	awk 'match($0, /^FROM ghcr\.io\/actions\/actions-runner:[0-9]+\.[0-9]+\.[0-9]+/) {
		v = substr($0, RSTART, RLENGTH); sub(/.*:/, "", v); print v; exit }' "$1"
}

# Stop here when sourced for its helpers, so the test drives them offline.
if [[ "${CHECK_VULN_POLICY_LIB:-}" == "1" ]]; then
	# shellcheck disable=SC2317  # reached only via `source`, which shellcheck cannot see
	return 0
fi

max_age_h=24
now=""
ref="origin/main"
while (($#)); do
	case "$1" in
	--max-age-hours) max_age_h="${2:?--max-age-hours needs a number}"; shift 2 ;;
	--now) now="${2:?--now needs an epoch}"; shift 2 ;;
	-h | --help) usage ;;
	-*) usage ;;
	*) ref="$1"; shift ;;
	esac
done
[[ "$max_age_h" =~ ^[0-9]+$ ]] || usage
now="${now:-$(date +%s)}"

if ! sha="$(git rev-parse --verify --quiet "${ref}^{commit}")"; then
	echo "check-vuln-policy: not a commit: ${ref}" >&2
	exit 2
fi
short="${sha:0:9}"
echo "check-vuln-policy: ${short} (${ref})"

failed=0
unmeasured=0

# 1. An open scheduled-scan issue means main went red after the last merge.
if issues="$(gh issue list --label security-scan --state open --json number,url 2>/dev/null)"; then
	if [[ "$(jq 'length' <<<"$issues")" -gt 0 ]]; then
		printf '  open scan issue       FAIL — %s\n' "$(jq -r 'map(.url) | join(" ")' <<<"$issues")"
		failed=$((failed + 1))
	else
		echo "  open scan issue       ok (none)"
	fi
else
	echo "  open scan issue       UNMEASURED — gh issue list failed"
	unmeasured=$((unmeasured + 1))
fi

# 2. A fresh, complete scan of this exact commit.
jobs=""
run_id=""
runs="$(gh run list --workflow=security-scan.yml --commit "$sha" -L 30 \
	--json databaseId,event,conclusion,createdAt 2>/dev/null || echo '[]')"
gaps="no successful run within ${max_age_h}h"
while IFS=$'\t' read -r id event created; do
	[[ -n "$id" ]] || continue
	j="$(gh api "repos/{owner}/{repo}/actions/runs/${id}/jobs?per_page=100" 2>/dev/null || echo '{"jobs":[]}')"
	gaps="$(scan_gaps "$j")"
	if [[ -z "$gaps" ]]; then
		jobs="$j"
		run_id="$id"
		printf '  fresh scan            ok (%s run %s at %s)\n' "$event" "$id" "$created"
		break
	fi
done < <(fresh_runs "$runs" "$now" "$max_age_h")
if [[ -z "$run_id" ]]; then
	printf '  fresh scan            FAIL — %s\n' "$gaps"
	failed=$((failed + 1))
fi

# 3. Inherited findings are only inherited while the runner base is current.
pin="$(dockerfile_runner_pin "${REPO_ROOT}/Dockerfile")"
if [[ -z "$pin" ]]; then
	echo "  runner base           UNMEASURED — no actions-runner FROM line in the Dockerfile"
	unmeasured=$((unmeasured + 1))
elif ! feed="$(gh api repos/actions/runner/releases 2>/dev/null)"; then
	echo "  runner base           UNMEASURED — could not read actions/runner releases"
	unmeasured=$((unmeasured + 1))
else
	rc=0
	verdict="$("${REPO_ROOT}/scripts/release/check-runner-window.sh" --pinned "$pin" --warn-days 0 --now "$now" - <<<"$feed" 2>&1)" || rc=$?
	case "$rc" in
	0) printf '  runner base           ok (%s)\n' "$verdict" ;;
	1) printf '  runner base           FAIL — %s\n' "$(head -1 <<<"$verdict")"; failed=$((failed + 1)) ;;
	*) printf '  runner base           UNMEASURED — %s\n' "$verdict"; unmeasured=$((unmeasured + 1)) ;;
	esac
fi

# Inherited findings, for the notes. Informational: never changes the verdict.
if [[ -n "$jobs" ]]; then
	echo "  inherited (report-only legs, fixable HIGH/CRITICAL, per-target totals summed):"
	while IFS=$'\t' read -r job_id image; do
		[[ -n "$job_id" ]] || continue
		if log="$(gh run view --job "$job_id" --log 2>/dev/null)"; then
			printf '    %-14s %s\n' "$image" "$(fixable_total "$log")"
		else
			printf '    %-14s unread\n' "$image"
		fi
	done < <(report_only_legs "$jobs")
fi

if ((failed > 0)); then
	{
		echo
		echo "check-vuln-policy: ${failed} check(s) failed on ${short}; do not tag it."
		cat <<-EOF
			A stale or missing scan is re-asked on main's head with:
			  gh workflow run security-scan.yml --ref main
			then re-run this once it completes. An open scan issue or a red scan needs
			a fix PR first; a stale runner base needs the pin bumped and a new scan.
		EOF
	} >&2
	exit 1
fi
if ((unmeasured > 0)); then
	echo "check-vuln-policy: ${unmeasured} check(s) could not measure on ${short}; that is not a pass." >&2
	exit 2
fi
echo "check-vuln-policy: ${short} meets the vulnerability policy"
