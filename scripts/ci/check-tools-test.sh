#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-tools.sh's version floor (Q751). Both
# directions are asserted: a tool below its registered minimum must fail the
# check, and one at or above it must pass — a floor that stops firing is as
# silent a failure as one that fires on everything.
#
# The version is measured through a stub `bash` on PATH rather than by shipping
# old binaries, so the comparison itself is exercised end to end. The stub
# shadows the interpreter too, which is why the checker is launched through the
# bash already running this suite instead of through the shebang.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/check-tools.sh"
REAL_BASH="$BASH"

FIXTURE_DIR="$REPO_ROOT/tmp/check-tools-test.$$"
STUB_DIR="$FIXTURE_DIR/bin"
mkdir -p "$STUB_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

pass() { printf 'ok   %-32s %s\n' "$1" "${2:-}"; }
fail() { printf 'FAIL %-32s %s\n' "$1" "$2" >&2; fails=$((fails + 1)); }

# --- the declared bash floor ------------------------------------------------
#
# version_case NAME BANNER EXPECT_RC [EXPECT_SUBSTRING] — plant a `bash` stub
# that reports BANNER for --version, check only the bash row, and assert the
# exit code (and, when given, a substring of the report).
version_case() {
	local name="$1" banner="$2" want_rc="$3" want_out="${4:-}" got_rc=0 out
	{
		printf '#!/bin/sh\n'
		printf 'printf "%%s\\n" %q\n' "$banner"
	} >"$STUB_DIR/bash"
	chmod +x "$STUB_DIR/bash"
	out="$(PATH="$STUB_DIR:$PATH" "$REAL_BASH" "$CHECKER" --required bash 2>&1)" || got_rc=$?
	if [[ "$got_rc" != "$want_rc" ]]; then
		fail "$name" "want rc=$want_rc got rc=$got_rc"
		return 0
	fi
	if [[ -n "$want_out" && "$out" != *"$want_out"* ]]; then
		fail "$name" "report does not mention '$want_out'"
		return 0
	fi
	pass "$name" "rc=$got_rc"
}

# The floor is 4.4, where inherit_errexit arrived.
version_case below-floor-3.2 'GNU bash, version 3.2.57(1)-release (arm64-apple-darwin25)' 1 'need 4.4+'
version_case below-floor-4.3 'GNU bash, version 4.3.48(1)-release (x86_64-pc-linux-gnu)' 1 'need 4.4+'
version_case at-floor-4.4 'GNU bash, version 4.4.20(1)-release (x86_64-pc-linux-gnu)' 0 '(4.4.20)'
version_case above-floor-5.3 'GNU bash, version 5.3.15(1)-release (aarch64-apple-darwin25.4.0)' 0 '(5.3.15)'

# 4.10 is above 4.4 numerically and below it lexically; a string compare passes
# this case backwards.
version_case minor-compared-numerically 'GNU bash, version 4.10.0(1)-release' 0 '(4.10.0)'

# The banner names the build platform after the version, so taking the last
# match — or every match — reads darwin25.4.0 as the version. The 5.3.15 case
# above only proves the right one was picked because this one exists: a floor
# read off the platform would clear 4.4 too.
version_case platform-suffix-not-the-version 'GNU bash, version 3.2.57(1)-release (arm64-apple-darwin25.4.0)' 1 'need 4.4+'

# A tool that reports nothing parseable is not silently accepted.
version_case unparseable-version-rejected 'bash' 1 'no version reported'

rm -f "$STUB_DIR/bash"

# The real bash this suite runs under must clear the floor, and the report must
# name the version it read — the whole point of declaring one.
real_rc=0
real_out="$("$CHECKER" --required bash 2>&1)" || real_rc=$?
real_version="$(printf '%s.%s.%s' "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}" "${BASH_VERSINFO[2]}")"
if (( real_rc == 0 )) && [[ "$real_out" == *"($real_version)"* ]]; then
	pass real-bash-clears-floor "$real_version"
else
	fail real-bash-clears-floor "rc=$real_rc, report did not name $real_version"
fi

# A tool with no declared floor is reported without a version, so nothing pays a
# `--version` fork for a check that is not being made.
nofloor_out="$("$CHECKER" --required git 2>&1)" || true
if [[ "$nofloor_out" == *"OK   git"* && "$nofloor_out" != *"OK   git"*"("*")"* ]]; then
	pass no-floor-no-version-probe
else
	fail no-floor-no-version-probe "git row carries a version: $nofloor_out"
fi

# --- registry shape ---------------------------------------------------------
#
# The minimum version is a seventh field appended to rows that were written with
# six, so a row that mis-counts its empty fields would silently declare a floor
# of "gcloud components install …" and reject the tool forever.
bad_rows="$(awk '
	/^tools_registry\(\) \{/ { inreg = 1; next }
	inreg && /^EOF$/         { exit }
	inreg && /^[a-z]/ {
		n = split($0, f, "|")
		if (n < 6 || n > 7) { print "field count " n ": " $0 }
		else if (n == 7 && f[7] != "" && f[7] !~ /^[0-9]+(\.[0-9]+)*$/) { print "bad min version: " $0 }
	}
' "$CHECKER")"
if [[ -z "$bad_rows" ]]; then
	pass registry-rows-well-formed
else
	fail registry-rows-well-formed "$bad_rows"
fi

# --- the pre-prologue bash guard --------------------------------------------
#
# `shopt -s inherit_errexit` is itself bash 4.4+, so the checker cannot reach its
# own registry on the shell it exists to report. The guard is therefore above
# the prologue, and being above it is the whole property.
guard_line="$(grep -n 'BASH_VERSINFO' "$CHECKER" | head -1 | cut -d: -f1)"
prologue_line="$(grep -n -m1 -x -F 'set -euo pipefail' "$CHECKER" | cut -d: -f1)"
if [[ -n "$guard_line" && -n "$prologue_line" ]] && (( guard_line < prologue_line )); then
	pass guard-precedes-prologue "line $guard_line < $prologue_line"
else
	fail guard-precedes-prologue "guard at '${guard_line:-none}', prologue at '${prologue_line:-none}'"
fi

# Measured rather than inferred where the host has a pre-4.4 bash to measure
# with: stock macOS keeps 3.2 at /bin/bash, Linux runners do not ship one, and
# there the ordering assertion above is the whole coverage.
old_bash=''
for candidate in /bin/bash /usr/bin/bash; do
	[[ -x "$candidate" ]] || continue
	if ! "$candidate" -c 'shopt -s inherit_errexit' 2>/dev/null; then
		old_bash="$candidate"
		break
	fi
done
if [[ -n "$old_bash" ]]; then
	old_rc=0
	old_out="$("$old_bash" "$CHECKER" 2>&1)" || old_rc=$?
	if (( old_rc != 0 )) && [[ "$old_out" == *'bash 4.4+'* ]]; then
		pass old-bash-reports-the-floor "$old_bash"
	else
		fail old-bash-reports-the-floor "rc=$old_rc, output: $old_out"
	fi
else
	printf 'skip %-32s no pre-4.4 bash on this host\n' old-bash-reports-the-floor
fi

if (( fails > 0 )); then
	echo "check-tools-test: $fails failure(s)" >&2
	exit 1
fi
echo "check-tools-test: all assertions passed"
