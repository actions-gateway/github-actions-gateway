#!/usr/bin/env bash
#
# Unit tests for the build half of scripts/lib/merge-driver-common.sh — the one
# part of the shared entry point no driver suite can see, because a driver suite
# only ever runs against a build that worked.
#
# Both cases are Q822's. Every invocation of a ported driver rebuilds
# devtools/git/mergedriver into one shared .build/mergedriver, and `make
# scripts-test` runs the scriptindex and planindex suites concurrently, so
# placements and execs of that path overlap. Three CI sightings and one local
# one reported `the driver could not be built` with no cause, because the
# build's output went to /dev/null.
#
# The fixture is its own tiny Go module rather than devtools/, so a build can be
# made to fail on demand without touching the real driver, and a successful one
# costs a compile of one file.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
LIB="$REPO_ROOT/scripts/lib/merge-driver-common.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/merge-driver-common-test.$$"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

ok() { echo "ok   $1"; }
bad() {
	echo "FAIL $1" >&2
	[[ -n "${2:-}" ]] && printf '     %s\n' "$2" >&2
	fails=$((fails + 1))
}

# The lib derives the repo root from its own BASH_SOURCE and builds
# $root/devtools ./git/mergedriver, so the fixture reproduces exactly that
# shape: a copy of the lib under scripts/lib/, and a module under devtools/.
mkdir -p "$FIXTURE_DIR/scripts/lib" "$FIXTURE_DIR/devtools/git/mergedriver"
cp "$LIB" "$FIXTURE_DIR/scripts/lib/merge-driver-common.sh"
cat >"$FIXTURE_DIR/devtools/go.mod" <<'MOD'
module fixture

go 1.26.6
MOD

# source_lib — the lib, sourced from the fixture tree, with the one global its
# note function reads.
source_lib() {
	cat <<'PRELUDE'
DRIVER_LOG='fixture-driver'
DRIVER_NAME='fixture'
PRELUDE
	printf '. %q\n' "$FIXTURE_DIR/scripts/lib/merge-driver-common.sh"
}

good_source() { printf 'package main\n\nfunc main() {}\n' >"$FIXTURE_DIR/devtools/git/mergedriver/main.go"; }
bad_source() { printf 'package main\n\nfunc main() { this is not go }\n' >"$FIXTURE_DIR/devtools/git/mergedriver/main.go"; }


# --- the shared path is replaced, never written through ----------------------------
#
# This is the whole fix, and the assertion that can tell it from the old
# behaviour. Renaming gives the shared path a new inode on every build, so a
# process already exec'ing the old one is never disturbed. The window it closes
# is go's cross-filesystem placement, which unlinks and streams a copy: for the
# length of that stream the path holds an incomplete file open for writing, and
# a concurrent exec gets ETXTBSY (measured on go1.26.8, 4,106 of 4,109).
#
# It asserts a cache *hit*: an unchanged source. Under the old code a cache hit
# wrote nothing at all, so the inode stood still; a changed source relinks, and
# go renames there itself, so a test that edited the source in between would
# pass either way and prove nothing.

good_source
BIN="$FIXTURE_DIR/bin/mergedriver"
ERR="$FIXTURE_DIR/build.err"
# A hard link stands in for the suite that is mid-exec: it holds whichever inode
# the path had when the link was made, so `-ef` after the rebuild answers
# "replaced or rewritten?" with no `stat`/`ls` portability to get wrong.
SENTINEL="$FIXTURE_DIR/bin/held-open"

build_rc=0
{ source_lib; printf '_merge_driver_build %q %q\n' "$BIN" "$ERR"; } | bash || build_rc=$?
if (( build_rc != 0 )); then
	bad 'the fixture builds' "rc=$build_rc: $(cat "$ERR")"
else
	ln "$BIN" "$SENTINEL"
	build_rc=0
	{ source_lib; printf '_merge_driver_build %q %q\n' "$BIN" "$ERR"; } | bash || build_rc=$?
	if (( build_rc != 0 )); then
		bad 'a cached rebuild succeeds' "rc=$build_rc: $(cat "$ERR")"
	elif [[ "$BIN" -ef "$SENTINEL" ]]; then
		bad 'a cached rebuild replaces the shared path' \
			'the rebuild left the inode standing, so the shared path is not being replaced'
	else
		ok 'a cached rebuild replaces the shared path rather than leaving it standing'
	fi
	rm -f "$SENTINEL"
fi

# No temp may outlive the build, or a wide fan-out litters .build/ with one
# per driver invocation.
if compgen -G "$FIXTURE_DIR/bin/*.build.*" >/dev/null; then
	bad 'a successful build leaves no temp behind' "$(ls "$FIXTURE_DIR/bin")"
else
	ok 'a successful build leaves no temp behind'
fi

# --- a failed build says why -------------------------------------------------

bad_source
build_rc=0
{ source_lib; printf '_merge_driver_build %q %q\n' "$BIN" "$ERR"; } | bash || build_rc=$?
if (( build_rc == 0 )); then
	bad 'a broken source fails the build' 'the build reported success'
elif ! grep -q 'mergedriver' "$ERR"; then
	bad "a failed build keeps go's output" "got: $(head -3 "$ERR")"
else
	ok "a failed build keeps go's output instead of discarding it"
fi

if compgen -G "$FIXTURE_DIR/bin/*.build.*" >/dev/null; then
	bad 'a failed build leaves no temp behind' "$(ls "$FIXTURE_DIR/bin")"
else
	ok 'a failed build leaves no temp behind'
fi

# --- the fallback relays that output, and still leaves markers ---------------
#
# The reason a reader gets is the driver's only account of the failure: git
# discards the driver's stdout and shows stderr. Before Q822 it was `the driver
# could not be built` and nothing else, which named a symptom and cost three CI
# sightings that could not say more.

printf 'base\n' >"$FIXTURE_DIR/base"
printf 'ours\n' >"$FIXTURE_DIR/ours"
printf 'theirs\n' >"$FIXTURE_DIR/theirs"

fb_rc=0
{
	source_lib
	printf '_merge_driver_unbuilt_fallback %q %q %q 7 %q %q\n' \
		"$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs" \
		'the/merged/file' "$ERR"
} | bash 2>"$FIXTURE_DIR/fb.err" || fb_rc=$?

if (( fb_rc != 1 )); then
	bad 'an unbuildable driver reports a conflict' "want rc=1, got rc=$fb_rc"
elif (( fb_rc > 128 )); then
	bad 'the fallback exit status stays under 128' "git would read rc=$fb_rc as a crash"
else
	ok 'an unbuildable driver reports a conflict rather than crashing'
fi

if grep -q 'the build failed with:' "$FIXTURE_DIR/fb.err" && grep -q 'mergedriver' "$FIXTURE_DIR/fb.err"; then
	ok "the fallback relays the build's own output on stderr"
else
	bad "the fallback relays the build's own output on stderr" \
		"got: $(head -3 "$FIXTURE_DIR/fb.err")"
fi

if grep -q '<<<<<<<' "$FIXTURE_DIR/ours"; then
	ok 'the fallback leaves ordinary conflict markers'
else
	bad 'the fallback leaves ordinary conflict markers' "$(cat "$FIXTURE_DIR/ours")"
fi

# A build can fail with no output at all, and the reason must survive that.
fb_rc=0
printf 'ours\n' >"$FIXTURE_DIR/ours"
: >"$FIXTURE_DIR/empty.err"
{
	source_lib
	printf '_merge_driver_unbuilt_fallback %q %q %q 7 %q %q\n' \
		"$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs" \
		'the/merged/file' "$FIXTURE_DIR/empty.err"
} | bash 2>"$FIXTURE_DIR/fb2.err" || fb_rc=$?
if (( fb_rc == 1 )) && grep -q 'could not be built' "$FIXTURE_DIR/fb2.err"; then
	ok 'the reason survives a build that failed silently'
else
	bad 'the reason survives a build that failed silently' \
		"rc=$fb_rc: $(head -3 "$FIXTURE_DIR/fb2.err")"
fi

if (( fails > 0 )); then
	echo "merge-driver-common-test: $fails failure(s)" >&2
	exit 1
fi
echo 'merge-driver-common-test: all assertions passed'
