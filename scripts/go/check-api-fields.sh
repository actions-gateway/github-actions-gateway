#!/usr/bin/env bash
#
# check-api-fields.sh — fail when a served API field has no consumer: a spec
# field no controller reads, or a status field no controller writes (Q573).
#
# The API server stores a served field whether or not anything acts on it, so an
# unconsumed one is accepted silently and then ignored. `sharing.allowedNamespaces`
# shipped that way in v2beta1 and only a manual docs sweep caught it (Q166);
# v1alpha1's `status.activeSessions` shipped described and never set (Q526).
#
# The checking is devtools/ci/apifields, which type-checks every workspace module
# and classifies each use of each field; its package comment has the rules. The
# known exceptions, each with its reason, are api/unconsumed-fields.txt.
#
# Each API_GROUPS entry is one API group, as the comma-separated packages serving
# its versions: versions convert through a JSON round-trip, so a v2beta1 field is
# consumed when the controllers read its v2alpha1 counterpart. A new version goes
# in here the day it lands, because apifields fails on any package declaring a
# root kind that no entry names.
#
# Heavy: loading the workspace compiles it, so this holds a heavy-build slot like
# the other `make check` heavy phases.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
source "$REPO_ROOT/scripts/lib/common.sh"

MOD="github.com/actions-gateway/github-actions-gateway"
API_GROUPS=(
	"$MOD/api/v2alpha1,$MOD/api/v2beta1"
	"$MOD/gmc/api/v1alpha1"
	"$MOD/agc/api/v1alpha1"
)
BASELINE="api/unconsumed-fields.txt"
# 326 fields at Q573. A floor well under that catches a walk that stops matching
# (a renamed ObjectMeta embed, a group that loads empty) without taxing a PR that
# removes a few fields.
MIN_FIELDS=250

main() {
	serialize_heavy_build "$@"

	local bin="$REPO_ROOT/.build/apifields"
	(cd devtools && GOWORK=off go build -o "$bin" ./ci/apifields)

	init_throttle
	[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"
	echo "==> apifields ${API_GROUPS[*]}"
	# shellcheck disable=SC2086  # the throttle prefix word-splits intentionally
	$THROTTLE_PREFIX "$bin" -baseline "$BASELINE" -min "$MIN_FIELDS" "${API_GROUPS[@]}"
}

main "$@"
