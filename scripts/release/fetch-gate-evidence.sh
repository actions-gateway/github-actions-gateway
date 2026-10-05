#!/usr/bin/env bash
# fetch-gate-evidence.sh — download the CI release gate's evidence for one
# release line, for check-validated-candidate.sh (Q880).
#
#   REPO=owner/name scripts/release/fetch-gate-evidence.sh <stable-tag> <dir>
#
# The evidence is the artifact validate-candidate.yml uploads from each gate run:
# the candidate's tag, the commit it names, and each leg that passed
# (scripts/dogfood/validate-release.sh, GATE_EVIDENCE_FILE). A failed run uploads
# the legs it passed too, so a gate finished by re-running one leg has its other
# legs on an earlier run.
#
# Only runs of that workflow dispatched on main count, read from each run's own
# path, branch and event rather than from the query that listed it. Main's copy
# is the one the dogfood identity trusts and the one that runs main's gate, so an
# artifact on a branch's copy, or on the tag push's dispatch job, is not evidence.
# The run-name carries the candidate tag, which keeps the fetch to this line.
#
# Writes each artifact's files under <dir>/<run-id>-<artifact-name>/. Exit 0 with
# whatever was found, nothing included; 2 when the runs cannot be listed.
set -euo pipefail
shopt -s inherit_errexit

usage() {
	cat >&2 <<-EOF
		usage: REPO=owner/name $(basename "$0") <stable-tag> <dir>

		  stable-tag  the tag being published, e.g. v1.9.0
		  dir         where to write the evidence files
	EOF
	exit 2
}

[[ $# -eq 2 ]] || usage
tag="$1"
dir="$2"
: "${REPO:?REPO must be set (owner/name)}"

WORKFLOW_FILE="validate-candidate.yml"
WORKFLOW_PATH=".github/workflows/${WORKFLOW_FILE}"
ARTIFACT_PREFIX="gate-evidence"

mkdir -p "${dir}"

if ! runs_json="$(gh api --paginate \
	"repos/${REPO}/actions/workflows/${WORKFLOW_FILE}/runs?event=workflow_dispatch&branch=main&status=completed&per_page=100" 2>&1)"; then
	echo "fetch-gate-evidence: could not list ${WORKFLOW_FILE} runs on ${REPO}" >&2
	printf '  %s\n' "${runs_json}" >&2
	exit 2
fi

runs="$(jq -r --arg path "${WORKFLOW_PATH}" --arg title "validate-candidate ${tag}-" '
	.workflow_runs[]
	| select(.path == $path and .head_branch == "main" and .event == "workflow_dispatch")
	| select(.display_title | startswith($title))
	| .id' <<<"${runs_json}")"

count=0
for run in ${runs}; do
	if ! names="$(gh api "repos/${REPO}/actions/runs/${run}/artifacts" \
		--jq ".artifacts[] | select(.expired | not) | select(.name | startswith(\"${ARTIFACT_PREFIX}\")) | .name" 2>&1)"; then
		echo "fetch-gate-evidence: could not list run ${run}'s artifacts; skipping it" >&2
		printf '  %s\n' "${names}" >&2
		continue
	fi
	for name in ${names}; do
		if gh run download "${run}" --repo "${REPO}" --name "${name}" --dir "${dir}/${run}-${name}" >/dev/null; then
			count=$((count + 1))
		else
			echo "fetch-gate-evidence: could not download ${name} from run ${run}; skipping it" >&2
		fi
	done
done

echo "fetch-gate-evidence: ${count} evidence artifact(s) for ${tag} candidates in ${dir}"
