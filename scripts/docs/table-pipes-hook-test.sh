#!/usr/bin/env bash
#
# Unit tests for hooks/table_pipes.py — the MkDocs hook that renders `\|` inside
# a table cell's code span as `|`, the way GitHub does, and warns (a failed
# `--strict` build) on any `\|` a table cell's code still renders. A missed
# rewrite publishes a PromQL query that matches a literal pipe (Q1269).
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# python3 is an extended-tier prerequisite (scripts/ci/check-tools.sh), not a
# required one, so this skips rather than fails when it is absent. CI runners
# always have it.
#
# The fixtures are Markdown, whose backticks are literal here.
# shellcheck disable=SC2016
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
readonly REPO_ROOT
readonly HOOK="$REPO_ROOT/hooks/table_pipes.py"

if ! command -v python3 >/dev/null 2>&1; then
	printf 'skip table-pipes-hook-test: python3 not found (extended tier, scripts/ci/check-tools.sh)\n'
	exit 0
fi

fails=0

# call FUNC INPUT — run one of the hook's pure functions over INPUT.
call() {
	python3 - "$HOOK" "$1" "$2" <<-'PY'
		import importlib.util, sys

		hook, func, text = sys.argv[1:4]
		spec = importlib.util.spec_from_file_location("table_pipes", hook)
		mod = importlib.util.module_from_spec(spec)
		spec.loader.exec_module(mod)
		out = getattr(mod, func)(text)
		sys.stdout.write(out if isinstance(out, str) else "\n".join(out))
	PY
}

# expect NAME WANT FUNC INPUT
expect() {
	local name="$1" want="$2" got
	got="$(call "$3" "$4")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s:\n  want=%q\n   got=%q\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# --- unescape: rewritten ------------------------------------------------------

expect 'code span in a table row' \
	'| `a{c=~"agc|proxy"}` | x |' unescape '| `a{c=~"agc\|proxy"}` | x |'
expect 'every pipe in the span' \
	'| `arm|check|cleanup` |' unescape '| `arm\|check\|cleanup` |'
expect 'double-backtick span' \
	'| ``a|b`` |' unescape '| ``a\|b`` |'
expect 'indented row (admonition body)' \
	'    | `a|b` |' unescape '    | `a\|b` |'
expect 'escaped backslash keeps one, as on GitHub' \
	'| `\|` |' unescape '| `\\|` |'

# --- unescape: left alone -----------------------------------------------------

expect 'escape outside a code span' \
	'| a\|b | `c` |' unescape '| a\|b | `c` |'
expect 'prose line, not a row' \
	'see `a\|b` here' unescape 'see `a\|b` here'
expect 'escaped backtick opens no span, as on GitHub' \
	'| \`x\|y` | z |' unescape '| \`x\|y` | z |'
expect 'escaped backslash before a backtick still opens one' \
	'| \\`x|y` |' unescape '| \\`x\|y` |'
expect 'table-shaped line inside a fence' \
	$'```\n| `a\\|b` |\n```' unescape $'```\n| `a\\|b` |\n```'
expect 'row after the fence closes is rewritten' \
	$'~~~\nx\n~~~\n| `a|b` |' unescape $'~~~\nx\n~~~\n| `a\\|b` |'

# --- escaped_pipes: the check -------------------------------------------------

expect 'flags \| in a cell'"'"'s code' \
	'agc\|proxy' escaped_pipes '<table><tr><td><code>agc\|proxy</code></td></tr></table>'
expect 'flags a header cell' \
	'a\|b' escaped_pipes '<tr><th class="x"><code>a\|b</code></th></tr>'
expect 'passes an unescaped pipe' \
	'' escaped_pipes '<td><code>agc|proxy</code></td>'
expect 'passes code outside a table' \
	'' escaped_pipes '<p><code>a\|b</code></p>'
expect 'passes the &#124; spelling of a literal backslash-pipe' \
	'' escaped_pipes '<td><code>\&#124;</code></td>'

if ((fails > 0)); then
	printf '%d table-pipes-hook test(s) failed\n' "$fails" >&2
	exit 1
fi
printf 'all table-pipes-hook tests passed\n'
