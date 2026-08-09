#!/usr/bin/env bash
#
# Require the full errexit prologue — `set -euo pipefail` followed by
# `shopt -s inherit_errexit` — in every executable script under scripts/.
#
# `set -e` does not reach inside a command substitution: `modules="$(f)"` runs f
# past its own failure and takes the status of f's *last* command, so a builder
# that died on step one yields a truncated value and exit 0. That is this repo's
# worst bug class — a gate that checks nothing and reports success. Measured
# under Q733 on scripts/go/go-lint.sh, whose scoped_module_dirs feeds the lint
# scope: with a fault injected into it the gate announced a full sweep, linted 1
# module instead of 11, swallowed four 127s and exited 0. With the shopt it
# exits 127 at the first failure. Q670 is the same hole reached without an
# injected fault. The forms and their measured exit statuses are tabulated in
# docs/development/bash-style.md#set--e-stops-at-a-command-substitution.
#
# Scripts under a lib/ directory are exempt: they are sourced, never executed,
# so they carry no `set -euo pipefail` of their own and run under the caller's
# shell options — a caller's shopt already covers the functions they define.
# Declaring it in a sourced file would instead switch the option on for every
# caller, including ones that never opted in.
#
# The declaration-builtin half of the class (`local x="$(f)"`, which stays exit
# 0 even *with* inherit_errexit, because the builtin's own status replaces the
# substitution's) is not this gate's job — shellcheck's SC2155 already rejects
# it, and `make shellcheck` runs in the same `make check`.
#
# Backs `make errexit-prologue-check`. Takes explicit paths for testing;
# with no arguments it checks the whole scripts/ tree.
set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which the test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

REPO_ROOT="$(git rev-parse --show-toplevel)"

SET_LINE='set -euo pipefail'
SHOPT_LINE='shopt -s inherit_errexit'
# inherit_errexit is bash 4.4+; stock macOS still ships 3.2 at /bin/bash, where
# the shopt fails and `set -e` turns that into a non-zero exit. For a Claude Code
# PreToolUse hook that breaks a stronger contract than the one this gate
# enforces: a hook must never block a tool call, so it fails open on any bash.
# Only the registered hooks may swallow the shopt's own failure (Q733).
SHOPT_LINE_FAILOPEN='shopt -s inherit_errexit 2>/dev/null || true'

# --- file selection (asserted by check-errexit-prologue-test.sh) -------------

# script_candidates — the git-known candidate paths under scripts/. Untracked
# files count as long as they are not gitignored, so a brand-new script is
# covered by its own first `make check` rather than only once committed (Q432).
script_candidates() {
	git_candidates 'scripts/*.sh'
}

# prologue_verdict FILE — print one of ok / missing-shopt / missing-prologue /
# failopen-not-allowed.
#
# The first `set -euo pipefail` in a file is the script's own: later ones belong
# to heredoc-generated stub scripts, each of which opens with its own shebang.
#
# An unreadable file yields no line number and so reads as missing-prologue,
# which fails the gate. That is the safe direction: a file this cannot inspect
# must not pass.
prologue_verdict() {
	local file="$1" n
	n="$(grep -n -m1 -x -F "$SET_LINE" "$file" | cut -d: -f1 || true)"
	if [[ -z "$n" ]]; then
		# Sourced libraries run under the caller's options and declare neither.
		if [[ "$file" == */lib/* ]]; then
			printf 'ok\n'
		else
			printf 'missing-prologue\n'
		fi
		return 0
	fi
	# The shopt must be the next line that can actually execute. Comments and
	# blanks are skipped so a script can explain itself between the two, and
	# skipping them is safe: neither can contain a command substitution.
	local next
	next="$(awk -v start="$((n + 1))" 'NR >= start && !/^[[:space:]]*(#|$)/ { print; exit }' "$file")"
	if [[ "$next" == "$SHOPT_LINE" ]]; then
		printf 'ok\n'
	elif [[ "$next" == "$SHOPT_LINE_FAILOPEN" ]]; then
		# The fail-open form is a real reduction in protection, so it is allowed
		# only where the never-block contract outranks it.
		if [[ "$(basename "$file")" == claude-*-hook.sh ]]; then
			printf 'ok\n'
		else
			printf 'failopen-not-allowed\n'
		fi
	else
		printf 'missing-shopt\n'
	fi
}

main() {
	cd "$REPO_ROOT"

	local files=()
	if (( $# > 0 )); then
		files=("$@")
	else
		local selected
		selected="$(script_candidates | select_present_files)"
		if [[ -n "$selected" ]]; then
			mapfile -t files <<<"$selected"
		fi
		# A gate that reports success over an empty file set is the very defect
		# this one exists to catch.
		if (( ${#files[@]} == 0 )); then
			echo "check-errexit-prologue: no scripts found under scripts/ — the selection is broken" >&2
			return 1
		fi
	fi

	local file verdict bad=0
	for file in "${files[@]}"; do
		verdict="$(prologue_verdict "$file")"
		case "$verdict" in
		ok) ;;
		missing-shopt)
			echo "$file: '$SET_LINE' is not followed by '$SHOPT_LINE'" >&2
			bad=$((bad + 1))
			;;
		missing-prologue)
			echo "$file: no '$SET_LINE' prologue (only a sourced lib/ file may omit it)" >&2
			bad=$((bad + 1))
			;;
		failopen-not-allowed)
			echo "$file: the '2>/dev/null || true' form is for claude-*-hook.sh only; use '$SHOPT_LINE'" >&2
			bad=$((bad + 1))
			;;
		esac
	done

	if (( bad > 0 )); then
		echo >&2
		echo "$bad script(s) missing the errexit prologue. Add both lines at the top:" >&2
		echo "    $SET_LINE" >&2
		echo "    $SHOPT_LINE" >&2
		echo "See docs/development/bash-style.md#set--e-stops-at-a-command-substitution" >&2
		return 1
	fi
	echo "==> errexit prologue ok in ${#files[@]} script(s) under scripts/"
}

# Run main only when executed directly, so the test can source this file and
# exercise prologue_verdict without scanning the tree.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
