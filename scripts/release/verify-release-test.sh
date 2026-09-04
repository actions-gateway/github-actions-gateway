#!/usr/bin/env bash
#
# Assert two properties of `make verify-release`.
#
# 1. The release signing-identity regexp (release_identity_regexp in
#    scripts/lib/common.sh, consumed by verify-release.sh) is tags-only: it must
#    ACCEPT a `refs/tags/vX.Y.Z` identity and REJECT a `refs/heads/...` branch
#    identity. This is the regression guard for Q124 — before the fix the
#    pattern matched `refs/(tags|heads)/.*`, so a release signature minted from a
#    branch (a workflow_dispatch run on a scratch branch) verified as legitimate.
# 2. verify-release.sh itself runs, against a stub cosign: which artifacts it
#    checks, that every check carries the identity and issuer constraints, and
#    that one failed signature fails the run. Q605 — asserting the regexp alone
#    left the script unexecuted, which is how a runtime path break in its cosign
#    download reached a release cut (the download half is covered by
#    download-cosign-test.sh).
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

id_re="$(release_identity_regexp)"
base='https://github.com/actions-gateway/github-actions-gateway/.github/workflows/publish.yml'

FIXTURE_DIR="$REPO_ROOT/tmp/verify-release-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

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

# --- verify-release.sh, run against a stub cosign --------------------------

SCRIPT="$REPO_ROOT/scripts/release/verify-release.sh"
COSIGN_LOG="$FIXTURE_DIR/cosign-calls.log"

# Stub cosign: log each invocation's flags and ref, then exit with
# $STUB_COSIGN_RC so the caller's failure path is reachable too.
cat > "$FIXTURE_DIR/cosign" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$STUB_COSIGN_LOG"
exit "${STUB_COSIGN_RC:-0}"
STUB
chmod +x "$FIXTURE_DIR/cosign"

out_text=""
out_rc=0

# run_verify ARGS... — run verify-release.sh against the stub, capturing the
# combined output in $out_text and the exit code in $out_rc.
run_verify() {
	out_rc=0
	: > "$COSIGN_LOG"
	out_text="$(COSIGN="$FIXTURE_DIR/cosign" STUB_COSIGN_LOG="$COSIGN_LOG" \
		"$SCRIPT" "$@" 2>&1)" || out_rc=$?
}

expect_rc() {
	local name="$1" want="$2"
	die_if_killed "$name" "$out_rc" "$want"
	if [[ "$out_rc" == "$want" ]]; then
		pass "$name (rc=$out_rc)"
	else
		fail "$name" "want rc=$want got rc=$out_rc; output: $out_text"
	fi
}

run_verify
expect_rc 'missing version rejected' 2

run_verify v1.2.3
expect_rc 'every signature verified passes' 0

# Every published artifact publish.yml signs must be checked, and the chart
# artifacts take the tagless chart version.
want_refs='ghcr.io/actions-gateway/gmc:v1.2.3
ghcr.io/actions-gateway/agc:v1.2.3
ghcr.io/actions-gateway/proxy:v1.2.3
ghcr.io/actions-gateway/worker:v1.2.3
ghcr.io/actions-gateway/wrapper:v1.2.3
ghcr.io/actions-gateway/build-runner:v1.2.3
ghcr.io/actions-gateway/charts/actions-gateway:1.2.3
ghcr.io/actions-gateway/charts/actions-gateway-crds-v2:1.2.3'
got_refs="$(awk '{print $NF}' "$COSIGN_LOG")"
if [[ "$got_refs" == "$want_refs" ]]; then
	pass 'verifies every signed artifact at the release version'
else
	fail 'verifies every signed artifact at the release version' \
		"want:"$'\n'"$want_refs"$'\n'"got:"$'\n'"$got_refs"
fi

# A check that drops either constraint would accept a signature from any
# identity or any issuer, which is the whole property Q124 hardened. Compared as
# literal strings: the identity constraint is itself a regexp, so a pattern match
# here would be matching it against itself.
issuer='https://token.actions.githubusercontent.com'
unconstrained=0
while IFS= read -r call; do
	if [[ "$call" == *"--certificate-identity-regexp $id_re "* &&
		"$call" == *"--certificate-oidc-issuer $issuer "* ]]; then
		continue
	fi
	unconstrained=$((unconstrained + 1))
done < "$COSIGN_LOG"
if ((unconstrained == 0)); then
	pass 'every check pins the signing identity and the OIDC issuer'
else
	fail 'every check pins the signing identity and the OIDC issuer' \
		"$unconstrained call(s) missing a constraint; calls:"$'\n'"$(< "$COSIGN_LOG")"
fi

# One bad signature must fail the run — the per-artifact OK/FAIL is printed, not
# returned, so the aggregate exit code is the only thing a release cut reads.
STUB_COSIGN_RC=1 run_verify v1.2.3
expect_rc 'a failed signature fails the run' 1
if [[ "$out_text" == *"signature verification FAILED"* ]]; then
	pass 'a failed signature is reported'
else
	fail 'a failed signature is reported' "output: $out_text"
fi

if ((fails > 0)); then
	echo "verify-release-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "verify-release-test: all assertions passed"
