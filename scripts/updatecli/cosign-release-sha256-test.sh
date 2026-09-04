#!/usr/bin/env bash
#
# Unit tests for scripts/updatecli/cosign-release-sha256.sh (Q927).
#
# This script decides, unattended and weekly, which SHA-256 digests land in
# scripts/release/download-cosign.sh — the pin that decides which bytes are
# allowed to become the binary verifying every release. The property that makes
# it safe to automate is that it reads those digests out of a checksums file it
# has verified against a pinned sigstore identity, and prints nothing at all if
# that verification fails. Neither half shows up in review of the manifest.
#
# So the assertions here are: the identity and issuer are actually passed to
# `cosign verify-blob` (not merely written in a comment), a failed verification
# produces an empty stdout rather than a digest, and the digest that is printed
# comes from the checksums file rather than from hashing whatever was
# downloaded.
#
# `curl` is stubbed on PATH to serve fixture assets and `cosign` is stubbed
# through $COSIGN, so the whole path runs with no network and no Rekor lookup.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/updatecli/cosign-release-sha256.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/cosign-release-sha256-test.$$"
STUB_DIR="$FIXTURE_DIR/bin"
ASSET_DIR="$FIXTURE_DIR/assets"
mkdir -p "$STUB_DIR" "$ASSET_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# --- fixtures --------------------------------------------------------------

# A stand-in release: two of the four platforms, so an absent one is testable.
# The digests are fixture values no real bytes hash to, which is what makes
# "read from the checksums file" distinguishable from "hash the download".
DARWIN_SHA='1111111111111111111111111111111111111111111111111111111111111111'
LINUX_SHA='2222222222222222222222222222222222222222222222222222222222222222'
cat > "$ASSET_DIR/cosign_checksums.txt" << EOF
$DARWIN_SHA  cosign-darwin-arm64
$LINUX_SHA  cosign-linux-amd64
3333333333333333333333333333333333333333333333333333333333333333  cosign_checksums.txt
EOF
printf 'stand-in certificate\n' > "$ASSET_DIR/cosign_checksums.txt-keyless.pem"
printf 'stand-in signature\n' > "$ASSET_DIR/cosign_checksums.txt-keyless.sig"

# --- stubs -----------------------------------------------------------------

# curl: serve $ASSET_DIR/<basename of the URL> to the -o path, or fail like a
# 404 when the release has no such asset.
cat > "$STUB_DIR/curl" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
out=""
url=""
while (($#)); do
	case "$1" in
		-o) out="$2"; shift 2 ;;
		--retry | --retry-delay) shift 2 ;;
		-*) shift ;;
		*) url="$1"; shift ;;
	esac
done
src="$STUB_ASSET_DIR/${url##*/}"
[[ -f "$src" ]] || exit 22
cp "$src" "$out"
STUB
chmod +x "$STUB_DIR/curl"

# cosign: log the arguments it was called with, announce on stderr as the real
# one does, and exit $STUB_COSIGN_RC so a case can make the verification fail.
cat > "$STUB_DIR/cosign" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" > "$STUB_COSIGN_ARGS"
if [[ "${STUB_COSIGN_RC:-0}" != 0 ]]; then
	echo "stub cosign: simulated verification failure" >&2
	exit "$STUB_COSIGN_RC"
fi
echo "Verified OK" >&2
STUB
chmod +x "$STUB_DIR/cosign"

export STUB_ASSET_DIR="$ASSET_DIR"
export STUB_COSIGN_ARGS="$FIXTURE_DIR/cosign-args.log"
export COSIGN="$STUB_DIR/cosign"

out_text=""
err_text=""
out_rc=0

# run ARGS... — run the script with the stubs on PATH, capturing stdout, stderr
# and the exit code separately. stdout is the source value updatecli would take,
# so it is asserted apart from the diagnostics.
run() {
	out_rc=0
	: > "$STUB_COSIGN_ARGS"
	PATH="$STUB_DIR:$PATH" "$SCRIPT" "$@" \
		> "$FIXTURE_DIR/stdout" 2> "$FIXTURE_DIR/stderr" || out_rc=$?
	out_text="$(< "$FIXTURE_DIR/stdout")"
	err_text="$(< "$FIXTURE_DIR/stderr")"
}

assert_rc() {
	local name="$1" want="$2"
	die_if_killed "$name" "$out_rc" "$want"
	if [[ "$out_rc" == "$want" ]]; then
		pass "$name (rc=$out_rc)"
	else
		fail "$name" "want rc=$want got rc=$out_rc; stderr: $err_text"
	fi
}

assert_stdout() {
	local name="$1" want="$2"
	if [[ "$out_text" == "$want" ]]; then
		pass "$name"
	else
		fail "$name" "want stdout '$want', got '$out_text'"
	fi
}

assert_stderr_contains() {
	local name="$1" needle="$2"
	if [[ "$err_text" == *"$needle"* ]]; then
		pass "$name"
	else
		fail "$name" "want stderr containing '$needle', got: $err_text"
	fi
}

# --- argument and precondition validation ----------------------------------

run
assert_rc 'no args rejected' 1
run v2.5.2
assert_rc 'missing platform rejected' 1

COSIGN="$FIXTURE_DIR/no-such-cosign" run v2.5.2 darwin-arm64
assert_rc 'missing cosign rejected' 1
assert_stderr_contains 'missing cosign names how to get it' 'make cosign'

# --- the digest comes from the signed checksums file -----------------------

run v2.5.2 darwin-arm64
assert_rc 'a verified release resolves' 0
assert_stdout 'the digest is the checksums line, not a hash of the bytes' "$DARWIN_SHA"
run v2.5.2 linux-amd64
assert_stdout 'each platform gets its own line' "$LINUX_SHA"

# No trailing newline: updatecli substitutes a shell source verbatim, so one
# would land inside the pin table's quoted digest.
if [[ "$(wc -c < "$FIXTURE_DIR/stdout" | tr -d ' ')" == 64 ]]; then
	pass 'the digest is printed with no trailing newline'
else
	fail 'the digest is printed with no trailing newline' \
		"want 64 bytes, got $(wc -c < "$FIXTURE_DIR/stdout" | tr -d ' ')"
fi

# --- the signature check is real -------------------------------------------

# The whole reason this may run unattended: the checksums file is verified
# against a pinned identity before a digest is read out of it. A verify-blob
# call missing either constraint verifies that the file was signed by *someone*.
run v2.5.2 darwin-arm64
mapfile -t cosign_args < "$STUB_COSIGN_ARGS"

# assert_arg_pair FLAG VALUE — the flag is present and the argument right after
# it is VALUE. Checking the two words independently would pass on a call that
# had swapped the identity and the issuer.
assert_arg_pair() {
	local flag="$1" want="$2" i
	for ((i = 0; i < ${#cosign_args[@]} - 1; i++)); do
		if [[ "${cosign_args[i]}" == "$flag" ]]; then
			if [[ "${cosign_args[i + 1]}" == "$want" ]]; then
				pass "verify-blob is constrained by $flag $want"
			else
				fail "verify-blob is constrained by $flag $want" \
					"$flag was given '${cosign_args[i + 1]}'"
			fi
			return
		fi
	done
	fail "verify-blob is constrained by $flag $want" \
		"$flag absent; cosign args were: ${cosign_args[*]}"
}

if [[ "${cosign_args[0]:-}" == verify-blob ]]; then
	pass 'the checksums file is verified, not merely downloaded'
else
	fail 'the checksums file is verified, not merely downloaded' \
		"cosign args were: ${cosign_args[*]}"
fi
assert_arg_pair --certificate-identity keyless@projectsigstore.iam.gserviceaccount.com
assert_arg_pair --certificate-oidc-issuer https://accounts.google.com

# A release signed by anyone else, or tampered with, must yield no digest at all
# — updatecli would otherwise pin whatever the CDN served.
STUB_COSIGN_RC=1 run v2.5.2 darwin-arm64
assert_rc 'a failed verification is fatal' 1
assert_stdout 'a failed verification prints no digest' ''
assert_stderr_contains 'a failed verification names the expected signer' \
	'keyless@projectsigstore.iam.gserviceaccount.com'

# --- fail-closed on a missing platform or asset ----------------------------

run v2.5.2 linux-arm64
assert_rc 'a platform absent from the checksums file is fatal' 1
assert_stdout 'a platform absent from the checksums file prints no digest' ''
assert_stderr_contains 'the absent platform is named' 'cosign-linux-arm64'

mv "$ASSET_DIR/cosign_checksums.txt-keyless.sig" "$ASSET_DIR/.sig-hidden"
run v2.5.2 darwin-arm64
assert_rc 'a release with no signature asset is fatal' 22
assert_stdout 'a release with no signature asset prints no digest' ''
mv "$ASSET_DIR/.sig-hidden" "$ASSET_DIR/cosign_checksums.txt-keyless.sig"

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall cosign-release-sha256.sh assertions passed\n'
