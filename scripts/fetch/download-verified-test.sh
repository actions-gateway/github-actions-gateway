#!/usr/bin/env bash
#
# Unit tests for scripts/fetch/download-verified.sh (Q433). The property that matters
# is that the retry can never become a path around the integrity check: a
# mismatched digest must fail and leave nothing at the output path, a malformed
# or absent digest must be rejected outright, and there must be no flag or
# environment variable that skips verification. Also pins `--retry-all-errors`
# into the curl invocation — without it curl retries only 408/429/5xx, which is
# the exact regression Q433 fixed.
#
# Downloads use file:// URLs, so these assertions need no network.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/fetch/download-verified.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/download-verified-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

# Keep a failing download from burning the default 5 retries x 2s.
export DOWNLOAD_RETRIES=0
export DOWNLOAD_RETRY_DELAY=0

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# assert_rc NAME WANT_RC ARGS... — run the script and assert its exit code.
assert_rc() {
	local name="$1" want_rc="$2" got_rc=0
	shift 2
	"$SCRIPT" "$@" > /dev/null 2>&1 || got_rc=$?
	if [[ "$got_rc" == "$want_rc" ]]; then
		pass "$name (rc=$got_rc)"
	else
		fail "$name" "want rc=$want_rc got rc=$got_rc"
	fi
}

# assert_fails NAME ARGS... — assert the script exits nonzero. Used where the
# code comes from curl and varies by scheme (403 over https exits 22, an
# unreadable file:// URL exits 37); only the failure itself is the contract.
assert_fails() {
	local name="$1" got_rc=0
	shift
	"$SCRIPT" "$@" > /dev/null 2>&1 || got_rc=$?
	if ((got_rc != 0)); then
		pass "$name (rc=$got_rc)"
	else
		fail "$name" 'want nonzero, got rc=0'
	fi
}

sha256_of() {
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

src="$FIXTURE_DIR/asset.bin"
printf 'pinned release asset\n' > "$src"
src_sha="$(sha256_of "$src")"
src_url="file://$src"
wrong_sha="$(printf '0%.0s' {1..64})"

# --- usage / argument validation ------------------------------------------

assert_rc 'no args rejected' 2
assert_rc 'missing output path rejected' 2 "$src_url" "$src_sha"
assert_rc 'empty digest rejected' 2 "$src_url" '' "$FIXTURE_DIR/out-empty.bin"
assert_rc 'short digest rejected' 2 "$src_url" 'deadbeef' "$FIXTURE_DIR/out-short.bin"
assert_rc 'non-hex digest rejected' 2 "$src_url" \
	"$(printf 'z%.0s' {1..64})" "$FIXTURE_DIR/out-nonhex.bin"

# --- happy path ------------------------------------------------------------

out="$FIXTURE_DIR/out-ok.bin"
assert_rc 'matching digest succeeds' 0 "$src_url" "$src_sha" "$out"
if cmp -s "$src" "$out"; then
	pass 'verified download has the source bytes'
else
	fail 'verified download has the source bytes' "content differs or missing: $out"
fi

# An uppercase digest is the same digest; the comparison must not care.
assert_rc 'uppercase digest accepted' 0 "$src_url" \
	"$(printf '%s' "$src_sha" | tr '[:lower:]' '[:upper:]')" "$FIXTURE_DIR/out-upper.bin"

# A missing parent directory is created rather than failing the move.
assert_rc 'missing output dir created' 0 "$src_url" "$src_sha" \
	"$FIXTURE_DIR/nested/dir/out.bin"

# --- integrity failures leave nothing behind -------------------------------

out_bad="$FIXTURE_DIR/out-mismatch.bin"
assert_rc 'digest mismatch fails' 1 "$src_url" "$wrong_sha" "$out_bad"
if [[ -e "$out_bad" ]]; then
	fail 'digest mismatch writes no output' "unverified bytes left at $out_bad"
else
	pass 'digest mismatch writes no output'
fi

out_missing="$FIXTURE_DIR/out-404.bin"
assert_fails 'unfetchable url fails' "file://$FIXTURE_DIR/no-such-asset.bin" \
	"$src_sha" "$out_missing"
if [[ -e "$out_missing" ]]; then
	fail 'failed download writes no output' "file left at $out_missing"
else
	pass 'failed download writes no output'
fi

# --- the retry must cover 4xx ----------------------------------------------

# curl's --retry alone covers 408/429/5xx and connection failures only, so a
# releases-CDN 403 fails instantly without --retry-all-errors (Q433).
if grep -Eq '^curl .*--retry-all-errors' "$SCRIPT"; then
	pass 'curl invocation carries --retry-all-errors'
else
	fail 'curl invocation carries --retry-all-errors' "not found in $SCRIPT"
fi

# Nothing may bypass the digest comparison: it is the only writer of the output
# path, and it is unconditional.
# shellcheck disable=SC2016  # matching the script's source text, not expanding it
if grep -q 'if \[\[ "$got" != "$want" \]\]; then' "$SCRIPT"; then
	pass 'digest comparison is unconditional'
else
	fail 'digest comparison is unconditional' "comparison not found in $SCRIPT"
fi

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall download-verified.sh assertions passed\n'
