#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-uses-pinned.sh (Q644).
#
# Every `uses:` in the tree is already a SHA, so the gate's real verdict is green
# and stays green — which makes it unfalsifiable unless something drives it red
# on purpose. Every case here therefore breaks a fixture and asserts it is
# caught, the standing form of the invert-the-fix verification
# (docs/development/testing.md § Diagnosing failures). Fixtures rather than the
# tracked workflows, because the tracked ones are (and must stay) correct.
#
# The reference classification itself is exhaustively covered by the Go tests in
# devtools/ci/usespin; what is asserted here is everything the script adds around
# them, which is where this gate could go quietly narrow:
#
#   1. Exit codes end to end — 1 on a finding, 0 on a clean set. A gate that
#      cannot report a finding through its own entry point protects nothing.
#   2. Fail closed on a file that cannot be parsed or read: exit 2, never the 0
#      that would let an unparseable workflow through.
#   3. The empty-extraction tripwire. A file set yielding no `uses:` at all is an
#      error, not a pass — that is what tells "the tree is clean" apart from "the
#      walk stopped matching", the failure this repo has shipped before (Q571).
#   4. Default file selection covers the whole tree, including the three
#      cmd/gmc/.github/workflows/ scaffolding files that actionlint never sees,
#      picks up an untracked new workflow, and excludes vendored action.yml.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

GATE="$REPO_ROOT/scripts/ci/check-uses-pinned.sh"
FIXTURE_ROOT="$REPO_ROOT/tmp/check-uses-pinned-test.$$"
# Group 4 needs an untracked workflow in the real .github/workflows/ to prove the
# selection sees one; it is removed on every exit path, including a failed
# assertion, so a test run cannot leave a tag-pinned file behind for the gate
# itself to trip over.
PROBE="$REPO_ROOT/.github/workflows/zz-uses-pinned-test-probe.yml"
mkdir -p "$FIXTURE_ROOT"
trap 'rm -rf "$FIXTURE_ROOT" "$PROBE"' EXIT INT TERM

PINNED="3d3c42e5aac5ba805825da76410c181273ba90b1"

fails=0
ok() { printf 'ok   %-46s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-46s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# fixture NAME BODY — write a workflow fixture and echo its path.
fixture() {
	local path="$FIXTURE_ROOT/$1.yml"
	printf '%s' "$2" >"$path"
	printf '%s\n' "$path"
}

# run_gate FILE... — run the gate, capturing status and output separately. The
# status is never read through a pipe: `grep` exits 1 on no match, which would
# report failure exactly when the tree is clean.
GATE_OUT=""
GATE_STATUS=0
run_gate() {
	local log="$FIXTURE_ROOT/run.log"
	set +e
	"$GATE" "$@" >"$log" 2>&1
	GATE_STATUS=$?
	set -e
	GATE_OUT="$(cat "$log")"
}

# expect_status NAME WANT FILE... — the gate exits WANT over these files.
expect_status() {
	local name="$1" want="$2"
	shift 2
	run_gate "$@"
	die_if_killed "$name" "$GATE_STATUS" "$want"
	if ((GATE_STATUS == want)); then
		ok "$name" "exit $GATE_STATUS"
	else
		bad "$name" "want exit $want, got $GATE_STATUS: $(head -3 <<<"$GATE_OUT")"
	fi
}

# expect_output NAME NEEDLE — the last run's output mentions NEEDLE.
expect_output() {
	local name="$1" needle="$2"
	if [[ "$GATE_OUT" == *"$needle"* ]]; then
		ok "$name" "names '$needle'"
	else
		bad "$name" "output does not mention '$needle'"
	fi
}

step_body() {
	printf 'name: t\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n%s' "$1"
}

# 1. Exit codes end to end.
tagged="$(fixture tagged "$(step_body "      - uses: actions/checkout@v4\n")")"
expect_status "tag ref fails the gate" 1 "$tagged"
expect_output "  and names the mutable ref" 'is a tag or branch'

clean="$(fixture clean "$(step_body "\
      - uses: actions/checkout@$PINNED # v7.0.1\n\
      - uses: anchore/sbom-action/download-syft@$PINNED # v0.24.0\n\
      - uses: ./.github/actions/setup\n\
      - uses: docker://alpine@sha256:$(printf 'a%.0s' {1..64})\n")")"
expect_status "pinned SHA, local action and digest pass" 0 "$clean"

# 2. Fail closed — an unparseable or unreadable file is never a pass.
broken="$(fixture broken 'jobs:
  j:
   - [unbalanced
')"
expect_status "unparseable workflow fails closed" 2 "$broken"
expect_status "unreadable file fails closed" 2 "$FIXTURE_ROOT/does-not-exist.yml"

# 3. The empty-extraction tripwire.
noUses="$(fixture no-uses "$(step_body "      - run: make check\n")")"
expect_status "a file set with no uses: is an error" 2 "$noUses"
expect_output "  and says the walk may have stopped" 'stopped matching'

# 4. Default selection covers the whole tree.
expect_status "the tracked tree passes" 0
expect_output "  covering cmd/gmc scaffolding too" "file(s)"

# The selection the gate makes when given no arguments. Captured whole rather
# than piped into grep: `grep -q` exits on its first match, and the SIGPIPE that
# sends upstream turns a successful match into a non-zero pipeline under
# `set -o pipefail` — a match that reads as a miss.
selected() {
	git_candidates \
		':(glob)**/.github/workflows/*.yml' ':(glob)**/.github/workflows/*.yaml' \
		':(glob)**/action.yml' ':(glob)**/action.yaml' ':(exclude)*vendor/*' | select_present_files
}

# expect_selected NAME WANT PATH — PATH is (or is not) in the captured selection.
expect_selected() {
	local name="$1" want="$2" path="$3" sel="$4" got=no
	[[ $'\n'"$sel"$'\n' == *$'\n'"$path"$'\n'* ]] && got=yes
	if [[ "$got" == "$want" ]]; then
		ok "$name" "$path: $got"
	else
		bad "$name" "$path selected=$got, want $want"
	fi
}

sel="$(selected)"
expect_selected "selection includes cmd/gmc workflows" yes \
	'cmd/gmc/.github/workflows/lint.yml' "$sel"
expect_selected "selection excludes vendored action.yml" no \
	'tools/vendor/github.com/securego/gosec/v2/action.yml' "$sel"

printf 'name: p\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n' >"$PROBE"
expect_selected "selection includes an untracked workflow" yes \
	'.github/workflows/zz-uses-pinned-test-probe.yml' "$(selected)"
# The probe is a tag-pinned workflow: while it exists the default-selection gate
# must see it and fail, which is this assertion's real subject.
expect_status "an untracked tag pin fails the default run" 1
rm -f "$PROBE"

if ((fails > 0)); then
	printf '\n%d check-uses-pinned assertion(s) failed.\n' "$fails" >&2
	exit 1
fi
printf '\nall check-uses-pinned assertions passed\n'
