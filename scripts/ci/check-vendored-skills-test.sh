#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-vendored-skills.sh (Q890): a vendored file
# edited without its digest moving fails, one that matches passes, and every
# read that would leave the gate checking nothing refuses with rc 2.
#
# Both directions are asserted because this gate's failure mode is silence. It
# is a loop over the rows of a manifest, so a manifest it parsed as empty — a
# comment-only file, a path that is not there, a row it skipped — reports every
# file intact having compared nothing, and reads identically to a clean tree.
# The rc-2 cases are what separate those.
#
# The shipped manifest is asserted too, at the end. The fixture cases prove the
# checker works; only that one proves it is pointed at this repo's own vendored
# files and green over them.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/check-vendored-skills.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/vendored-skills-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

sha256_of() {
	local f="$1"
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$f" | awk '{print $1}'
	else
		shasum -a 256 "$f" | awk '{print $1}'
	fi
}

# fixture NAME — a throwaway git repo holding one vendored file, `tool.py`,
# whose manifest row is stamped to match it. Echoes the repo path. The checker
# takes its root from `git rev-parse`, so each case needs its own repo rather
# than its own directory.
fixture() {
	local dir="$FIXTURE_DIR/$1"
	mkdir -p "$dir/scripts"
	git -C "$dir" init -q
	printf 'print("hello")\n' > "$dir/scripts/tool.py"
	local sha
	sha="$(sha256_of "$dir/scripts/tool.py")"
	{
		printf '# a comment line, and a blank one below\n\n'
		printf 'scripts/tool.py\tskill/scripts/tool.py\tdeadbeef\t%s\t%s\n' "$sha" "$sha"
	} > "$dir/manifest.tsv"
	printf '%s\n' "$dir"
}

# expect NAME WANT_RC DESCRIPTION DIR ARG... — run the gate in DIR, compare rc.
expect() {
	local name="$1" want="$2" desc="$3" dir="$4" rc=0 out
	shift 4
	out="$(cd "$dir" && "$CHECKER" --manifest manifest.tsv "$@" 2>&1)" || rc=$?
	die_if_killed "$name" "$rc" "$want"
	if ((rc != want)); then
		printf 'FAIL: %s — %s: expected rc %d, got %d\n' "$name" "$desc" "$want" "$rc" >&2
		printf '%s\n' "$out" | sed 's/^/       /' >&2
		((fails++)) || true
		return
	fi
	printf 'ok   %s — %s\n' "$name" "$desc"
}

# expect_says NAME PATTERN DESCRIPTION DIR ARG... — run the gate and grep its
# combined output. Paired with expect: an rc alone cannot tell which row a
# finding was about, and a report that names the wrong one still exits 0.
expect_says() {
	local name="$1" pattern="$2" desc="$3" dir="$4" out
	shift 4
	out="$(cd "$dir" && "$CHECKER" --manifest manifest.tsv "$@" 2>&1)" || true
	if ! grep -q "$pattern" <<< "$out"; then
		printf 'FAIL: %s — %s: no match for %s\n' "$name" "$desc" "$pattern" >&2
		printf '%s\n' "$out" | sed 's/^/       /' >&2
		((fails++)) || true
		return
	fi
	printf 'ok   %s — %s\n' "$name" "$desc"
}

# --- the pair the gate exists for -----------------------------------------

clean="$(fixture clean)"
expect clean 0 "a file matching its declared digest passes" "$clean"
expect_says clean '1 file(s) match' "and says how many it compared" "$clean"

edited="$(fixture edited)"
printf 'print("forked here")\n' > "$edited/scripts/tool.py"
expect edited 1 "an edit with no digest change fails" "$edited"
expect_says edited 'scripts/tool.py has changed' "and names the file" "$edited"

# A declared file that is not there is the other way to break the pairing, and
# it is the one a `git mv` produces. Without this the loop skips the row and
# the count still reads plausible.
missing="$(fixture missing)"
rm "$missing/scripts/tool.py"
expect missing 1 "a declared file that is missing fails" "$missing"
expect_says missing 'declared in .* but missing' "and says so rather than skipping it" "$missing"

# --- the reads that must refuse rather than pass ---------------------------

empty="$(fixture empty)"
printf '# every line a comment\n' > "$empty/manifest.tsv"
expect empty 2 "a manifest declaring no files refuses" "$empty"

short="$(fixture short)"
printf 'scripts/tool.py\tskill/scripts/tool.py\tdeadbeef\n' >> "$short/manifest.tsv"
expect short 2 "a row missing a field refuses rather than skipping it" "$short"

absent="$(fixture absent)"
rm "$absent/manifest.tsv"
expect absent 2 "no manifest at all refuses" "$absent"

# --- --report tells a fork from an unmodified vendor -----------------------

# The two digests are equal in a fresh fixture, which is what `vendored` means.
report_clean="$(fixture report-clean)"
expect_says report-clean 'scripts/tool\.py  *vendored ' "--report calls an unmodified file vendored" "$report_clean" --report

# Move only the local digest, as a declared fork does.
report_forked="$(fixture report-forked)"
printf 'print("forked here")\n' > "$report_forked/scripts/tool.py"
(cd "$report_forked" && "$CHECKER" --manifest manifest.tsv --update > /dev/null)
expect_says report-forked 'scripts/tool\.py  *forked ' "--report calls a declared edit forked" "$report_forked" --report
expect report-forked 0 "and the gate passes once the fork is declared" "$report_forked"

# --- --update is the declaring step, not a bypass --------------------------

# It has to move the digest it is given and leave `vendored_sha256` alone, or a
# re-stamp would erase the record of what was adopted in the first place.
updated="$(fixture updated)"
before="$(awk -F'\t' '/^scripts/ {print $4}' "$updated/manifest.tsv")"
printf 'print("forked here")\n' > "$updated/scripts/tool.py"
(cd "$updated" && "$CHECKER" --manifest manifest.tsv --update > /dev/null)
after_vendored="$(awk -F'\t' '/^scripts/ {print $4}' "$updated/manifest.tsv")"
after_local="$(awk -F'\t' '/^scripts/ {print $5}' "$updated/manifest.tsv")"
if [[ "$after_vendored" != "$before" ]]; then
	printf 'FAIL: update — --update moved vendored_sha256, which records what was adopted\n' >&2
	((fails++)) || true
else
	printf 'ok   update — --update leaves vendored_sha256 alone\n'
fi
if [[ "$after_local" == "$before" ]]; then
	printf 'FAIL: update — --update did not move local_sha256\n' >&2
	((fails++)) || true
else
	printf 'ok   update — --update moves local_sha256 to what is on disk\n'
fi

# --- the shipped manifest --------------------------------------------------

rc=0
out="$("$CHECKER" 2>&1)" || rc=$?
die_if_killed shipped "$rc"
if ((rc != 0)); then
	printf 'FAIL: shipped — this repo'"'"'s own vendored files do not match their digests\n' >&2
	printf '%s\n' "$out" | sed 's/^/       /' >&2
	((fails++)) || true
else
	printf 'ok   shipped — this repo'"'"'s vendored files match their declared digests\n'
fi

# The count is asserted, not just the exit code: a manifest that lost its rows
# passes rc 0 above only because the rc-2 refusal is what catches it, and this
# pins the set the repo actually vendors.
if ! grep -q '4 file(s) match' <<< "$out"; then
	printf 'FAIL: shipped — expected 4 vendored files, got: %s\n' "$out" >&2
	((fails++)) || true
else
	printf 'ok   shipped — all 4 vendored files are declared\n'
fi

if ((fails > 0)); then
	printf 'check-vendored-skills-test: FAILED (%d)\n' "$fails" >&2
	exit 1
fi
printf 'check-vendored-skills-test: all checks passed\n'
