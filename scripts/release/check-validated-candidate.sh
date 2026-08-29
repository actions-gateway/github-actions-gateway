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
# newest prerelease would have accepted it.
#
#   scripts/release/check-validated-candidate.sh <tag>
#
# The marker is `refs/validated/<rc-tag>`, written on the remote by the dogfood
# gate when it passes (scripts/dogfood/record-validated-candidate.sh) and read
# here. A ref rather than a file in the tree, for the reason `refs/queue-ids/*` is
# one: it records a fact about a commit without being a commit, so recording a
# verdict cannot move the surface that verdict covers.
#
# It is a record, not an attestation. Anyone who can push a tag can push the
# marker, so this proves the gate was run and reported to, never that a green
# verdict was earned. That is the same trust level as the tag itself, and it is
# what closes the gap the row named: a promote with no validation anywhere behind
# it now fails before an image is pushed.
#
# Exit 0 when a validated candidate covers the tag or the tag is a prerelease,
# 1 when it does not, 2 on a usage or git error.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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

# Newest *validated* candidate of this release line, by version order. Newest
# prerelease is the reading this exists to replace, so the glob is over the marker
# namespace and never over `git tag --list`.
marker="$(git for-each-ref --format='%(refname:lstrip=2)' "refs/validated/${tag}-*" | sort -V | tail -1)"
if [[ -z "$marker" ]]; then
	cat >&2 <<-EOF
		check-validated-candidate: no candidate for ${tag} has a recorded validation.

		Nothing under refs/validated/ names a ${tag}-rc.* candidate, so no dogfood gate
		reported a pass for this release line — or the run that did could not record it.

		  git ls-remote origin 'refs/validated/*'

		Validate a candidate (docs/operations/release.md#validate-the-release-candidate-on-dogfood),
		or record a validation that already happened, with the command that gate prints.
	EOF
	exit 1
fi
validated_sha="$(git rev-parse "refs/validated/${marker}^{commit}")"

# The marker records the commit; the RC tag names one too. They are written from
# the same read, so a disagreement means the tag moved under the record — which is
# the incident that filed the row, seen from the other side. Refusing to measure is
# the honest answer when the tag is absent: publish.yml checks out at full depth,
# so an absent tag there is an anomaly rather than a shallow clone.
if ! git rev-parse --verify --quiet "${marker}^{commit}" >/dev/null; then
	echo "check-validated-candidate: ${marker} is recorded as validated but no such tag is present here" >&2
	echo "  (a full-depth checkout is required to cross-check the marker against the tag)" >&2
	exit 2
fi
marker_tag_commit="$(git rev-parse "${marker}^{commit}")"
if [[ "$validated_sha" != "$marker_tag_commit" ]]; then
	cat >&2 <<-EOF
		check-validated-candidate: ${marker} does not point at the commit it was validated at.

		  validated: ${validated_sha}
		  ${marker}: ${marker_tag_commit}

		The gate ran against one commit and the tag names another, so the verdict does not
		describe the candidate. Cut and validate a new candidate.
	EOF
	exit 1
fi

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
