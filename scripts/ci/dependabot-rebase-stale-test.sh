#!/usr/bin/env bash
#
# Unit tests for scripts/ci/dependabot-rebase-stale.sh (Q427). Covers the pure
# half of the script: bump extraction from a pair of go.mod files, which decides
# *which* version each module is replayed at and is therefore the only place a
# wrong answer could downgrade a dependency, plus candidate selection from
# recorded `gh pr list` output. The rest (mergeable polling, `go get`, the
# force-push) needs a live PR.
#
# Selection was untested until it broke: the author filter matched one of the
# two spellings gh uses and the run exited 0 having selected nothing, so neither
# the workflow nor its dry run could go red. A fixture cannot notice gh changing
# that spelling again - only `--list` against the real repo can - but it does
# keep both known spellings matched.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/ci/dependabot-rebase-stale.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/dependabot-rebase-stale-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# gomod NAME CONTENT - write a go.mod fixture and print its path.
gomod() {
	local name="$1" content="$2" dir
	dir="$FIXTURE_DIR/$name"
	mkdir -p "$dir"
	printf '%s\n' "$content" >"$dir/go.mod"
	printf '%s\n' "$dir/go.mod"
}

# expect_bumps NAME BASE_FILE TIP_FILE WANT - assert --bumps prints WANT
# (newline-separated, order-insensitive) for the given pair.
expect_bumps() {
	local name="$1" base="$2" tip="$3" want="$4" got
	got="$("$SCRIPT" --bumps "$base" "$tip" | LC_ALL=C sort)"
	want="$(printf '%s' "$want" | LC_ALL=C sort)"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-28s %s\n' "$name" "$(tr '\n' ';' <<<"$got")"
	else
		printf 'FAIL %-28s want [%s] got [%s]\n' "$name" \
			"$(tr '\n' ';' <<<"$want")" "$(tr '\n' ';' <<<"$got")" >&2
		fails=$((fails + 1))
	fi
}

BASE="$(gomod base 'module example.com/m

go 1.25

require (
	golang.org/x/text v0.38.0
	k8s.io/api v0.36.2
)

require github.com/spf13/pflag v1.0.5 // indirect')"

# A single-module bump is picked up, and untouched requires are not.
SINGLE="$(gomod single 'module example.com/m

go 1.25

require (
	golang.org/x/text v0.39.0
	k8s.io/api v0.36.2
)

require github.com/spf13/pflag v1.0.5 // indirect')"
expect_bumps single-bump "$BASE" "$SINGLE" 'golang.org/x/text v0.39.0'

# A grouped bump yields one line per module - the case the branch name cannot be
# parsed for, since it carries only the group's hash.
GROUPED="$(gomod grouped 'module example.com/m

go 1.25

require (
	golang.org/x/text v0.39.0
	k8s.io/api v0.36.3
)

require github.com/spf13/pflag v1.0.6 // indirect')"
expect_bumps grouped-bump "$BASE" "$GROUPED" 'golang.org/x/text v0.39.0
k8s.io/api v0.36.3
github.com/spf13/pflag v1.0.6'

# Indirect requires are bumps like any other - Dependabot bumps them too.
INDIRECT="$(gomod indirect 'module example.com/m

go 1.25

require (
	golang.org/x/text v0.38.0
	k8s.io/api v0.36.2
)

require github.com/spf13/pflag v1.0.6 // indirect')"
expect_bumps indirect-bump "$BASE" "$INDIRECT" 'github.com/spf13/pflag v1.0.6'

# The require form must not matter: the same versions written as separate
# single-line requires produce no bumps at all against the block form.
REFORMATTED="$(gomod reformatted 'module example.com/m

go 1.25

require golang.org/x/text v0.38.0

require k8s.io/api v0.36.2

require github.com/spf13/pflag v1.0.5 // indirect')"
expect_bumps reformat-is-not-a-bump "$BASE" "$REFORMATTED" ''

# An identical file yields nothing.
expect_bumps no-change "$BASE" "$BASE" ''

# Additions and removals are tidy bookkeeping, not bumps: vendor-sync redoes
# them, and replaying them would fight tidy over requires main has dropped.
ADDED="$(gomod added 'module example.com/m

go 1.25

require (
	golang.org/x/text v0.38.0
	k8s.io/api v0.36.2
	sigs.k8s.io/yaml v1.6.0
)

require github.com/spf13/pflag v1.0.5 // indirect')"
expect_bumps addition-is-not-a-bump "$BASE" "$ADDED" ''

REMOVED="$(gomod removed 'module example.com/m

go 1.25

require golang.org/x/text v0.38.0

require github.com/spf13/pflag v1.0.5 // indirect')"
expect_bumps removal-is-not-a-bump "$BASE" "$REMOVED" ''

# A downgrade in the diff is still reported as a bump here - refusing it is
# apply_bump's job, using Go's own "downgraded" signal, so that transitive
# downgrades are caught too. Extraction must not silently swallow it.
DOWNGRADE="$(gomod downgrade 'module example.com/m

go 1.25

require (
	golang.org/x/text v0.37.0
	k8s.io/api v0.36.2
)

require github.com/spf13/pflag v1.0.5 // indirect')"
expect_bumps downgrade-still-extracted "$BASE" "$DOWNGRADE" 'golang.org/x/text v0.37.0'

# A go.mod with no requires at all must not error out under `set -euo pipefail`.
EMPTY_A="$(gomod empty-a 'module example.com/m

go 1.25')"
EMPTY_B="$(gomod empty-b 'module example.com/m

go 1.26')"
expect_bumps no-requires "$EMPTY_A" "$EMPTY_B" ''

# --- candidate selection ---------------------------------------------------
#
# The selection filter reads `gh pr list --json number,headRefName,author`. gh
# spells a GitHub App author `app/dependabot` there while the REST user object
# says `dependabot[bot]`, and matching only the latter is what left this
# workflow reporting "No open Dependabot Go-module PRs" on every run while
# exiting 0. Both spellings are fixtures below so a filter narrowed back to one
# of them fails here instead of silently selecting nothing.

# expect_select NAME JSON WANT - assert --select prints WANT for JSON on stdin.
expect_select() {
	local name="$1" json="$2" want="$3" got
	got="$(printf '%s' "$json" | "$SCRIPT" --select | LC_ALL=C sort | tr '\n' ';')"
	want="$(printf '%s' "$want" | tr ' ' '\n' | LC_ALL=C sort | tr '\n' ';')"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-28s %s\n' "$name" "$got"
	else
		printf 'FAIL %-28s want [%s] got [%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

expect_select select-app-slug '[
	{"number":1726,"headRefName":"dependabot/go_modules/cmd/gmc/go-deps-38d","author":{"login":"app/dependabot"}}
]' '1726'

expect_select select-bracket-login '[
	{"number":1726,"headRefName":"dependabot/go_modules/cmd/gmc/go-deps-38d","author":{"login":"dependabot[bot]"}}
]' '1726'

# A human's PR on a lookalike branch, and Dependabot's own non-gomod ecosystems,
# are both out of scope: the replay only knows how to redo go.mod bumps.
expect_select select-skips-human '[
	{"number":10,"headRefName":"dependabot/go_modules/api/x","author":{"login":"karlkfi"}},
	{"number":11,"headRefName":"claude/dependabot/go_modules/api/x","author":{"login":"app/dependabot"}}
]' ''

expect_select select-skips-other-ecosystems '[
	{"number":12,"headRefName":"dependabot/github_actions/actions-a8b","author":{"login":"app/dependabot"}},
	{"number":13,"headRefName":"dependabot/docker/base-image-c3f","author":{"login":"dependabot[bot]"}}
]' ''

expect_select select-empty-list '[]' ''

# --bumps with a missing operand fails rather than silently comparing nothing.
if "$SCRIPT" --bumps "$BASE" >/dev/null 2>&1; then
	printf 'FAIL %-28s expected failure on a missing operand\n' bumps-arity >&2
	fails=$((fails + 1))
else
	printf 'ok   %-28s missing operand rejected\n' bumps-arity
fi

# An unknown flag fails rather than being read as a PR number.
if "$SCRIPT" --nope >/dev/null 2>&1; then
	printf 'FAIL %-28s expected failure on an unknown flag\n' unknown-flag >&2
	fails=$((fails + 1))
else
	printf 'ok   %-28s unknown flag rejected\n' unknown-flag
fi

if ((fails > 0)); then
	echo "$fails check(s) failed" >&2
	exit 1
fi
echo "all dependabot-rebase-stale checks passed"
