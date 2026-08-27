#!/usr/bin/env bash
#
# Assert what verify-pages-artifact.sh catches (Q1000).
#
# The positive control is the incident's own shape: the tree gh-pages held
# BEFORE the v1.6.0 mike commit — 1.5.0 carrying `stable`, no 1.6.0 anywhere —
# with a root index.html and a CNAME beside it. That artifact passed the check
# this script replaces, which asserted only those two files, so the suite fails
# the old check by construction and the new one has to earn its pass.
#
# The intermediate shape is a control in its own right, and it is not
# hypothetical: mike writes the version and the alias in two commits, so the run
# genuinely passed through a tree listing 1.6.0 with `stable` still on 1.5.0
# (gh-pages 5f8cd7940). An artifact assembled there is wrong in a way a
# version-only assertion cannot see.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

SCRIPT="$REPO_ROOT/scripts/pages/verify-pages-artifact.sh"
FIXTURE_DIR="$REPO_ROOT/tmp/verify-pages-artifact-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s: %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# site NAME VERSIONS_JSON DIR... — build an artifact fixture. Every fixture gets
# the root index.html and CNAME the old check asserted, so nothing here can pass
# for the reason that check would have passed.
site() {
	local name="$1" versions="$2"
	shift 2
	local root="$FIXTURE_DIR/$name"
	rm -rf "$root"
	mkdir -p "$root"
	echo '<meta http-equiv="refresh" content="0; url=stable/">' > "$root/index.html"
	echo 'actions-gateway.com' > "$root/CNAME"
	printf '%s\n' "$versions" > "$root/versions.json"
	local d
	for d in "$@"; do
		mkdir -p "$root/$d"
		echo "<html>$d</html>" > "$root/$d/index.html"
	done
	printf '%s' "$root"
}

# expect NAME EXPECTED_RC ARGS... — run the script and compare exit status.
expect() {
	local name="$1" want="$2"
	shift 2
	local got=0
	"$SCRIPT" "$@" > "$FIXTURE_DIR/out.log" 2>&1 || got=$?
	if ((got == want)); then
		pass "$name"
	else
		fail "$name" "expected exit $want, got $got: $(tr '\n' ' ' < "$FIXTURE_DIR/out.log")"
	fi
}

CORRECT='[{"version":"dev","title":"dev (main)","aliases":[]},
{"version":"1.6.0","title":"1.6.0","aliases":["stable"]},
{"version":"1.5.0","title":"1.5.0","aliases":[]}]'

STALE='[{"version":"dev","title":"dev (main)","aliases":[]},
{"version":"1.5.0","title":"1.5.0","aliases":["stable"]}]'

HALF='[{"version":"dev","title":"dev (main)","aliases":[]},
{"version":"1.6.0","title":"1.6.0","aliases":[]},
{"version":"1.5.0","title":"1.5.0","aliases":["stable"]}]'

# --- the tree the run meant to deploy ---
root="$(site correct "$CORRECT" dev 1.6.0 1.5.0 stable)"
expect "a correct artifact passes" 0 --site "$root" --version 1.6.0 --alias stable

# --- the positive control: gh-pages as it stood before the mike commit ---
root="$(site stale "$STALE" dev 1.5.0 stable)"
expect "a stale artifact fails" 1 --site "$root" --version 1.6.0 --alias stable
if grep -q 'no 1.6.0/index.html' "$FIXTURE_DIR/out.log" &&
	grep -q 'does not list 1.6.0' "$FIXTURE_DIR/out.log"; then
	pass "the stale artifact is reported as missing the version and its tree"
else
	fail "the stale artifact is reported as missing the version and its tree" \
		"got: $(tr '\n' ' ' < "$FIXTURE_DIR/out.log")"
fi

# --- the intermediate commit: version present, alias still on the old release ---
root="$(site half "$HALF" dev 1.6.0 1.5.0 stable)"
expect "an artifact whose alias is still on the previous release fails" 1 \
	--site "$root" --version 1.6.0 --alias stable
if grep -q "does not put the 'stable' alias on 1.6.0" "$FIXTURE_DIR/out.log"; then
	pass "the misplaced alias names the version actually carrying it"
else
	fail "the misplaced alias names the version actually carrying it" \
		"got: $(tr '\n' ' ' < "$FIXTURE_DIR/out.log")"
fi

# --- versions.json can be right while the tree is not ---
root="$(site listed-only "$CORRECT" dev 1.5.0 stable)"
expect "a version listed but not built fails" 1 --site "$root" --version 1.6.0 --alias stable

root="$(site no-alias-dir "$CORRECT" dev 1.6.0 1.5.0)"
expect "a claimed alias with no directory fails" 1 --site "$root" --version 1.6.0 --alias stable

# --- a backport claims no alias, and must not be failed for the one it left alone ---
root="$(site backport "$HALF" dev 1.6.0 1.5.0 stable)"
expect "a deploy claiming no alias ignores where stable sits" 0 --site "$root" --version 1.6.0

# The workflow passes the alias mike claimed verbatim, so a deploy that claimed
# none calls this with an empty --alias rather than omitting the flag.
root="$(site empty-alias "$HALF" dev 1.6.0 1.5.0 stable)"
expect "an explicitly empty --alias is not a claim" 0 --site "$root" --version 1.6.0 --alias ""
root="$(site dev-deploy "$HALF" dev 1.6.0 1.5.0 stable)"
expect "a dev deploy asserts its own tree" 0 --site "$root" --version dev --alias ""
rm -rf "${root:?}/dev"
expect "a dev deploy with no dev tree fails" 1 --site "$root" --version dev --alias ""

# --- the two assertions this check inherited ---
root="$(site no-root "$CORRECT" dev 1.6.0 1.5.0 stable)"
rm "$root/index.html"
expect "a missing root index.html still fails" 1 --site "$root" --version 1.6.0 --alias stable

root="$(site no-cname "$CORRECT" dev 1.6.0 1.5.0 stable)"
rm "$root/CNAME"
expect "a missing CNAME still fails" 1 --site "$root" --version 1.6.0 --alias stable

# --- an unreadable versions.json is unproven, not clean ---
root="$(site bad-json 'not json at all' dev 1.6.0 1.5.0 stable)"
expect "an unparseable versions.json fails" 1 --site "$root" --version 1.6.0 --alias stable

root="$(site no-json "$CORRECT" dev 1.6.0 1.5.0 stable)"
rm "$root/versions.json"
expect "a missing versions.json fails" 1 --site "$root" --version 1.6.0 --alias stable

# --- usage ---
expect "a missing --version is a usage error" 2 --site "$FIXTURE_DIR/correct"

if ((fails > 0)); then
	echo "verify-pages-artifact-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "verify-pages-artifact-test: all assertions passed"
