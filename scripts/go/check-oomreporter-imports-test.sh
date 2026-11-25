#!/usr/bin/env bash
#
# Assertions for the oomreporter import gate (Q1403), against throwaway fixture
# repos: the red directions cannot be staged in this tree. Runs under
# `make check` (via `make scripts-test`).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

GATE="$REPO_ROOT/scripts/go/check-oomreporter-imports.sh"
WORK="$REPO_ROOT/tmp/check-oomreporter-imports-test.$$"
trap 'rm -rf "$WORK"' EXIT INT TERM
mkdir -p "$WORK"

M=github.com/actions-gateway/github-actions-gateway
fails=0

# assert_gate WANT_RC DESCRIPTION [FILE TEXT] — a repo that passes, with TEXT
# appended to FILE, or without FILE where TEXT is empty.
assert_gate() {
	local want="$1" desc="$2" file="${3:-}" text="${4:-}" dir="$WORK/${RANDOM}${RANDOM}" rc=0 out
	mkdir -p "$dir/cmd/oomreporter/internal/a" "$dir/cmd/agc" "$dir/vendor/x"
	printf 'module %s/oomreporter\n' "$M" >"$dir/cmd/oomreporter/go.mod"
	printf 'package main\n\nimport (\n\t"fmt"\n\n\t"%s/oomreporter/internal/a"\n)\n' "$M" >"$dir/cmd/oomreporter/main.go"
	printf 'module %s/agc\n\nrequire (\n\t%s/api v0.0.0\n)\n' "$M" "$M" >"$dir/cmd/agc/go.mod"
	printf 'package main\n\nimport "%s/api/v2"\n\n// Not an import: "%s/oomreporter".\n' "$M" "$M" >"$dir/cmd/agc/main.go"
	printf 'package x\n\nimport "%s/oomreporter"\n' "$M" >"$dir/vendor/x/x.go"
	if [[ -n "$text" ]]; then
		printf '%s\n' "$text" >>"$dir/$file"
	elif [[ -n "$file" ]]; then
		rm -r "${dir:?}/${file:?}"
	fi
	git -C "$dir" init -q .
	git -C "$dir" add -A
	out="$(cd "$dir" && "$GATE" 2>&1)" || rc=$?
	die_if_killed "$desc" "$rc" "$want"
	if [[ "$rc" != "$want" ]]; then
		printf 'FAIL: %s: want exit %s, got %s\n%s\n' "$desc" "$want" "$rc" "$out" >&2
		fails=$((fails + 1))
	fi
}

assert_gate 0 "nothing imports across"
assert_gate 1 "the reporter imports a module" cmd/oomreporter/internal/a/a.go "import \"$M/api/v2\""
assert_gate 1 "by another name" cmd/oomreporter/main.go "import v2 \"$M/api/v2\""
assert_gate 1 "for its effect" cmd/oomreporter/main.go "import _ \"$M/api/v2\""
assert_gate 1 "in a block" cmd/oomreporter/main.go "	\"$M/githubapp\""
assert_gate 1 "between backquotes" cmd/oomreporter/main.go "import \`$M/githubapp\`"
assert_gate 1 "behind a build tag" cmd/oomreporter/x_test.go "//go:build integration

import \"$M/broker\""
assert_gate 1 "one whose name starts as its own" cmd/oomreporter/main.go "import \"$M/oomreporter2\""
assert_gate 1 "the reporter requires a module" cmd/oomreporter/go.mod "require $M/api v0.0.0"
assert_gate 1 "a module imports the reporter" cmd/agc/main.go "import \"$M/oomreporter\""
assert_gate 1 "a package of it" cmd/agc/x.go "import r \"$M/oomreporter/internal/report\""
assert_gate 1 "a module requires the reporter" cmd/agc/go.mod "	$M/oomreporter v0.0.0"
assert_gate 0 "another whose name starts as the reporter's" cmd/agc/x.go "import \"$M/oomreporter2\""
assert_gate 2 "no module but the reporter" cmd/agc
assert_gate 2 "no reporter" cmd/oomreporter

if ((fails > 0)); then
	echo "$fails oomreporter import gate assertion(s) failed" >&2
	exit 1
fi
echo "all oomreporter import gate assertions passed"
