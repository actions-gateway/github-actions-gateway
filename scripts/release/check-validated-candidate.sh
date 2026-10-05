#!/usr/bin/env bash
# check-validated-candidate.sh — does a validated candidate cover this stable tag?
#
# The freeze rule has always had two halves and only ever machine-checked one.
# check-candidate-covers-main.sh asks whether the released surface moved after the
# newest prerelease tag; nothing asked whether that candidate was ever *validated*.
# The validated one was named in prose — a line in the release notes, a plan doc —
# so promoting the wrong candidate could not fail anything.
#
# Newest-RC is the reading that suggests itself and it is unsafe: `v1.5.0-rc.2` was
# tagged, published, and never validated (a stale-commit push burned the number,
# docs/postmortems/2026-08-15-rc2-tagged-a-stale-commit.md). A check keyed on the
# newest prerelease accepts whatever is newest, validated or not, so it would have
# accepted rc.2 for as long as rc.2 was the newest. What superseded it was a person
# questioning the tag's commit directly, which is the control this replaces.
#
#   scripts/release/check-validated-candidate.sh <tag>
#
# The evidence is CI's (Q880): each run of validate-candidate.yml on main uploads
# the candidate tag, the commit it names and each gate leg that passed, and
# fetch-gate-evidence.sh downloads it. A candidate is validated once those runs,
# together, have passed every leg in scripts/dogfood/lib/gate-legs.sh for one
# commit. Only main's copy of the workflow can put an artifact on such a run,
# where a ref (the `refs/validated/<rc-tag>` marker before Q880) is written by
# anyone who can push, so the verdict is the gate's own.
#
# CHECK_VALIDATED_EVIDENCE_DIR reads evidence already downloaded, for tests and
# for a maintainer re-checking; unset, it is fetched into a temporary directory.
#
# Exit 0 when a validated candidate covers the tag or the tag is a prerelease,
# 1 when it does not, 2 on a usage or git error.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=scripts/dogfood/lib/gate-legs.sh
source "${REPO_ROOT}/scripts/dogfood/lib/gate-legs.sh"

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") <tag>

		  tag  the tag being published (e.g. v1.6.0)
	EOF
	exit 2
}

[[ $# -eq 1 ]] || usage
[[ "$1" == -h || "$1" == --help ]] && usage

tag="$1"

if ! git rev-parse --verify --quiet "${tag}^{commit}" >/dev/null; then
	echo "check-validated-candidate: not a tag or commit: ${tag}" >&2
	exit 2
fi
tag_commit="$(git rev-parse "${tag}^{commit}")"

# Same prerelease test as publish.yml's tag resolver and the announce bar (Q293):
# SemVer core 0.x, or any '-' suffix. A candidate is the thing that gets
# validated, so asking this of one is the wrong question rather than a failure.
version="${tag#v}"
if [[ "${version}" == 0.* || "${version}" == *-* ]]; then
	echo "check-validated-candidate: ${tag} is a prerelease — it is the artifact that gets validated, nothing to check"
	exit 0
fi

evidence="${CHECK_VALIDATED_EVIDENCE_DIR:-}"
if [[ -z "${evidence}" ]]; then
	evidence="$(mktemp -d)"
	trap 'rm -rf "${evidence}"' EXIT
	if ! "${SCRIPT_DIR}/fetch-gate-evidence.sh" "${tag}" "${evidence}" >&2; then
		echo "check-validated-candidate: could not fetch the CI gate's evidence for ${tag}" >&2
		exit 2
	fi
fi

# Every leg each candidate of this line passed, keyed by candidate, from every
# evidence file whose commit is the one that candidate's tag names here. A file
# naming another commit means the tag moved under the gate, which is the
# stale-tag incident from the other side, so it stops the ship rather than being
# skipped.
declare -A legs_of=()
declare -A commit_of=()
while IFS= read -r -d '' file; do
	ev_tag="$(awk '$1 == "tag" { print $2; exit }' "${file}")"
	ev_commit="$(awk '$1 == "commit" { print $2; exit }' "${file}")"
	[[ "${ev_tag}" == "${tag}"-* ]] || continue
	if ! git rev-parse --verify --quiet "${ev_tag}^{commit}" >/dev/null; then
		cat >&2 <<-EOF
			check-validated-candidate: the CI gate passed legs for ${ev_tag}, but no such tag is present here.

			The evidence cannot be cross-checked against the commit its tag names. Either the
			checkout is not full-depth, or the tag is gone.
		EOF
		exit 2
	fi
	tag_sha="$(git rev-parse "${ev_tag}^{commit}")"
	if [[ "${ev_commit}" != "${tag_sha}" ]]; then
		cat >&2 <<-EOF
			check-validated-candidate: the CI gate validated ${ev_tag} at a commit its tag does not name.

			  validated: ${ev_commit}
			  ${ev_tag}: ${tag_sha}

			The gate ran against one commit and the tag names another, so the verdict does not
			describe the candidate. Cut and validate a new candidate.
		EOF
		exit 1
	fi
	commit_of["${ev_tag}"]="${ev_commit}"
	legs_of["${ev_tag}"]+=" $(awk '$1 == "leg" { printf "%s ", $2 }' "${file}")"
done < <(find "${evidence}" -type f -print0)

# A candidate is validated when every leg the gate runs has passed for it.
validated=()
summary=""
for candidate in "${!legs_of[@]}"; do
	missing=""
	for leg in "${GATE_LEGS_ALL[@]}"; do
		[[ " ${legs_of[${candidate}]} " == *" ${leg} "* ]] || missing+=" ${leg}"
	done
	if [[ -z "${missing}" ]]; then
		validated+=("${candidate}")
	else
		summary+="  ${candidate} still needs:${missing}"$'\n'
	fi
done

# Newest *validated* candidate of this release line, by version order. Newest
# prerelease is the reading this exists to replace, so the candidates come from
# the evidence and never from `git tag --list`.
marker="$(printf '%s\n' "${validated[@]}" | sort -V | tail -1)"
if [[ -z "$marker" ]]; then
	cat >&2 <<-EOF
		check-validated-candidate: no candidate for ${tag} has passed every gate leg in CI.

		A stable tag needs CI runs of validate-candidate.yml on main that, together, passed
		every leg (${GATE_LEGS_ALL[*]}) for one ${tag}-rc.N candidate.
		A local gate run does not count.
	EOF
	if [[ -n "${summary}" ]]; then
		printf '\n%s' "${summary}" >&2
		echo "Run the missing legs: gh workflow run validate-candidate.yml --ref main -f tag=<candidate> -f legs=<legs>" >&2
	fi
	cat >&2 <<-EOF

		Validate a candidate: docs/operations/release.md#validate-the-release-candidate-on-dogfood.
		A patch line needs its own candidate too; there is no exemption for one.
	EOF
	exit 1
fi
validated_sha="${commit_of[${marker}]}"

# A validated commit off this history is not a window that moved, it is a verdict
# from somewhere else, so it gets its own message rather than a file list.
if ! git merge-base --is-ancestor "$validated_sha" "$tag_commit"; then
	cat >&2 <<-EOF
		check-validated-candidate: ${marker} was validated at a commit that is not an ancestor of ${tag}.

		  validated: ${validated_sha}
		  ${tag}: ${tag_commit}

		The verdict covers a different line of history than the one being published.
	EOF
	exit 1
fi

# One question, one implementation: whether the window moved the released surface
# is check-artifact-unchanged.sh's to answer, and it derives that surface from
# publish.yml rather than listing it.
#
# Overridable so the decision layer above can be exercised at any checkout depth,
# the same way check-candidate-covers-main.sh does it. Tests only: nothing in the
# repository sets it.
surface_check="${CHECK_VALIDATED_SURFACE_CHECK:-$SCRIPT_DIR/check-artifact-unchanged.sh}"

out=""
rc=0
out="$("$surface_check" "$validated_sha" "$tag_commit" 2>&1)" || rc=$?

case "$rc" in
0)
	printf 'check-validated-candidate: %s validated %s and still covers %s\n' \
		"$marker" "$(git rev-parse --short "$validated_sha")" "$tag"
	exit 0
	;;
1) ;;
*)
	printf '%s\n' "$out" >&2
	echo "check-validated-candidate: surface check failed (exit ${rc})" >&2
	exit 2
	;;
esac

printf '%s\n' "$out" >&2
cat >&2 <<-EOF

	${marker} is the newest validated candidate for ${tag}, and the released surface moved
	after it was validated — so ${tag} would ship something no candidate ever exercised.

	Revert those files, or cut and validate a new candidate:

	  docs/operations/release.md#2-tag-and-push
EOF
exit 1
