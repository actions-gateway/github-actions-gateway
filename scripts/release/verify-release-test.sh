#!/usr/bin/env bash
#
# Assert the release signing-identity regexp (release_identity_regexp in
# scripts/lib/common.sh, consumed by verify-release.sh) is tags-only: it must
# ACCEPT a `refs/tags/vX.Y.Z` identity and REJECT a `refs/heads/...` branch
# identity. This is the regression guard for Q124 — before the fix the pattern
# matched `refs/(tags|heads)/.*`, so a release signature minted from a branch
# (a workflow_dispatch run on a scratch branch) verified as legitimate.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

id_re="$(release_identity_regexp)"
base='https://github.com/actions-gateway/github-actions-gateway/.github/workflows/publish.yml'

fails=0

# expect_match RE IDENTITY EXPECT(accept|reject) — bash =~ mirrors what cosign's
# --certificate-identity-regexp does (RE2 anchored the same way), so matching
# here matches there for these inputs.
expect_match() {
	local re="$1" identity="$2" expect="$3" got
	if [[ "$identity" =~ $re ]]; then got=accept; else got=reject; fi
	if [[ "$got" == "$expect" ]]; then
		printf 'ok   %-6s %s\n' "$expect" "$identity"
	else
		printf 'FAIL want=%s got=%s  %s\n' "$expect" "$got" "$identity" >&2
		fails=$((fails + 1))
	fi
}

# Accept: real tag-triggered release signatures.
expect_match "$id_re" "${base}@refs/tags/v1.2.3" accept
expect_match "$id_re" "${base}@refs/tags/v1.0.0" accept
expect_match "$id_re" "${base}@refs/tags/v10.20.30-rc.1" accept

# Reject: branch identities (the Q124 hole) and other non-tag refs.
expect_match "$id_re" "${base}@refs/heads/main" reject
expect_match "$id_re" "${base}@refs/heads/v1.2.3" reject
expect_match "$id_re" "${base}@refs/heads/attacker" reject
expect_match "$id_re" "${base}@refs/pull/1/merge" reject
# Reject: a tag that isn't a v* version, and a foreign workflow file.
expect_match "$id_re" "${base}@refs/tags/nightly" reject
expect_match "$id_re" "https://github.com/actions-gateway/github-actions-gateway/.github/workflows/evil.yml@refs/tags/v1.2.3" reject

if ((fails > 0)); then
	echo "verify-release-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "verify-release-test: all assertions passed"
