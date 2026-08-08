#!/usr/bin/env bash
#
# Assertions for the RUN= spec filter on the test make targets (Q679, Q680).
# One contract spans them: RUN narrows the run, and a RUN that selects nothing
# FAILS. `go test -run` and `ginkgo --focus` both exit 0 when their filter
# matches no test, so a mistyped name otherwise reports a green suite — the
# worst shape of failure, because it reads exactly like the filter worked.
#
# The unit half runs scripts/go/go-test.sh for real against a PATH shim that
# records each `go test` argv and replays canned output, forwarding every other
# subcommand to the real toolchain: the guard's input is what `go test -v`
# printed, so a shim that produces it exercises the whole decision without a
# workspace-wide compile. The shim answers per working directory, which is what
# lets a case cover the aggregate rule — a regex may match nothing in the
# workspace and still be right about devtools/.
#
# The e2e half is here rather than beside a scripts/e2e/ script because it is
# the same contract on the same knob, and it asserts the resolved `ginkgo run`
# command (`make -n`) rather than launching the tier: the wiring is what this
# repo owns, and --fail-on-empty is ginkgo's own guarantee. Runs under `make
# check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

fails=0
WORK="$REPO_ROOT/tmp/go-test-run-filter-test.$$"
trap 'rm -rf "$WORK"' EXIT INT TERM
mkdir -p "$WORK/bin" "$WORK/out"

REAL_GO="$(command -v go)"
ARGV_LOG="$WORK/argv.txt"

# Canned `go test -v` output. The no-match shape is verbatim what `go test -run
# <nonsense>` prints: an `ok` line per package carrying `[no tests to run]`, no
# `=== RUN` anywhere, and exit 0.
cat >"$WORK/out/matched" <<'EOF'
=== RUN   TestSelected
--- PASS: TestSelected (0.00s)
PASS
ok  	github.com/actions-gateway/github-actions-gateway/api/apilabels	0.012s
EOF
cat >"$WORK/out/empty" <<'EOF'
testing: warning: no tests to run
PASS
ok  	github.com/actions-gateway/github-actions-gateway/api/apilabels	0.008s [no tests to run]
EOF

cat >"$WORK/bin/go" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if [[ "\${1:-}" == test ]]; then
	printf '%s\n' "\$*" >>"$ARGV_LOG"
	if [[ "\$PWD" == */devtools ]]; then
		cat "$WORK/out/\$STUB_DEVTOOLS"
	else
		cat "$WORK/out/\$STUB_WORKSPACE"
	fi
	exit 0
fi
exec "$REAL_GO" "\$@"
EOF
chmod +x "$WORK/bin/go"

# run_gate WORKSPACE_OUT DEVTOOLS_OUT [ENV=VAL ...] — run go-test.sh against the
# shim with the named canned outputs, leaving the run's argv in $ARGV_LOG and its
# status in $rc. GAG_HEAVY_BUILD_LOCK_HELD skips the machine-wide build
# semaphore, which would otherwise re-exec the script behind a lock this suite
# has no reason to contend for.
# gate_flags carries the script's own flags (--race), which are positional and
# so cannot ride in with the environment assignments.
rc=0
argv=""
gate_flags=()
run_gate() {
	local ws="$1" dt="$2"
	shift 2
	: >"$ARGV_LOG"
	rc=0
	env PATH="$WORK/bin:$PATH" GAG_HEAVY_BUILD_LOCK_HELD=1 \
		STUB_WORKSPACE="$ws" STUB_DEVTOOLS="$dt" "$@" \
		scripts/go/go-test.sh "${gate_flags[@]}" >"$WORK/stdout" 2>"$WORK/stderr" || rc=$?
	argv="$(cat "$ARGV_LOG")"
}

# expect_status NAME WANT — the run's exit status.
expect_status() {
	local name="$1" want="$2"
	if [[ "$rc" == "$want" ]]; then
		printf 'ok   status   %-28s -> %s\n' "$name" "$rc"
	else
		printf 'FAIL status   %-28s want=%s got=%s\n' "$name" "$want" "$rc" >&2
		printf '     stderr: %s\n' "$(cat "$WORK/stderr")" >&2
		fails=$((fails + 1))
	fi
}

# expect_argv NAME NEEDLE WANT — WANT is 'yes' or 'no'; NEEDLE is matched
# against every recorded `go test` argv joined together.
expect_argv() {
	local name="$1" needle="$2" want="$3" got=no
	[[ "$argv" == *"$needle"* ]] && got=yes
	if [[ "$got" == "$want" ]]; then
		printf 'ok   argv     %-28s %s %s\n' "$name" "$want" "$needle"
	else
		printf 'FAIL argv     %-28s want=%s got=%s for [%s] in:\n%s\n' \
			"$name" "$want" "$got" "$needle" "$argv" >&2
		fails=$((fails + 1))
	fi
}

# A RUN that selects something: the filter reaches `go test`, and -v -count=1
# ride along so the run is uncached and its per-test markers are visible.
run_gate matched matched RUN=TestSelected
expect_status run-matches 0
expect_argv run-matches '-run TestSelected' yes
expect_argv run-matches '-count=1' yes
expect_argv run-matches ' -v ' yes
expect_argv run-matches '-race' no

# The defect Q680 names: `go test` reports success on a filter that matched
# nothing, so the guard has to be the one to fail, and it has to say why.
run_gate empty empty RUN=TestNoSuchThing
expect_status run-matches-nothing 1
if grep -q "TestNoSuchThing" "$WORK/stderr" && grep -q "matched no tests" "$WORK/stderr"; then
	printf 'ok   stderr   %-28s names the filter that missed\n' run-matches-nothing
else
	printf 'FAIL stderr   %-28s want a message naming RUN, got: %s\n' \
		run-matches-nothing "$(cat "$WORK/stderr")" >&2
	fails=$((fails + 1))
fi

# The guard is aggregate over every module, not per invocation: go-test.sh runs
# the workspace and each non-workspace module separately, and a name that lives
# only in devtools/ is a legitimate match.
run_gate empty matched RUN=TestOnlyInDevtools
expect_status match-outside-workspace 0

# Unfiltered runs are untouched — no -run, no forced -count=1, and the pre-existing
# caching and progress behaviour intact. TEST_PROGRESS_INTERVAL=0 takes the plain
# path, so the shim never has to stand in for the heartbeat renderer's build.
run_gate matched matched TEST_PROGRESS_INTERVAL=0
expect_status run-unset 0
expect_argv run-unset '-run ' no
expect_argv run-unset '-count=1' no

# The race gate takes the same filter, and the same guard.
gate_flags=(--race)
run_gate matched matched RUN=TestSelected
expect_status run-with-race 0
expect_argv run-with-race '-race' yes
expect_argv run-with-race '-run TestSelected' yes
run_gate empty empty RUN=TestNoSuchThing
expect_status run-with-race-matches-nothing 1
gate_flags=()

# --- e2e target: the resolved `ginkgo run` command -------------------------
# e2e_cmd VAR=VAL... — the recipe `make e2e` would run, on one line.
e2e_cmd() {
	make -n e2e "$@" | tr '\n' ' '
}

# expect_e2e NAME CMD NEEDLE WANT — WANT is 'yes' or 'no'.
expect_e2e() {
	local name="$1" cmd="$2" needle="$3" want="$4" got=no
	[[ "$cmd" == *"$needle"* ]] && got=yes
	if [[ "$got" == "$want" ]]; then
		printf 'ok   e2e      %-28s %s %s\n' "$name" "$want" "$needle"
	else
		printf 'FAIL e2e      %-28s want=%s got=%s for [%s]\n' "$name" "$want" "$got" "$needle" >&2
		fails=$((fails + 1))
	fi
}

plain="$(e2e_cmd)"
focused="$(e2e_cmd RUN='provisions a worker pod')"
both="$(e2e_cmd RUN=scales SUITE=single-node)"

expect_e2e e2e-unfiltered "$plain" '--focus' no
expect_e2e e2e-focus "$focused" "--focus 'provisions a worker pod'" yes
# SUITE selects the labelled subset and RUN narrows within it — the two compose
# rather than replacing one another.
expect_e2e e2e-focus-with-suite "$both" "--focus 'scales'" yes
expect_e2e e2e-focus-with-suite "$both" "--label-filter '!multi-node'" yes
# Unconditional, so a SUITE that selects nothing fails the same way a RUN does.
expect_e2e e2e-fail-on-empty "$plain" '--fail-on-empty' yes
expect_e2e e2e-fail-on-empty "$focused" '--fail-on-empty' yes

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall RUN= filter assertions passed\n'
