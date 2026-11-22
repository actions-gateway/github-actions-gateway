#!/usr/bin/env bash
#
# Assert tag-deploy-inputs.sh resolves a pushed tag to the right deploy inputs
# (Q1355). Both directions: the highest release claims `stable`, and a backport
# or a prerelease must not. A suite where every case claimed `stable` would pass
# the first alone, so the backport and prerelease cases are the controls.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$REPO_ROOT/scripts/pages/tag-deploy-inputs.sh"

fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

TAGS=$'v1.9.0\nv1.10.0\nv2.0.0\nv2.0.0-rc.3\nv2.0.1-rc.1\nv0.9.0\nv2.0.1'

# expect NAME TAG WANT_OUTPUT — the full stdout must equal WANT_OUTPUT.
expect() {
	local name="$1" tag="$2" want="$3" got
	got="$("$SCRIPT" "$tag" <<< "$TAGS")"
	if [[ "$got" == "$want" ]]; then
		pass "$name"
	else
		fail "$name" "got: $(tr '\n' ' ' <<< "$got")| want: $(tr '\n' ' ' <<< "$want")"
	fi
}

expect "highest release claims stable" v2.0.1 $'deploy=true\nversion=2.0.1\ndocs_ref=v2.0.1\nalias=stable\nset_default=true'
expect "backport to an older line leaves stable" v1.10.0 $'deploy=true\nversion=1.10.0\ndocs_ref=v1.10.0\nalias=\nset_default=false'
expect "release candidate deploys nothing" v2.0.0-rc.3 'deploy=false'
expect "0.x deploys nothing" v0.9.0 'deploy=false'

# Version order is numeric: v1.10.0 outranks v1.9.0. A string sort ranks 1.9.0 higher.
got="$("$SCRIPT" v1.10.0 <<< $'v1.9.0\nv1.10.0')"
if grep -q '^alias=stable$' <<< "$got"; then
	pass "1.10.0 outranks 1.9.0"
else
	fail "1.10.0 outranks 1.9.0" "$got"
fi

# A tag the listing does not yet carry still ranks (the push races the listing).
got="$("$SCRIPT" v3.0.0 <<< $'v2.0.1')"
if grep -q '^alias=stable$' <<< "$got"; then
	pass "unlisted newest tag claims stable"
else
	fail "unlisted newest tag claims stable" "$got"
fi

if "$SCRIPT" 2.0.1 <<< "" 2> /dev/null; then
	fail "rejects a tag without the v prefix" "exit 0"
else
	pass "rejects a tag without the v prefix"
fi

((fails == 0)) || exit 1
