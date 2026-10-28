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
if [[ -n "${STUB_COSIGN_SLEEP:-}" ]]; then exec sleep "$STUB_COSIGN_SLEEP"; fi
exit "${STUB_COSIGN_RC:-0}"
STUB
chmod +x "$FIXTURE_DIR/cosign"

# Stub docker and gh: an index with two arches and one attestation manifest (or
# amd64 alone under STUB_NO_ARM64), and
# a Release carrying both blob bundles unless STUB_BUNDLES overrides the list.
mkdir -p "$FIXTURE_DIR/bin"
cat > "$FIXTURE_DIR/bin/docker" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${STUB_NO_ARM64:-}" ]]; then
	echo '{"manifests":[{"digest":"sha256:aaaa","platform":{"os":"linux","architecture":"amd64"}}]}'
	exit 0
fi
cat << 'JSON'
{"manifests":[
 {"digest":"sha256:aaaa","platform":{"os":"linux","architecture":"amd64"}},
 {"digest":"sha256:bbbb","platform":{"os":"linux","architecture":"arm64"}},
 {"digest":"sha256:cccc","platform":{"os":"unknown","architecture":"unknown"}}]}
JSON
STUB
cat > "$FIXTURE_DIR/bin/gh" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "$2" in
	view) printf '%s\n' ${STUB_BUNDLES-actions-gateway-crds-v2.yaml.cosign.bundle SHA256SUMS.cosign.bundle} ;;
	download)
		dir="" pats=()
		while (($#)); do
			case "$1" in
				--dir) dir="$2"; shift ;;
				--pattern) pats+=("$2"); shift ;;
			esac
			shift
		done
		for p in "${pats[@]}"; do : > "$dir/$p"; done ;;
esac
STUB
chmod +x "$FIXTURE_DIR/bin/docker" "$FIXTURE_DIR/bin/gh"

out_text=""
out_rc=0

# run_verify ARGS... — run verify-release.sh against the stub, capturing the
# combined output in $out_text and the exit code in $out_rc.
run_verify() {
	out_rc=0
	: > "$COSIGN_LOG"
	out_text="$(PATH="$FIXTURE_DIR/bin:$PATH" COSIGN="$FIXTURE_DIR/cosign" \
		STUB_COSIGN_LOG="$COSIGN_LOG" "$SCRIPT" "$@" 2>&1)" || out_rc=$?
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

# Every published artifact publish.yml signs must be checked: each image, its
# SBOM attestation on every per-arch manifest (never the attestation manifest),
# the chart artifacts at the tagless chart version, and each blob bundle.
want_refs=""
for img in gmc agc proxy worker wrapper build-runner; do
	want_refs+="verify ghcr.io/actions-gateway/$img:v1.2.3"$'\n'
	want_refs+="verify-attestation ghcr.io/actions-gateway/$img@sha256:aaaa"$'\n'
	want_refs+="verify-attestation ghcr.io/actions-gateway/$img@sha256:bbbb"$'\n'
done
want_refs+='verify ghcr.io/actions-gateway/charts/actions-gateway:1.2.3
verify ghcr.io/actions-gateway/charts/actions-gateway-crds-v2:1.2.3
verify-blob actions-gateway-crds-v2.yaml
verify-blob SHA256SUMS'
got_refs="$(awk '{ref = $NF; if ($1 == "verify-blob") sub(/.*\//, "", ref); print $1, ref}' "$COSIGN_LOG")"
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

# An attestation check that skipped --type would accept any predicate, and a
# blob check must name the bundle that signs the subject it verifies.
if grep -q '^verify-attestation .*--type spdxjson ' "$COSIGN_LOG" &&
	! grep '^verify-attestation ' "$COSIGN_LOG" | grep -qv -- '--type spdxjson '; then
	pass 'every attestation check requires the spdxjson predicate'
else
	fail 'every attestation check requires the spdxjson predicate' "calls:"$'\n'"$(< "$COSIGN_LOG")"
fi
if grep -q -- '--bundle [^ ]*/SHA256SUMS.cosign.bundle [^ ]*/SHA256SUMS$' "$COSIGN_LOG"; then
	pass 'a blob check pairs each bundle with its own subject'
else
	fail 'a blob check pairs each bundle with its own subject' "calls:"$'\n'"$(< "$COSIGN_LOG")"
fi

# An index that lost a platform fails, though every manifest it has verifies.
STUB_NO_ARM64=1 run_verify v1.2.3
expect_rc 'an index without linux/arm64 fails' 1

# A release missing the CRD manifest bundle fails, rather than verifying the
# bundles it does carry and reporting green.
STUB_BUNDLES='SHA256SUMS.cosign.bundle' run_verify v1.2.3
expect_rc 'a release without the CRD bundle fails' 1

# One bad signature must fail the run — the per-artifact OK/FAIL is printed, not
# returned, so the aggregate exit code is the only thing a release cut reads.
STUB_COSIGN_RC=1 run_verify v1.2.3
expect_rc 'a failed signature fails the run' 1
if [[ "$out_text" == *"signature verification FAILED"* ]]; then
	pass 'a failed signature is reported'
else
	fail 'a failed signature is reported' "output: $out_text"
fi

# A check that never returns must fail the run rather than stall it: a hung
# Docker credential helper holds cosign past its own --timeout with no output.
STUB_COSIGN_SLEEP=30 COSIGN_TIMEOUT=1 run_verify v1.2.3
expect_rc 'a hung signature check fails the run' 1
if [[ "$out_text" == *"TIMEOUT"* && "$out_text" == *"credential helper"* ]]; then
	pass 'a hung signature check is reported as a timeout'
else
	fail 'a hung signature check is reported as a timeout' "output: $out_text"
fi

if ((fails > 0)); then
	echo "verify-release-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "verify-release-test: all assertions passed"
