#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-tools.sh's version floors (Q751). Both
# directions are asserted throughout: a tool below its registered minimum must
# fail the check, and one at or above it must pass — a floor that stops firing
# is as silent a failure as one that fires on everything.
#
# Three mechanisms, each needing a different fixture:
#   * the comparison itself — measured through a stub `bash` on PATH rather than
#     by shipping old binaries. The stub shadows the interpreter too, which is
#     why the checker is launched through the bash already running this suite
#     instead of through the shebang.
#   * floor resolution (@ci:VAR, @go.work) — driven against a skeleton repo
#     holding a copy of the checker, since the real repo can only ever show
#     resolution succeeding.
#   * the version cmd field — checked against the real binaries where the host
#     has them, because whether `helm version --short` prints a number is a fact
#     about helm, not about this script.
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
version_case at-floor-4.4 'GNU bash, version 4.4.20(1)-release (x86_64-pc-linux-gnu)' 0 '(4.4.20, need 4.4+)'
version_case above-floor-5.3 'GNU bash, version 5.3.15(1)-release (aarch64-apple-darwin25.4.0)' 0 '(5.3.15, need 4.4+)'

# 4.10 is above 4.4 numerically and below it lexically; a string compare passes
# this case backwards.
version_case minor-compared-numerically 'GNU bash, version 4.10.0(1)-release' 0 '(4.10.0, need 4.4+)'

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
if (( real_rc == 0 )) && [[ "$real_out" == *"($real_version, need "* ]]; then
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
# The floor and the version cmd are a seventh and eighth field appended to rows
# that were written with six, so a row that mis-counts its empty fields would
# silently declare a floor of "gcloud components install …" and reject the tool
# forever.
bad_rows="$(awk '
	/^tools_registry\(\) \{/ { inreg = 1; next }
	inreg && /^EOF$/         { exit }
	inreg && /^[a-z]/ {
		n = split($0, f, "|")
		if (n < 6 || n > 8) { print "field count " n ": " $0 }
		else if (n >= 7 && f[7] != "" \
		         && f[7] !~ /^[0-9]+(\.[0-9]+)*$/ \
		         && f[7] !~ /^@ci:[A-Z0-9_]+$/ \
		         && f[7] != "@go.work") { print "bad min version: " $0 }
	}
' "$CHECKER")"
if [[ -z "$bad_rows" ]]; then
	pass registry-rows-well-formed
else
	fail registry-rows-well-formed "$bad_rows"
fi

# --- floor references against the real repo ---------------------------------
#
# Every @ci:/@go.work reference must name something the repo still declares. A
# pin that is renamed or moves out of .github/workflows/ leaves the tool
# silently unchecked, which is the failure this whole mechanism exists to avoid.
floors_rc=0
floors_out="$("$CHECKER" --floors 2>&1)" || floors_rc=$?
if (( floors_rc == 0 )) && [[ "$floors_out" != *UNRESOLVED* ]]; then
	pass real-floors-all-resolve "$(grep -c . <<<"$floors_out") declared"
else
	fail real-floors-all-resolve "rc=$floors_rc: $floors_out"
fi

# …and the reference forms must still be in use. Without this, a registry that
# quietly loses every reference passes the assertion above by having nothing
# left to resolve.
if [[ "$floors_out" == *"@ci:"* && "$floors_out" == *"@go.work"* ]]; then
	pass real-floors-use-references
else
	fail real-floors-use-references "no @ci:/@go.work reference left in the registry"
fi

# The go floor is the go.work directive, not a copy of it that drifted.
gowork_go="$(awk '$1 == "go" && $2 ~ /^[0-9]+(\.[0-9]+)+$/ { print $2; exit }' "$REPO_ROOT/go.work")"
reported_go="$(printf '%s\n' "$floors_out" | awk '$1 == "go" { print $2 }')"
if [[ -n "$gowork_go" && "$reported_go" == "$gowork_go" ]]; then
	pass go-floor-tracks-go-work "$gowork_go"
else
	fail go-floor-tracks-go-work "go.work says '${gowork_go:-none}', registry resolves '${reported_go:-none}'"
fi

# Every tool that declares a floor and is installed here must report a version.
# Five of them reject --version outright (go, kubectl, helm, kubeconform,
# polaris), and a floor over a probe that reports nothing rejects the tool
# forever — so the version cmd field is checked against the real binaries
# wherever the host has them.
unprobed=''
checked=0
while read -r tool _; do
	command -v "$tool" >/dev/null 2>&1 || continue
	checked=$((checked + 1))
	tool_out="$("$CHECKER" "$tool" 2>&1)" || true
	[[ "$tool_out" == *"no version reported"* ]] && unprobed+=" $tool"
done < <(printf '%s\n' "$floors_out")
if [[ -z "$unprobed" ]] && (( checked > 0 )); then
	pass installed-floors-report-a-version "$checked probed"
else
	fail installed-floors-report-a-version "unprobed:${unprobed:-none}, checked=$checked"
fi

# A declared version cmd must work whether or not the row also declares a floor
# — kubectl carries one with no floor today, and the reason to record it is that
# a floor added later lands on a probe already known to answer. Unverified, that
# is just a claim.
bad_cmds=''
cmds_checked=0
while IFS='|' read -r cmd_tool cmd_args; do
	[[ -n "$cmd_args" ]] || continue
	command -v "$cmd_tool" >/dev/null 2>&1 || continue
	cmds_checked=$((cmds_checked + 1))
	# shellcheck disable=SC2086  # the field is a literal argument list
	if ! "$cmd_tool" $cmd_args 2>/dev/null | grep -qE '[0-9]+(\.[0-9]+)+'; then
		bad_cmds+=" $cmd_tool"
	fi
done < <(awk '
	/^tools_registry\(\) \{/ { inreg = 1; next }
	inreg && /^EOF$/         { exit }
	inreg && /^[a-z]/ { n = split($0, f, "|"); if (n == 8) print f[1] "|" f[8] }
' "$CHECKER")
if [[ -z "$bad_cmds" ]] && (( cmds_checked > 0 )); then
	pass version-cmds-answer "$cmds_checked probed"
else
	fail version-cmds-answer "no version from:${bad_cmds:-none}, checked=$cmds_checked"
fi

# --- floor resolution, driven against a fixture repo ------------------------
#
# The real repo can only ever show resolution working. These cases own the other
# direction, and the pin values they turn on: a copy of the checker sits at the
# same depth inside a skeleton repo, so its repo_root lands on fixture go.work
# and fixture workflows instead of the real ones.
#
# fake_repo ROWS — write a skeleton repo whose registry is ROWS, and print the
# path of the checker copy inside it.
fake_repo() {
	local rows="$1" root="$FIXTURE_DIR/repo"
	rm -rf "$root"
	mkdir -p "$root/scripts/ci" "$root/.github/workflows"
	printf '%s\n' "$rows" >"$FIXTURE_DIR/rows.txt"
	printf 'go 1.23.4\n\nuse (\n\t./a\n)\n' >"$root/go.work"
	# Two workflows pin STUB_VERSION and disagree; the lower one is the floor,
	# because a floor above what some CI job runs rejects a working host.
	# OTHER_STUB_VERSION ends in the same text and is lower still: if the lookup
	# matched a suffix rather than the whole key, it would answer here.
	printf 'env:\n  STUB_VERSION: v2.5.0\n  OTHER_STUB_VERSION: v0.1.0\n' >"$root/.github/workflows/a.yml"
	printf 'env:\n  STUB_VERSION: v2.4.0\n' >"$root/.github/workflows/b.yml"
	awk -v rowsfile="$FIXTURE_DIR/rows.txt" '
		/^tools_registry\(\) \{$/ { print; getline; print
		                            while ((getline line < rowsfile) > 0) print line
		                            skip = 1; next }
		skip { if ($0 == "EOF") { skip = 0; print } ; next }
		{ print }
	' "$CHECKER" >"$root/scripts/ci/check-tools.sh"
	chmod +x "$root/scripts/ci/check-tools.sh"
	printf '%s' "$root/scripts/ci/check-tools.sh"
}

# floor_case NAME ROW EXPECT_RC EXPECT_SUBSTRING — resolve ROW's floor through a
# fixture repo and assert the --floors line and exit code.
floor_case() {
	local name="$1" row="$2" want_rc="$3" want_out="$4" got_rc=0 out checker
	checker="$(fake_repo "$row")"
	out="$("$REAL_BASH" "$checker" --floors 2>&1)" || got_rc=$?
	if [[ "$got_rc" != "$want_rc" ]]; then
		fail "$name" "want rc=$want_rc got rc=$got_rc ($out)"
	elif [[ "$out" != *"$want_out"* ]]; then
		fail "$name" "want '$want_out', got '$out'"
	else
		pass "$name" "$out"
	fi
}

floor_case ci-pin-lowest-of-several 'stubtool|required|||https://example.invalid||@ci:STUB_VERSION|' 0 'stubtool	2.4.0'
floor_case ci-pin-key-not-suffix 'stubtool|required|||https://example.invalid||@ci:OTHER_STUB_VERSION|' 0 'stubtool	0.1.0'
floor_case gowork-floor-read 'stubtool|required|||https://example.invalid||@go.work|' 0 'stubtool	1.23.4'
floor_case literal-floor-passes-through 'stubtool|required|||https://example.invalid||3.0|' 0 'stubtool	3.0'
floor_case unresolvable-pin-is-a-failure 'stubtool|required|||https://example.invalid||@ci:NO_SUCH_VERSION|' 1 'UNRESOLVED'

# An unresolvable reference must also be loud in the normal report, and must not
# leave the tool passing as though it had been checked.
checker="$(fake_repo 'stubtool|required|||https://example.invalid||@ci:NO_SUCH_VERSION|')"
broken_rc=0
broken_out="$("$REAL_BASH" "$checker" 2>&1)" || broken_rc=$?
if (( broken_rc != 0 )) && [[ "$broken_out" == *"PIN"*"does not resolve"* ]]; then
	pass unresolvable-pin-reported "rc=$broken_rc"
else
	fail unresolvable-pin-reported "rc=$broken_rc: $broken_out"
fi

# --- the version cmd field --------------------------------------------------
#
# A stub that answers only `report --short` stands in for the five real tools
# that reject --version. Both directions: with the field the floor is checked,
# without it the same binary reports nothing and is rejected — which is what a
# floor on an unprobed tool does, and why the field had to exist before any of
# those five could declare one.
{
	printf '#!/bin/sh\n'
	# shellcheck disable=SC2016  # this is the stub's source, not this script's
	printf 'if [ "$1" = "report" ] && [ "$2" = "--short" ]; then echo "stubtool v1.2.3"; else echo "unknown flag" >&2; exit 1; fi\n'
} >"$STUB_DIR/stubtool"
chmod +x "$STUB_DIR/stubtool"

checker="$(fake_repo 'stubtool|required|||https://example.invalid||1.0.0|report --short')"
vc_rc=0
vc_out="$(PATH="$STUB_DIR:$PATH" "$REAL_BASH" "$checker" 2>&1)" || vc_rc=$?
if (( vc_rc == 0 )) && [[ "$vc_out" == *"(1.2.3, need 1.0.0+)"* ]]; then
	pass version-cmd-consulted "1.2.3"
else
	fail version-cmd-consulted "rc=$vc_rc: $vc_out"
fi

checker="$(fake_repo 'stubtool|required|||https://example.invalid||1.0.0|')"
novc_rc=0
novc_out="$(PATH="$STUB_DIR:$PATH" "$REAL_BASH" "$checker" 2>&1)" || novc_rc=$?
if (( novc_rc == 1 )) && [[ "$novc_out" == *"no version reported"* ]]; then
	pass version-cmd-is-load-bearing "rc=$novc_rc"
else
	fail version-cmd-is-load-bearing "rc=$novc_rc: $novc_out"
fi

rm -f "$STUB_DIR/stubtool"

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
