#!/usr/bin/env bash
#
# Unit tests for scripts/ci/dependabot-rebase-stale.sh (Q427, Q1118). Covers the
# pure half of the script: bump extraction from a pair of go.mod files, which
# decides *which* version each module is replayed at and is therefore the only
# place a wrong answer could downgrade a dependency; candidate selection from
# recorded `gh pr list` output; and the eligibility decision, which reads a
# mergeable state, a behind-count and a checks verdict and says whether the PR
# is stranded. The rest (the mergeable poll, the compare and rollup reads, `go
# get`, the force-push) needs a live PR.
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

# --- checks classification -------------------------------------------------
#
# checks_verdict turns a `statusCheckRollup` array into one of four words, and
# the arm that decides a rescue reads only FAILING. The conclusions below are
# taken from the GraphQL enum rather than from the ones this repo happens to
# emit, so a conclusion nobody here has seen yet lands in a named bucket instead
# of whichever branch it falls through to.

# expect_checks NAME JSON WANT - assert --checks prints WANT for JSON on stdin.
expect_checks() {
	local name="$1" json="$2" want="$3" got
	got="$(printf '%s' "$json" | "$SCRIPT" --checks)"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-28s %s\n' "$name" "$got"
	else
		printf 'FAIL %-28s want [%s] got [%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

expect_checks checks-empty '[]' NONE
expect_checks checks-null 'null' NONE
expect_checks checks-all-green '[
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}
]' PASSING
expect_checks checks-one-red '[
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}
]' FAILING
expect_checks checks-timed-out '[
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"TIMED_OUT"}
]' FAILING
expect_checks checks-in-progress '[
	{"__typename":"CheckRun","status":"IN_PROGRESS"},
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}
]' PENDING

# A path-gated job that correctly skipped is not a failure, and a rollup of
# nothing but skips is not one either.
expect_checks checks-skipped-is-not-red '[
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SKIPPED"},
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}
]' PASSING

# THE LOOP GUARD. A GITHUB_TOKEN force-push - this script's own - leaves every
# run withheld at ACTION_REQUIRED until a maintainer clicks Approve and run.
# Classified FAILING, a PR this script just rescued would qualify for another
# rescue the moment the base moved, force-pushing and commenting on every base
# push forever. A withheld run has not run, so it is PENDING.
expect_checks checks-withheld-is-pending '[
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"ACTION_REQUIRED"}
]' PENDING

# A legacy commit status, which carries `state` and no `conclusion` at all.
expect_checks checks-statuscontext-error '[
	{"__typename":"StatusContext","state":"ERROR"}
]' FAILING
expect_checks checks-statuscontext-ok '[
	{"__typename":"StatusContext","state":"SUCCESS"}
]' PASSING

# The control from OUTSIDE the failing set: a terminal conclusion the classifier
# does not name must not fall through into FAILING. Without this, the failing
# list could be anything and every case above would still pass.
expect_checks checks-unknown-conclusion '[
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"NOT_A_REAL_CONCLUSION"}
]' PASSING
expect_checks checks-cancelled-is-not-red '[
	{"__typename":"CheckRun","status":"COMPLETED","conclusion":"CANCELLED"}
]' PASSING

# --- rescue verdict --------------------------------------------------------
#
# The whole eligibility decision, as a pure function of three readings (Q1118).
# Two arms rescue and everything else is left alone; both arms and every skip
# branch are asserted here, because a rescue that fires on a PR nobody can help
# force-pushes and comments for no reason.

# expect_verdict NAME STATE BEHIND CHECKS WANT - assert --verdict's first word.
expect_verdict() {
	local name="$1" state="$2" behind="$3" checks="$4" want="$5" got line
	line="$("$SCRIPT" --verdict "$state" "$behind" "$checks")"
	got="${line%% *}"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-28s %s\n' "$name" "$line"
	else
		printf 'FAIL %-28s want [%s] got [%s]\n' "$name" "$want" "$line" >&2
		fails=$((fails + 1))
	fi
}

# Arm 1, Q427's original case: a branch that cannot merge at all, whatever its
# checks say. Nothing about this arm changed when the second one was added.
expect_verdict verdict-conflicting CONFLICTING 0 PASSING rescue
expect_verdict verdict-conflicting-red CONFLICTING 7 FAILING rescue

# Arm 2, Q1118: mergeable, behind the base branch, and red. #1878 and #1880 were
# both MERGEABLE and failing release-pins because v1.8.0 was tagged after their
# base - a gate whose verdict is a function of wall-clock time rather than of
# the branch. Nothing heals that but a rebase, and this is the arm that does it.
expect_verdict verdict-behind-and-red MERGEABLE 12 FAILING rescue

# Behind ALONE is not enough: the merge queue rebases a green PR itself, so
# force-pushing it would be noise. This is the assertion that separates the new
# arm from "rescue anything that is not green".
expect_verdict verdict-behind-but-green MERGEABLE 12 PASSING skip
expect_verdict verdict-behind-but-pending MERGEABLE 12 PENDING skip
expect_verdict verdict-behind-no-checks MERGEABLE 12 NONE skip

# Red ALONE is not enough either: a bump that genuinely breaks the build is red
# on its own tree, and replaying it onto current main reproduces the same red.
expect_verdict verdict-level-and-red MERGEABLE 0 FAILING skip
expect_verdict verdict-level-and-green MERGEABLE 0 PASSING skip

# An unsettled mergeable state is left to the next run rather than guessed at.
expect_verdict verdict-unknown-state UNKNOWN 12 FAILING skip
expect_verdict verdict-unknown-state-red UNKNOWN 0 FAILING skip

# A non-numeric behind count is what behind_by prints when the compare API could
# not be read. It must skip, not rescue on a guess.
expect_verdict verdict-unreadable-behind MERGEABLE '' FAILING skip

# --verdict with a missing operand fails rather than deciding on two readings.
if "$SCRIPT" --verdict MERGEABLE 3 >/dev/null 2>&1; then
	printf 'FAIL %-28s expected failure on a missing operand\n' verdict-arity >&2
	fails=$((fails + 1))
else
	printf 'ok   %-28s missing operand rejected\n' verdict-arity
fi

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
