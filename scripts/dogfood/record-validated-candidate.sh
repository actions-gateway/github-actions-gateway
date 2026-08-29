#!/usr/bin/env bash
# record-validated-candidate.sh — record that a candidate passed the dogfood gate.
#
# The gate's verdict used to live in prose: a line in the release notes, a plan
# doc, a session transcript. Nothing at publish time could read any of those, so
# promoting a candidate that was never validated could not fail anything, and
# newest-RC is not a safe stand-in — `v1.5.0-rc.2` was tagged and published having
# spent no validation at all (docs/postmortems/2026-08-15-rc2-tagged-a-stale-commit.md).
#
#   REPO=owner/name scripts/dogfood/record-validated-candidate.sh <rc-tag>
#
# Writes `refs/validated/<rc-tag>` on the remote, pointing at the commit that tag
# resolves to. scripts/release/check-validated-candidate.sh reads it from
# publish.yml, ahead of every publishing job.
#
# A ref rather than a commit in the tree, for the reason `refs/queue-ids/*` is one
# (docs/development/queue-id-allocation.md): it records a fact about a commit
# without being a commit, so recording a verdict cannot move the released surface
# that verdict covers — which a file committed to `main` would, immediately, for
# every candidate outstanding.
#
# The commit comes from the REMOTE tag, not from the local checkout. The gate runs
# from a checkout of any recent ref rather than from the candidate, so a local
# `git rev-parse` would answer about the wrong tree — and the remote's answer is
# the one publish.yml will see.
#
# Idempotent: re-running against an already-recorded candidate is a no-op when the
# commit agrees, and exit 1 when it does not. A ref create is a compare-and-swap
# server-side, so this cannot silently overwrite an earlier verdict.
#
# Exit 0 recorded (or already recorded), 1 the record could not be made, 2 usage.
set -euo pipefail
shopt -s inherit_errexit

usage() {
	cat >&2 <<-EOF
		usage: REPO=owner/name $(basename "$0") <rc-tag>

		  rc-tag  the candidate that passed, e.g. v1.6.0-rc.2
	EOF
	exit 2
}

[[ $# -eq 1 ]] || usage
[[ "$1" == -h || "$1" == --help ]] && usage

rc_tag="$1"
: "${REPO:?REPO must be set (owner/name)}"

if [[ "$rc_tag" != v*-* ]]; then
	echo "record-validated-candidate: ${rc_tag} is not a candidate tag (expected vX.Y.Z-rc.N)" >&2
	exit 2
fi

ref="refs/validated/${rc_tag}"

# The tag object first, then its target. An annotated tag — which every release
# tag here is — points at a tag object, and it is the COMMIT that has to be
# recorded: check-artifact-unchanged.sh diffs commits, and a tag object's sha
# would resolve to nothing in publish.yml's checkout.
if ! tag_obj="$(gh api "repos/${REPO}/git/ref/tags/${rc_tag}" --jq '.object.type + " " + .object.sha' 2>&1)"; then
	echo "record-validated-candidate: could not read tag ${rc_tag} on ${REPO}" >&2
	printf '  %s\n' "$tag_obj" >&2
	exit 1
fi
read -r obj_type obj_sha <<<"$tag_obj"

commit="$obj_sha"
if [[ "$obj_type" == tag ]]; then
	if ! commit="$(gh api "repos/${REPO}/git/tags/${obj_sha}" --jq '.object.sha' 2>&1)"; then
		echo "record-validated-candidate: could not dereference the ${rc_tag} tag object" >&2
		printf '  %s\n' "$commit" >&2
		exit 1
	fi
fi

# Already recorded is the re-run case and must stay free — a gate whose record
# failed on a network blip is re-run by hand, and so is one re-run for any other
# reason. Only a DISAGREEMENT is a finding.
if existing="$(gh api "repos/${REPO}/git/ref/validated/${rc_tag}" --jq '.object.sha' 2>/dev/null)"; then
	if [[ "$existing" == "$commit" ]]; then
		echo "record-validated-candidate: ${ref} already records ${commit}"
		exit 0
	fi
	cat >&2 <<-EOF
		record-validated-candidate: ${ref} already records a different commit.

		  recorded: ${existing}
		  ${rc_tag}: ${commit}

		A validation verdict is not re-pointed. Cut and validate a new candidate.
	EOF
	exit 1
fi

if ! out="$(gh api -X POST "repos/${REPO}/git/refs" -f "ref=${ref}" -f "sha=${commit}" 2>&1)"; then
	cat >&2 <<-EOF
		record-validated-candidate: could not create ${ref} on ${REPO}.

		${out}

		The validation itself is unaffected — it is the record that is missing, and
		publish.yml will refuse the stable tag until it exists. Re-run:

		  REPO=${REPO} scripts/dogfood/record-validated-candidate.sh ${rc_tag}
	EOF
	exit 1
fi

echo "record-validated-candidate: ${ref} -> ${commit}"
