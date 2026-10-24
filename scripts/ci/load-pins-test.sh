#!/usr/bin/env bash
#
# Unit tests for scripts/ci/load-pins.sh (Q1166): a well-formed pins file lands
# in both $GITHUB_ENV and $GITHUB_OUTPUT, and every line that could set
# something other than a pin — a key outside the three suffixes, a value
# carrying whitespace or shell syntax, a duplicate — fails the job with nothing
# written. The real scripts/ci/pins.env is loaded too, so a malformed edit to it
# reddens here rather than in the first workflow that reads it.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
LOADER="$REPO_ROOT/scripts/ci/load-pins.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/load-pins-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

out_rc=0
err_text=""

# load NAME FILE — run the loader over FILE with fresh GITHUB_ENV/GITHUB_OUTPUT
# files under the fixture dir, keeping its exit code and stderr.
load() {
	local name="$1" file="$2"
	: > "$FIXTURE_DIR/$name.env"
	: > "$FIXTURE_DIR/$name.out"
	out_rc=0
	GITHUB_ENV="$FIXTURE_DIR/$name.env" GITHUB_OUTPUT="$FIXTURE_DIR/$name.out" \
		"$LOADER" "$file" > /dev/null 2> "$FIXTURE_DIR/$name.err" || out_rc=$?
	err_text="$(< "$FIXTURE_DIR/$name.err")"
}

# expect_rejected NAME LINE — a pins file holding LINE beside a good pin fails,
# names the offending line, and writes nothing to either file: a partial write
# would export the good pin and leave the job running on the rest unpinned.
expect_rejected() {
	local name="$1" line="$2"
	printf 'KIND_VERSION=v0.32.0\n%s\n' "$line" > "$FIXTURE_DIR/$name.pins"
	load "$name" "$FIXTURE_DIR/$name.pins"
	die_if_killed "$name" "$out_rc" 1
	if ((out_rc != 1)); then
		fail "$name" "want rc=1, got rc=$out_rc"
	elif [[ -s "$FIXTURE_DIR/$name.env" || -s "$FIXTURE_DIR/$name.out" ]]; then
		fail "$name" "rejected file still wrote: $(cat "$FIXTURE_DIR/$name.env")"
	elif [[ "$err_text" != *":2:"* ]]; then
		fail "$name" "want the error to name line 2, got: $err_text"
	else
		pass "$name"
	fi
}

# --- a good file lands in both places ----------------------------------------

cat > "$FIXTURE_DIR/good.pins" << 'EOF'
# a comment, then a blank line

KIND_VERSION=v0.32.0
KIND_BINARY_SHA256=50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54
BUILDKIT_IMAGE=moby/buildkit:buildx-stable-1@sha256:0168606be2315b7c807a03b3d8aa79beefdb31c98740cebdffdfeebf31190c9f
ENVTEST_K8S_VERSION=1.35.x
EOF
load good "$FIXTURE_DIR/good.pins"
want="$(grep -v -e '^#' -e '^$' "$FIXTURE_DIR/good.pins")"
if ((out_rc != 0)); then
	fail 'a well-formed file loads' "rc=$out_rc: $err_text"
elif [[ "$(< "$FIXTURE_DIR/good.env")" != "$want" ]]; then
	fail 'every pin reaches GITHUB_ENV' "got: $(< "$FIXTURE_DIR/good.env")"
elif [[ "$(< "$FIXTURE_DIR/good.out")" != "$want" ]]; then
	fail 'every pin reaches GITHUB_OUTPUT' "got: $(< "$FIXTURE_DIR/good.out")"
else
	pass 'a well-formed file reaches GITHUB_ENV and GITHUB_OUTPUT, comments dropped'
fi

# A last line with no trailing newline is still a pin.
printf 'KIND_VERSION=v0.32.0' > "$FIXTURE_DIR/nonl.pins"
load nonl "$FIXTURE_DIR/nonl.pins"
if ((out_rc == 0)) && [[ "$(< "$FIXTURE_DIR/nonl.env")" == 'KIND_VERSION=v0.32.0' ]]; then
	pass 'a final line without a newline is loaded'
else
	fail 'a final line without a newline is loaded' "rc=$out_rc env=$(< "$FIXTURE_DIR/nonl.env")"
fi

# --- anything but a pin is refused -------------------------------------------

expect_rejected path-is-not-a-pin 'PATH=/tmp/evil'
expect_rejected ld-preload-is-not-a-pin 'LD_PRELOAD=/tmp/evil.so'
expect_rejected lowercase-key 'kind_version=v0.32.0'
expect_rejected value-with-space 'SYFT_VERSION=v1 v2'
# shellcheck disable=SC2016
expect_rejected value-with-command-substitution 'SYFT_VERSION=$(id)'
expect_rejected inline-comment 'SYFT_VERSION=v1.45.1 # bumped'
expect_rejected empty-value 'SYFT_VERSION='
expect_rejected quoted-value 'SYFT_VERSION="v1.45.1"'
expect_rejected duplicate-key 'KIND_VERSION=v0.33.0'

printf '# only a comment\n\n' > "$FIXTURE_DIR/empty.pins"
load empty "$FIXTURE_DIR/empty.pins"
if ((out_rc == 1)) && [[ "$err_text" == *'pins nothing'* ]]; then
	pass 'a file that pins nothing is refused'
else
	fail 'a file that pins nothing is refused' "rc=$out_rc: $err_text"
fi

# --- outside a job it refuses rather than writing nowhere ---------------------

out_rc=0
env -u GITHUB_ENV -u GITHUB_OUTPUT "$LOADER" "$FIXTURE_DIR/good.pins" \
	> /dev/null 2> "$FIXTURE_DIR/nojob.err" || out_rc=$?
if ((out_rc != 0)) && grep -q 'GITHUB_ENV is unset' "$FIXTURE_DIR/nojob.err"; then
	pass 'outside a job it refuses'
else
	fail 'outside a job it refuses' "rc=$out_rc: $(< "$FIXTURE_DIR/nojob.err")"
fi

# --- the committed pins file loads --------------------------------------------

load real "$REPO_ROOT/scripts/ci/pins.env"
if ((out_rc == 0)) && [[ -s "$FIXTURE_DIR/real.env" ]]; then
	pass "scripts/ci/pins.env loads ($(wc -l < "$FIXTURE_DIR/real.env" | tr -d ' ') pins)"
else
	fail 'scripts/ci/pins.env loads' "rc=$out_rc: $err_text"
fi

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall load-pins.sh assertions passed\n'
