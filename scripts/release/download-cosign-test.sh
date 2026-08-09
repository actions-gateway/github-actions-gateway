#!/usr/bin/env bash
#
# Execute scripts/release/download-cosign.sh end to end (Q605). `make
# verify-release` builds on this script and nothing ran it: the helper it shells
# out to is resolved at runtime, so when Q571 moved download-verified.sh into
# scripts/fetch/ the download died at "No such file or directory" and the break
# surfaced only at the v1.3.0-rc.4 cut. shellcheck cannot see a path assembled at
# runtime; only running the script can.
#
# curl is stubbed with a fixture-serving shim, so these assertions need no
# network. The download then necessarily ends at a digest mismatch — a pinned
# cosign binary has no preimage to serve — and that is the signal: only
# download-verified.sh reports a mismatch, so reaching it proves the helper
# resolved, ran, and failed closed.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/release/download-cosign.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/download-cosign-test.$$"
STUB_DIR="$FIXTURE_DIR/bin"
ARCH_STUB_DIR="$FIXTURE_DIR/bin-arch"
mkdir -p "$STUB_DIR" "$ARCH_STUB_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

# Keep a stubbed failure from burning download-verified.sh's default retries.
export DOWNLOAD_RETRIES=0
export DOWNLOAD_RETRY_DELAY=0

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# The version under test is the one the Makefile pins, so a COSIGN_VERSION bump
# that forgets to add the new digests fails here instead of at a release cut.
version="$(awk '/^COSIGN_VERSION[[:space:]]*\?=/ {print $3}' Makefile)"
if [[ -z "$version" ]]; then
	echo "download-cosign-test: no COSIGN_VERSION pin found in Makefile" >&2
	exit 1
fi

# The platform the pin table is expected to cover, derived independently of the
# script under test.
host_arch="$(uname -m)"
case "$host_arch" in
	aarch64 | arm64) host_arch=arm64 ;;
	x86_64 | amd64) host_arch=amd64 ;;
	*)
		echo "download-cosign-test: unsupported host arch $host_arch" >&2
		exit 1
		;;
esac
platform="$(uname -s | tr '[:upper:]' '[:lower:]')-$host_arch"

CURL_LOG="$FIXTURE_DIR/curl-urls.log"
export STUB_CURL_LOG="$CURL_LOG"

# Stub curl: log every URL requested and serve a fixture. Handles only the flags
# download-verified.sh passes.
cat > "$STUB_DIR/curl" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
out=""
while (($#)); do
	case "$1" in
		-o) out="$2"; shift 2 ;;
		--retry | --retry-delay) shift 2 ;;
		-*) shift ;;
		*)
			printf '%s\n' "$1" >> "$STUB_CURL_LOG"
			shift
			;;
	esac
done
printf 'stand-in for the cosign binary\n' > "$out"
STUB
chmod +x "$STUB_DIR/curl"

# Stub uname for the architecture gate only, in its own directory so the other
# cases still see the real host.
cat > "$ARCH_STUB_DIR/uname" << 'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-m" ]]; then
	echo ppc64le
else
	echo Linux
fi
STUB
chmod +x "$ARCH_STUB_DIR/uname"

STUB_PATH="$STUB_DIR"
out_text=""
out_rc=0

# run ARGS... — run the script with $STUB_PATH prepended to PATH, capturing the
# combined output in $out_text and the exit code in $out_rc.
run() {
	out_rc=0
	out_text="$(PATH="$STUB_PATH:$PATH" "$SCRIPT" "$@" 2>&1)" || out_rc=$?
}

assert_rc() {
	local name="$1" want="$2"
	if [[ "$out_rc" == "$want" ]]; then
		pass "$name (rc=$out_rc)"
	else
		fail "$name" "want rc=$want got rc=$out_rc; output: $out_text"
	fi
}

assert_contains() {
	local name="$1" needle="$2"
	if [[ "$out_text" == *"$needle"* ]]; then
		pass "$name"
	else
		fail "$name" "want output containing '$needle', got: $out_text"
	fi
}

assert_not_contains() {
	local name="$1" needle="$2"
	if [[ "$out_text" != *"$needle"* ]]; then
		pass "$name"
	else
		fail "$name" "want output without '$needle', got: $out_text"
	fi
}

# --- argument validation ---------------------------------------------------

run
assert_rc 'no args rejected' 2
run "$FIXTURE_DIR/cosign-noversion"
assert_rc 'missing version rejected' 2

# --- fail-closed gates -----------------------------------------------------

run "$FIXTURE_DIR/cosign-unpinned" v0.0.0-not-pinned
assert_rc 'version with no pinned digest rejected' 1
assert_contains 'unpinned version names the pin table' 'no pinned SHA256'

STUB_PATH="$ARCH_STUB_DIR"
run "$FIXTURE_DIR/cosign-badarch" "$version"
assert_rc 'unmappable architecture rejected' 1
assert_contains 'unmappable architecture is named' 'unsupported arch ppc64le'
STUB_PATH="$STUB_DIR"

# --- the download path executes --------------------------------------------

: > "$CURL_LOG"
out_bin="$FIXTURE_DIR/cosign"
run "$out_bin" "$version"

# The mismatch message comes from download-verified.sh, so it can only appear if
# the helper path resolved and the helper ran. A moved helper exits 127 here.
assert_contains 'the download reaches download-verified.sh' 'SHA256 mismatch'
assert_rc 'stubbed bytes fail the integrity check' 1

# The Makefile's pin must have a digest for the platform running this test.
assert_not_contains "COSIGN_VERSION $version has a digest for $platform" 'no pinned SHA256'

want_url="https://github.com/sigstore/cosign/releases/download/$version/cosign-$platform"
got_url="$(< "$CURL_LOG")"
if [[ "$got_url" == "$want_url" ]]; then
	pass 'requests the pinned release asset for this platform'
else
	fail 'requests the pinned release asset for this platform' \
		"want $want_url, got ${got_url:-<no request>}"
fi

# An unverified binary must never land at $(COSIGN): the Makefile would take it
# for a built target and never download again.
if [[ -e "$out_bin" ]]; then
	fail 'a failed download leaves no binary behind' "unverified bytes left at $out_bin"
else
	pass 'a failed download leaves no binary behind'
fi

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall download-cosign.sh assertions passed\n'
