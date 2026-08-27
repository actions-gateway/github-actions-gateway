#!/usr/bin/env bash
# check-candidate-covers-main.sh — does the outstanding candidate still cover main?
#
# The freeze rule between a validated candidate and its stable tag is documented
# and real, but it was only ever evaluated by hand, by whoever promotes, after the
# fact. `v1.6.0-rc.1` died that way: it was tagged, three Dependabot pull requests
# merged on top, and nothing said so until the promote-time pre-flight ran the next
# morning (Q1001).
#
#   scripts/release/check-candidate-covers-main.sh [to-ref]
#
# Exit 0 when no candidate is outstanding or the outstanding one still covers
# `to-ref`, 1 when it no longer does, 2 on a usage or git error.
#
# WHY THIS RUNS POST-MERGE ON `main` AND NOT ON THE PULL REQUEST.
# The answer is a conjunction — a candidate is outstanding, and the released
# surface moved — and on a pull request neither half is final. Measured over the
# 21 candidate windows between `v1.0.0-rc.1` and `v1.6.0`: 497 pull requests
# merged inside one, 112 of them touching the released surface, against 16
# candidates actually invalidated. A pull-request warning therefore fires about
# seven times per thing worth saying, and 96 of those 112 report a candidate that
# was already dead when they fired. The one correct response — cut a new
# candidate — belongs to the release engineer either way, so the volume lands on
# somebody who cannot act on it.
#
# Tag creation fires no `pull_request` event, so a check on that trigger is also
# blind to a candidate cut after the branch last moved: measured on the 29 firing
# pull requests since `v1.3.0-rc.1`, 2 had heads predating the tag they went on to
# invalidate (#919 by four days, #1078 by thirteen minutes). Post-merge on `main`
# both halves are facts rather than predictions, and the trigger is the event that
# flips the answer, the way autoscaler-drift.yml picks its own.
#
# It is deliberately author-blind. The pull requests that killed rc.1 were
# Dependabot's, which nobody reads as release-affecting; a design keyed on human
# pull requests would have missed the incident it exists for.
#
# The reference is the newest prerelease tag, not the commit a candidate was
# validated at. That is the conservative reading and it needs nothing declared:
# rc.1 was never validated at all, so a check keyed on a recorded validation would
# have had nothing to key on. Same reasoning as resolve_prepared_release, which
# derives an outstanding candidate from tags alone so nothing has to set or clear
# it.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") [to-ref]

		  to-ref  the commit to test the candidate against
		          default: origin/main if it resolves, else HEAD
	EOF
	exit 2
}

[[ $# -le 1 ]] || usage
[[ "${1:-}" == -h || "${1:-}" == --help ]] && usage

# Everything below reads the repository the script is invoked in, not the one it
# lives in, so a fixture clone can pose a candidate window this tree has already
# closed. check-artifact-unchanged.sh reads the working directory the same way.
repo_root="$(git rev-parse --show-toplevel)"

to="${1:-}"
if [[ -z "$to" ]]; then
	to="HEAD"
	if git -C "$repo_root" rev-parse --verify --quiet origin/main >/dev/null; then
		to="origin/main"
	fi
fi

if ! git -C "$repo_root" rev-parse --verify --quiet "${to}^{commit}" >/dev/null; then
	echo "check-candidate-covers-main: not a commit: ${to}" >&2
	exit 2
fi

# Tags only, and cheap: no candidate outstanding is the common case (the windows
# above cover 39% of the repository's tagged lifetime, so most pushes land
# outside one) and answering it here avoids building the surface tool at all.
release="$(resolve_prepared_release "$repo_root")"
if [[ -z "$release" ]]; then
	echo "check-candidate-covers-main: no candidate outstanding"
	exit 0
fi

# The newest prerelease of that release, by version order — rc.2 supersedes rc.1,
# so the freeze that matters is the one that began at the newest tag.
candidate="$(git -C "$repo_root" tag --list "${release}-*" | sort -V | tail -1)"
if [[ -z "$candidate" ]]; then
	echo "check-candidate-covers-main: ${release} is outstanding but no prerelease tag matches ${release}-*" >&2
	exit 2
fi

# One question, one implementation: the released surface is check-artifact-unchanged.sh's
# to answer, and it derives that surface from publish.yml rather than listing it.
#
# Overridable so the decision layer above — resolving the candidate, picking the
# newest prerelease, mapping the delegate's verdict — can be exercised at any
# checkout depth. Deriving the surface for real needs `go list -deps` over every
# module the Dockerfile builds, which a fixture cannot pose cheaply and which
# check-artifact-unchanged-test.sh already owns. Tests only: nothing in the
# repository sets it.
surface_check="${CHECK_CANDIDATE_SURFACE_CHECK:-$SCRIPT_DIR/check-artifact-unchanged.sh}"

out=""
rc=0
out="$("$surface_check" "$candidate" "$to" 2>&1)" || rc=$?

case "$rc" in
0)
	printf 'check-candidate-covers-main: %s still covers %s\n' "$candidate" "$to"
	exit 0
	;;
1) ;;
*)
	printf '%s\n' "$out" >&2
	echo "check-candidate-covers-main: surface check failed (exit ${rc})" >&2
	exit 2
	;;
esac

printf '%s\n' "$out" >&2
cat >&2 <<-EOF

	${candidate} no longer covers ${to}: the released surface moved after it was tagged,
	so any validation verdict it carries describes something other than what would ship.

	This is not a broken build and nothing here needs reverting. The merge was fine;
	the candidate is spent. Cut and validate a new one:

	  docs/operations/release.md#2-tag-and-push
EOF
exit 1
