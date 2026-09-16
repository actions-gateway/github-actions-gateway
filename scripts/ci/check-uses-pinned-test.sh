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
#      picks up an untracked new workflow and scans it rather than only listing
#      it, and excludes vendored action.yml.
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
# Group 4 needs an untracked workflow in the real tree to prove the selection
# sees one. Two things about it are load-bearing, both of them Q1106.
#
# It lives under cmd/gmc/ rather than the root .github/workflows/. That root
# directory is read concurrently by actionlint, check-gate-needs.sh and
# check-path-filters.sh in the same `make check` fan-out. With the probe
# churning there, 2 of 20 actionlint runs went red: one on a path its own walk
# had just listed and was then unable to stat, one on the probe read
# half-written (`"jobs" section is missing`). actionlint walks the directory
# inside a third-party binary, so neither is reachable by tolerance in our own
# readers. cmd/gmc/.github/workflows/ is in this gate's selection and in no
# other reader's, so the probe still proves what it has to from the real tree.
#
# It is pinned, not tagged. A tag-pinned probe is a true finding to a
# concurrent `make uses-pinned-check`, which failed on it in 12 of 20 runs. So
# what group 4 takes from the probe is that default selection reaches an
# untracked workflow AND feeds it to the scan; that a bad pin in the scanned
# set exits 1 is group 1's subject, over an explicit file set.
#
# It is removed on every exit path, including a failed assertion, so a test run
# cannot leave a stray workflow behind.
PROBE="$REPO_ROOT/cmd/gmc/.github/workflows/zz-uses-pinned-test-probe.yml"
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

# scanned_count — the file count the last run reported, which is what tells a
# file the selection merely listed from one the scan actually read.
scanned_count() {
	awk '/workflow\/action file\(s\)/ {
		for (i = 1; i < NF; i++) if ($i == "across") { print $(i + 1); exit }
	}' <<<"$GATE_OUT"
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
tracked_count="$(scanned_count)"

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

printf 'name: p\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@%s # v7.0.1\n' \
	"$PINNED" >"$PROBE"
expect_selected "selection includes an untracked workflow" yes \
	'cmd/gmc/.github/workflows/zz-uses-pinned-test-probe.yml' "$(selected)"
# Being listed is not being read. The default run's own file count is what says
# the probe reached the scan, and it is the assertion a pinned probe can still
# carry — see the note on PROBE.
expect_status "the default run stays green with the probe present" 0
probe_count="$(scanned_count)"
if [[ -n "$tracked_count" && -n "$probe_count" ]] && ((probe_count == tracked_count + 1)); then
	ok "  and the untracked workflow reaches the scan" "$tracked_count -> $probe_count file(s)"
else
	bad "  and the untracked workflow reaches the scan" \
		"want $((${tracked_count:-0} + 1)), got ${probe_count:-none}"
fi
rm -f "$PROBE"

if ((fails > 0)); then
	printf '\n%d check-uses-pinned assertion(s) failed.\n' "$fails" >&2
	exit 1
fi
printf '\nall check-uses-pinned assertions passed\n'
