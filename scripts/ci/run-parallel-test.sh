#!/usr/bin/env bash
#
# Unit tests for scripts/ci/run-parallel.sh — the fan-out behind `make check`,
# `make status-gates` and `make scripts-test`.
#
# The load-bearing claim is that a failure is attributable, and attributable to
# the right thing: output stays labeled, every pid is waited so a red suite is
# never collateral from a sibling, and the summary carries each failure's exit
# status. The status is what tells an assertion failure (small rc) apart from a
# command the kernel killed (128+n; 137 is the OOM killer's) or one that was
# never found (127). Five flakes in this family went undiagnosed while the
# summary named only a label (Q703).
#
# The status also decides the verdict: a signal death is reported under KILLED
# and exits with its own 128+n status, because it reached no verdict and is not
# a defect to go and read (Q837). The cases below pin both halves, since
# silencing a kill and calling it a failure are both wrong.
#
# Wall time is the second claim (Q819): a fan-out's total is its slowest member,
# so every run reports per-label seconds slowest-first. The timing sits inside
# the same subshell whose exit status `wait` collects, so the cases below pin
# that it costs no verdict — a failure, a kill and a missing command each keep
# their status while gaining a duration.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
RP="$REPO_ROOT/scripts/ci/run-parallel.sh"

fails=0
out=""
rc=0

# rp SPEC... — run the fan-out, capturing merged output and exit code.
rp() {
	rc=0
	out="$("$RP" "$@" 2>&1)" || rc=$?
}

# want NAME PATTERN — assert the captured output matched an extended regexp.
want() {
	local name="$1" pattern="$2"
	if grep -Eq -- "$pattern" <<<"$out"; then
		printf 'ok   %-42s\n' "$name"
	else
		printf 'FAIL %-42s no match for /%s/\n%s\n' "$name" "$pattern" "$out" >&2
		fails=$((fails + 1))
	fi
}

# want_no NAME PATTERN — assert the captured output did NOT match.
want_no() {
	local name="$1" pattern="$2"
	if grep -Eq -- "$pattern" <<<"$out"; then
		printf 'FAIL %-42s unexpected match for /%s/\n%s\n' "$name" "$pattern" "$out" >&2
		fails=$((fails + 1))
	else
		printf 'ok   %-42s\n' "$name"
	fi
}

# want_rc NAME WANT — assert the fan-out's own exit code.
want_rc() {
	local name="$1" want="$2"
	if [[ "$rc" == "$want" ]]; then
		printf 'ok   %-42s rc=%s\n' "$name" "$rc"
	else
		printf 'FAIL %-42s want rc=%s got rc=%s\n%s\n' "$name" "$want" "$rc" "$out" >&2
		fails=$((fails + 1))
	fi
}

# An all-passing run is silent about failure and exits 0.
rp "a:true" "b:true"
want_rc 'all commands pass' 0
want_no 'a passing run names no failure' 'FAILED'

# Output stays attributable to its command.
rp "alpha:echo hello" "beta:echo world"
want 'output is labeled per command' '^\[alpha\] hello$'
want 'a second command keeps its own label' '^\[beta\] world$'

# stderr is captured and labeled too, not lost.
rp "noisy:sh -c 'echo oops >&2'"
want 'stderr is labeled, not dropped' '^\[noisy\] oops$'

# A failure names the label AND the exit status (Q703). Without the status,
# an assertion failure and a killed command read identically.
rp "ok-one:true" "boom:sh -c 'exit 3'"
want_rc 'one failure fails the fan-out' 1
want 'a failure names its label' 'FAILED:.*boom'
want 'a failure reports its exit status' 'boom \(exit 3\)'
want_no 'a passing sibling is not reported' 'ok-one \(exit'

# A command the kernel kills is reported as a signal, not as an assertion
# failure. 137 (SIGKILL) is what an OOM kill looks like in a labeled log.
rp "killed:sh -c 'kill -9 \$\$'"
want_rc 'a kill exits with the killed status' 137
want 'a signal death is named as a signal' 'KILLED:.*killed \(signal 9, exit 137\)'
want_no 'a kill is not counted as a failure' 'FAILED'
want 'the summary says a kill is not a gate failure' 'not a gate failure'

# Q837's measurement: SIGTERM under host contention. An external kill must not
# read as a real gate failure, and must not vanish from the output either.
rp "term:sh -c 'kill -15 \$\$'"
want_rc 'a SIGTERM kill exits 143, not 1' 143
want 'a SIGTERM kill is named as signal 15' 'KILLED:.*term \(signal 15, exit 143\)'
want_no 'a SIGTERM kill is not counted as a failure' 'FAILED'

# A verdict outranks a kill: something reached a bad verdict, so the fan-out
# exits 1 — and the kill is still reported beside it.
rp "boom:sh -c 'exit 3'" "killed:sh -c 'kill -9 \$\$'"
want_rc 'a real failure beside a kill exits 1' 1
want 'the failure is reported' 'FAILED:.*boom \(exit 3\)'
want 'the kill is reported beside it' 'KILLED:.*killed \(signal 9, exit 137\)'

# Two kills exit with the first status, and the second is still reported. This
# also pins that the first-kill bookkeeping survives `set -e`.
rp "first:sh -c 'kill -15 \$\$'" "second:sh -c 'kill -9 \$\$'"
want_rc 'two kills exit with the first status' 143
want 'a second kill is reported too' 'KILLED:.*second \(signal 9, exit 137\)'

# 128 is not a signal death: git spends it on any fatal error, which is what
# Q820's temp-file signature looks like. It stays a primary failure, so the
# split is rc > 128 rather than the row's rc >= 128.
rp "gitfatal:sh -c 'exit 128'"
want_rc 'exit 128 still fails the fan-out' 1
want 'exit 128 is a failure, not a kill' 'FAILED:.*gitfatal \(exit 128\)'
want_no 'exit 128 is not reported as a kill' 'KILLED'

# 127 is a missing command, not a failing one — the distinction a bare label
# also erased.
rp "absent:this-command-does-not-exist-q703"
want 'a missing command reports 127' 'absent \(exit 127\)'

# Every pid is waited, so a red suite is never collateral from a sibling — the
# claim the Q703 row had to make by reading the source. The later command
# outlives the earlier failure, so its status is reported only if the wait loop
# keeps going past one. Asserting a survivor's side effect instead would prove
# nothing: the EXIT trap kills the wrapper subshells, not the commands under
# them, so an abandoned sibling still finishes and still writes its output.
rp "early:sh -c 'exit 2'" "late:sh -c 'sleep 0.5; exit 4'"
want_rc 'a sibling failure fails the fan-out' 1
want 'the first failure is named' 'early \(exit 2\)'
want 'a failure after it is still waited for' 'late \(exit 4\)'

# A command may contain colons; only the first splits label from command.
rp "colon:sh -c 'echo a:b:c'"
want 'only the first colon splits the spec' '^\[colon\] a:b:c$'

# No arguments is a usage error, not a silent success.
rp
want_rc 'no arguments exits non-zero' 1
want 'no arguments prints usage' 'usage:'

# want_order NAME FIRST SECOND — assert FIRST's line precedes SECOND's. grep is
# line-oriented, so an ordering claim cannot be written as one pattern.
want_order() {
	local name="$1" first="$2" second="$3" a b
	# `|| true` on each: a pattern that matches nothing is this assertion's own
	# failure to report, not a reason for `set -e` to abandon the suite.
	a="$(grep -En -- "$first" <<<"$out" | head -1 | cut -d: -f1 || true)"
	b="$(grep -En -- "$second" <<<"$out" | head -1 | cut -d: -f1 || true)"
	if [[ -n "$a" && -n "$b" ]] && (( a < b )); then
		printf 'ok   %-42s\n' "$name"
	else
		printf 'FAIL %-42s /%s/ at %s should precede /%s/ at %s\n%s\n' \
			"$name" "$first" "${a:-none}" "$second" "${b:-none}" "$out" >&2
		fails=$((fails + 1))
	fi
}

# Q819: a slow gate hid behind a label for three CI-speed rounds because the
# fan-out never timed anything. Every run now reports each label's wall time.
# The seconds are asserted as ranges, not exact values: the duration truncates
# both endpoints, so a command spanning a second boundary reports one more.
# Every bound is a floor and none a ceiling: the timed span wraps four forks and
# a pipeline teardown, so an upper bound asserts host load rather than the
# runner (Q970). Hence `slow`'s alternation over a plain `[3-9][0-9]*`, which
# would reject the two-digit duration a contended host reports.
rp "quick:true" "mid:sleep 1" "slow:sleep 3"
want_rc 'a timed run still exits 0' 0
want 'the run reports wall time' 'wall time, slowest first'
want 'a slow command reports its seconds' '^\[run-parallel\] +([3-9]|[1-9][0-9]+)s +slow$'
want 'a fast command is reported too' '^\[run-parallel\] +[0-9]+s +quick$'
want 'the run reports its own elapsed time' 'elapsed [0-9]+s across 3 command\(s\)'

# Slowest first, so the member setting the fan-out's total is the first line
# read. Ordering is by duration, not spawn order: the three are passed fastest
# first and come back reversed. The claim runs between the two sleepers —
# ordering `quick` would rest on fork overhead staying under a sibling's — and
# their two-second gap is the margin a flip has to cross (Q970).
want_order 'the slowest command is listed first' '[0-9]+s +slow$' '[0-9]+s +mid$'

# The trap this change had to avoid: the timing sits inside the same subshell
# whose status `wait` collects, so a bookkeeping step that swallowed the status
# would turn every gate green. A failure stays red, named, and exact.
rp "timed-ok:true" "timed-boom:sh -c 'sleep 1; exit 3'"
want_rc 'timing does not swallow a failure' 1
want 'a timed failure keeps its exit status' 'FAILED:.*timed-boom \(exit 3\)'
want 'a timed failure still reports its seconds' '^\[run-parallel\] +[1-9][0-9]*s +timed-boom$'
want_order 'the failure summary follows the timing' 'wall time, slowest first' 'FAILED:'

# A kill keeps its 128+n status too, and still reports the time it spent before
# the signal arrived.
rp "timed-kill:sh -c 'sleep 1; kill -9 \$\$'"
want_rc 'timing does not swallow a kill' 137
want 'a killed command is still named a kill' 'KILLED:.*timed-kill \(signal 9, exit 137\)'
want 'a killed command still reports its seconds' '^\[run-parallel\] +[1-9][0-9]*s +timed-kill$'

# A missing command has no wall time worth reading, but dropping it from the
# block would make the timing disagree with the failure summary about what ran.
rp "absent-timed:this-command-does-not-exist-q819"
want 'a missing command still appears in the timing' '^\[run-parallel\] +[0-9]+s +absent-timed$'

# RUN_PARALLEL_GIT_TRACE_DIR gives each child its own GIT_TRACE, so a fan-out
# doubles as the measurement behind check-fixture-maintenance.sh (Q921). Both
# directions: unset must leave GIT_TRACE alone, because the trace is opt-in and a
# child that inherits one silently is a behaviour change to every gate. The file
# is named for the label, which is what makes a finding attributable to a suite.
TRACE_DIR="$REPO_ROOT/tmp/run-parallel-test-traces.$$"
rm -rf "$TRACE_DIR"
mkdir -p "$TRACE_DIR"
trap 'rm -rf "$TRACE_DIR"' EXIT

rc=0
out="$(RUN_PARALLEL_GIT_TRACE_DIR="$TRACE_DIR" "$RP" \
	"alpha:git rev-parse --show-toplevel" "beta:git rev-parse --show-toplevel" 2>&1)" || rc=$?
want_rc 'a traced fan-out still reports its own verdict' 0
for label in alpha beta; do
	if [[ -s "$TRACE_DIR/$label.trace" ]]; then
		printf 'ok   %-42s\n' "GIT_TRACE reaches the $label child"
	else
		printf 'FAIL %-42s no trace at %s\n' "GIT_TRACE reaches the $label child" \
			"$TRACE_DIR/$label.trace" >&2
		fails=$((fails + 1))
	fi
done
if grep -q 'rev-parse' "$TRACE_DIR/alpha.trace" 2>/dev/null; then
	printf 'ok   %-42s\n' 'the trace records the child'"'"'s own git'
else
	printf 'FAIL %-42s alpha.trace does not record the git it ran\n' \
		'the trace records the child'"'"'s own git' >&2
	fails=$((fails + 1))
fi

# The dir is not inherited past one level. A nested fan-out that re-pointed
# GIT_TRACE at its own labels would file a suite's git under a name that is not a
# suite and never write the suite's own file at all, so the outer trace must
# survive the inner run (measured before the fix: the git landed in inner.trace
# and suite-x.trace was never created).
rm -rf "$TRACE_DIR"
mkdir -p "$TRACE_DIR"
rc=0
out="$(RUN_PARALLEL_GIT_TRACE_DIR="$TRACE_DIR" "$RP" \
	"outer:$RP 'inner:git rev-parse --show-toplevel'" 2>&1)" || rc=$?
want_rc 'a nested fan-out still reports its verdict' 0
if [[ -s "$TRACE_DIR/outer.trace" ]] && grep -q 'rev-parse' "$TRACE_DIR/outer.trace"; then
	printf 'ok   %-42s\n' "a nested run's git stays in the outer trace"
else
	printf 'FAIL %-42s outer.trace missing or empty of the nested git\n' \
		"a nested run's git stays in the outer trace" >&2
	fails=$((fails + 1))
fi
if [[ -e "$TRACE_DIR/inner.trace" ]]; then
	printf 'FAIL %-42s the inner label claimed a trace of its own\n' \
		'a nested fan-out writes no trace of its own' >&2
	fails=$((fails + 1))
else
	printf 'ok   %-42s\n' 'a nested fan-out writes no trace of its own'
fi

# Unset: the runner writes nothing and changes nothing. GIT_TRACE is asserted
# unchanged rather than empty, because this suite runs as a traced child of
# `make scripts-test` and inherits one there — asserting empty would pass
# standalone and fail under the gate, which is how this case was found.
rm -rf "$TRACE_DIR"
mkdir -p "$TRACE_DIR"
rp "gamma:printf 'GIT_TRACE=[%s]\\n' \"\${GIT_TRACE:-}\""
want 'an untraced child inherits GIT_TRACE unchanged' \
	"^\\[gamma\\] GIT_TRACE=\\[${GIT_TRACE:-}\\]$"
if [[ -z "$(ls -A "$TRACE_DIR")" ]]; then
	printf 'ok   %-42s\n' 'no trace dir set writes no trace'
else
	printf 'FAIL %-42s wrote %s\n' 'no trace dir set writes no trace' \
		"$(ls -A "$TRACE_DIR")" >&2
	fails=$((fails + 1))
fi

if (( fails > 0 )); then
	echo "run-parallel-test: $fails failure(s)" >&2
	exit 1
fi
echo "run-parallel-test: all assertions passed"
