#!/usr/bin/env bash
#
# Fail when cmd/oomreporter imports another module of this repository, or one of
# them imports it (Q1403). The reporter is a tool of its own with its own image
# and chart, and moving it to a repository of its own stays a move only while
# nothing imports across.
#
# Reads the tracked Go files as text, so a file behind a build tag counts too,
# and every go.mod. Refuses with rc 2 where it finds no import on either side.
#
# Usage: scripts/go/check-oomreporter-imports.sh
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

PREFIX='github\.com/actions-gateway/github-actions-gateway/'
OWN='oomreporter(["`/ ]|$)'

# imports PATHSPEC... — file:line:text of each import or requirement of a module
# of this repository. git grep exits 1 for no match, which the caller judges.
imports() {
	local rc=0
	git grep -nE "^[[:space:]]*((import|require)[[:space:]]+)?([A-Za-z0-9_.]+[[:space:]]+)?[\"\`]?${PREFIX}" -- "$@" ':!vendor' ':!*/vendor' || rc=$?
	((rc <= 1)) || return "$rc"
}

inside="$(imports 'cmd/oomreporter/*.go' cmd/oomreporter/go.mod)"
outside="$(imports '*.go' '*go.mod' ':!cmd/oomreporter')"
if [[ -z "$inside" || -z "$outside" ]]; then
	echo "check-oomreporter-imports: found no import to read on one side, so this checks nothing" >&2
	exit 2
fi

rc=0
crossing="$(grep -vE "${PREFIX}${OWN}" <<<"$inside" || true)"
if [[ -n "$crossing" ]]; then
	printf 'cmd/oomreporter imports another module of this repository:\n%s\n' "$crossing" >&2
	rc=1
fi
crossing="$(grep -E "${PREFIX}${OWN}" <<<"$outside" || true)"
if [[ -n "$crossing" ]]; then
	printf 'another module of this repository imports cmd/oomreporter:\n%s\n' "$crossing" >&2
	rc=1
fi
((rc == 0)) && echo "check-oomreporter-imports: ok ($(wc -l <<<"$inside" | tr -d ' ') imports inside, $(wc -l <<<"$outside" | tr -d ' ') outside)"
exit "$rc"
