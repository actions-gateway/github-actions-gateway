#!/usr/bin/env bash
# wait-for-publish.sh — hold the CI release gate until the candidate's images
# have been published, and refuse to go on when the publish did not succeed.
#
#   REPO=owner/name scripts/dogfood/wait-for-publish.sh [--images] <rc-tag>
#
# validate-candidate.yml dispatches the gate the moment a candidate tag is pushed,
# and publish.yml starts from the same push, so nothing orders the gate's deploy
# after the six images it pulls. On v1.9.0-rc.2 the margin was 2m18s, eaten by a
# slower publish or a free e2e lane; a lost race fails `kubectl rollout status`
# in start.sh, a gate failure that says nothing about the candidate (Q1162).
#
# The publish run is found by the tag's name and commit and the push event: a tag
# push records the tag as the run's branch, so a manual dispatch cannot stand in
# for it, and neither can a sibling tag on the same commit (v1.2.0 and v1.2.0-rc.1
# share one, and the commit alone returns v1.2.0's run for the candidate). Its conclusion
# is read once it completes: `success` returns 0, anything else returns 1, so a
# red publish fails the gate rather than deploying what it left behind.
#
# --images returns as soon as every `publish (…)` image job has succeeded, without
# waiting for `chart-publish`, which creates the GitHub Release and attaches the
# CRD manifest and gag-migrate binaries and finishes about 5.5 minutes later
# (Q1365). The deploy reads only the images: setup.sh installs the chart from the
# checkout and git-archives the CRD chart at the tag. A red or cancelled image job
# returns 1, as does a finished run with no succeeded image set (a stop-ship gate
# that failed ahead of every image). Whatever reads the release's assets must then
# run the plain form, which still requires the whole run to succeed.
#
# A wait that runs out returns 1 too. Nothing here treats "no verdict yet" as
# "no problem": every path that is not a successful publish is non-zero.
#
# Env: REPO (required); WAIT_TIMEOUT seconds (default 2700); WAIT_INTERVAL
# seconds between polls (default 30).
set -euo pipefail
shopt -s inherit_errexit

images_only=0
if [[ "${1:-}" == "--images" ]]; then
	images_only=1
	shift
fi
[[ $# -eq 1 ]] || {
	echo "usage: REPO=owner/name $(basename "$0") [--images] <rc-tag>" >&2
	exit 2
}
tag="$1"
: "${REPO:?REPO must be set (owner/name)}"
timeout="${WAIT_TIMEOUT:-2700}"
interval="${WAIT_INTERVAL:-30}"
[[ "$timeout" =~ ^[0-9]+$ && "$interval" =~ ^[1-9][0-9]*$ ]] || {
	echo "wait-for-publish: WAIT_TIMEOUT and WAIT_INTERVAL must be whole seconds, interval at least 1" >&2
	exit 2
}

if ! sha="$(git rev-parse --verify --quiet "refs/tags/${tag}^{commit}")"; then
	echo "wait-for-publish: tag ${tag} is not in this checkout (fetch tags)" >&2
	exit 1
fi

# image_jobs_verdict <run-id> — print "success", "failed" or "pending" for the
# run's `publish (…)` matrix jobs; "pending" also covers none listed yet and a
# failed `gh run view`, since neither is a verdict.
image_jobs_verdict() {
	local jobs
	jobs="$(gh run view "$1" --repo "${REPO}" --json jobs 2>&1)" || {
		echo pending
		return 0
	}
	jq -r '[.jobs[] | select(.name | startswith("publish ("))] as $j
		| if ($j | length) == 0 then "pending"
		  elif any($j[]; .status == "completed" and .conclusion != "success") then "failed"
		  elif all($j[]; .status == "completed") then "success"
		  else "pending" end' <<<"${jobs}" 2>/dev/null || echo pending
}

polls=$((timeout / interval))
last="no publish run for ${tag} (${sha:0:7}) had appeared"
for ((i = 0; i <= polls; i++)); do
	if runs="$(gh run list --repo "${REPO}" --workflow publish.yml --event push \
		--commit "${sha}" --branch "${tag}" --limit 1 --json databaseId,status,conclusion,url 2>&1)"; then
		status="$(jq -r '.[0].status // "none"' <<<"${runs}")"
		conclusion="$(jq -r '.[0].conclusion // ""' <<<"${runs}")"
		url="$(jq -r '.[0].url // ""' <<<"${runs}")"
		if ((images_only)) && [[ "${status}" != none ]]; then
			case "$(image_jobs_verdict "$(jq -r '.[0].databaseId' <<<"${runs}")")" in
			success)
				echo "wait-for-publish: the images of ${tag} were published (${url})"
				exit 0
				;;
			failed)
				echo "::error::an image job in publish.yml for ${tag} did not succeed, so its images are missing or unsigned and the gate would deploy what it left: ${url}"
				exit 1
				;;
			esac
			if [[ "${status}" == completed ]]; then
				echo "::error::publish.yml for ${tag} concluded '${conclusion}' with no succeeded image jobs: ${url}"
				exit 1
			fi
			last="publish run ${url} was still ${status}, image jobs not all succeeded"
			echo "wait-for-publish: waiting (${i}/${polls}): ${last}"
			if ((i < polls)); then sleep "${interval}"; fi
			continue
		fi
		case "${status}" in
		completed)
			if [[ "${conclusion}" == "success" ]]; then
				echo "wait-for-publish: publish of ${tag} succeeded (${url})"
				exit 0
			fi
			echo "::error::publish.yml for ${tag} concluded '${conclusion}', so its images are missing or unsigned and the gate would deploy what it left: ${url}"
			exit 1
			;;
		none) last="no publish run for ${tag} (${sha:0:7}) had appeared" ;;
		*) last="publish run ${url} was still ${status}" ;;
		esac
	else
		last="gh run list failed: ${runs}"
	fi
	echo "wait-for-publish: waiting (${i}/${polls}): ${last}"
	if ((i < polls)); then sleep "${interval}"; fi
done
echo "::error::timed out after ${timeout}s waiting for publish.yml on ${tag}: ${last}"
exit 1
