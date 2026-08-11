#!/usr/bin/env bash
#
# Assertions for the envtest suite budget wrapper (Q741).
#
# The contract has two halves and both have to be pinned, because each fails
# silently in the shape of the other: a run that finishes inside its budget must
# report the total and say nothing alarming, and a run that exceeds it must say
# the SUITE ran out of time — naming the panic's test as a bystander, since that
# misattribution is the whole reason the wrapper exists (Q166 spent two wrong
# hypotheses on it). A banner that stopped firing and one that fires on every
# green run are equally wrong, so every case asserts presence and absence.
#
# `go test` is a PATH shim recording argv and replaying canned output at a
# canned status: the wrapper's inputs are that output and that status, so no
# envtest binary, apiserver, or module compile is involved.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

fails=0
WORK="$REPO_ROOT/tmp/go-test-integration-test.$$"
trap 'rm -rf "$WORK"' EXIT INT TERM
mkdir -p "$WORK/bin" "$WORK/out"

REAL_GO="$(command -v go)"
ARGV_LOG="$WORK/argv.txt"

pkg=github.com/actions-gateway/github-actions-gateway/gmc/internal/controller/integration

# Canned `go test` output. The breach shape is what Go actually prints when a
# test binary outlives -timeout: a panic naming the test that happened to be
# running, then the package's own FAIL line carrying the real total.
printf 'ok  \t%s\t120.500s\n' "$pkg" >"$WORK/out/fast"
printf 'ok  \t%s\t(cached)\n' "$pkg" >"$WORK/out/cached"
printf 'ok  \t%s\t540.000s\n' "$pkg" >"$WORK/out/near"
cat >"$WORK/out/breach" <<EOF
panic: test timed out after 10m0s
running tests:
	TestGMC_CredRotation_PodTemplateAnnotation (1s)

goroutine 4711 [running]:
FAIL	${pkg}	600.117s
EOF

cat >"$WORK/bin/go" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if [[ "\${1:-}" == test ]]; then
	printf '%s\n' "\$*" >>"$ARGV_LOG"
	cat "$WORK/out/\$STUB_OUT"
	exit "\${STUB_RC:-0}"
fi
exec "$REAL_GO" "\$@"
EOF
chmod +x "$WORK/bin/go"

# run_gate STUB_OUT STUB_RC [ENV=VAL ...] -- [script args] — run the wrapper
# against the shim. GAG_HEAVY_BUILD_LOCK_HELD skips the machine-wide build
# semaphore, which would otherwise re-exec the script behind a lock this suite
# has no reason to contend for.
rc=0
argv=""
run_gate() {
	local out="$1" status="$2" env_args=() script_args=(cmd/gmc)
	shift 2
	while (($#)); do
		[[ "$1" == -- ]] && { shift; script_args=("$@"); break; }
		env_args+=("$1")
		shift
	done
	: >"$ARGV_LOG"
	rc=0
	env PATH="$WORK/bin:$PATH" GAG_HEAVY_BUILD_LOCK_HELD=1 \
		STUB_OUT="$out" STUB_RC="$status" "${env_args[@]}" \
		scripts/go/go-test-integration.sh "${script_args[@]}" \
		>"$WORK/stdout" 2>"$WORK/stderr" || rc=$?
	argv="$(cat "$ARGV_LOG")"
}

expect_status() {
	local name="$1" want="$2"
	if [[ "$rc" == "$want" ]]; then
		printf 'ok   status   %-30s -> %s\n' "$name" "$rc"
	else
		printf 'FAIL status   %-30s want=%s got=%s\n' "$name" "$want" "$rc" >&2
		printf '     stderr: %s\n' "$(cat "$WORK/stderr")" >&2
		fails=$((fails + 1))
	fi
}

expect_argv() {
	local name="$1" needle="$2" want="$3" got=no
	[[ "$argv" == *"$needle"* ]] && got=yes
	if [[ "$got" == "$want" ]]; then
		printf 'ok   argv     %-30s %s %s\n' "$name" "$want" "$needle"
	else
		printf 'FAIL argv     %-30s want=%s got=%s for [%s] in:\n%s\n' \
			"$name" "$want" "$got" "$needle" "$argv" >&2
		fails=$((fails + 1))
	fi
}

# expect_output NAME STREAM NEEDLE WANT — STREAM is stdout or stderr.
expect_output() {
	local name="$1" stream="$2" needle="$3" want="$4" got=no
	grep -qF "$needle" "$WORK/$stream" && got=yes
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-8s %-30s %s %s\n' "$stream" "$name" "$want" "$needle"
	else
		printf 'FAIL %-8s %-30s want=%s got=%s for [%s] in:\n%s\n' \
			"$stream" "$name" "$want" "$got" "$needle" "$(cat "$WORK/$stream")" >&2
		fails=$((fails + 1))
	fi
}

# A run well inside its budget: the flags the tier is defined by reach `go test`,
# the total is reported, and nothing warns.
run_gate fast 0
expect_status inside-budget 0
expect_argv inside-budget '-race' yes
expect_argv inside-budget '-tags integration' yes
expect_argv inside-budget '-timeout 10m' yes
expect_argv inside-budget './internal/controller/integration/...' yes
expect_output inside-budget stdout 'used 120.5s of its 10m budget (20%)' yes
expect_output inside-budget stderr 'ran out of its' no
expect_output inside-budget stderr 'within 80% of the budget' no

# Approaching it: still green, but the run says so before a later one breaches.
# This is the signal that was missing — a suite creeping up on the cliff was
# indistinguishable from one with room to spare until the day it panicked.
run_gate near 0
expect_status near-budget 0
expect_output near-budget stderr 'used 540.0s of its 10m budget (90%)' yes
expect_output near-budget stderr 'within 80% of the budget' yes
expect_output near-budget stderr 'ran out of its' no

# The breach. The suite is named as what ran out of time, and the test the panic
# names is called a bystander outright.
run_gate breach 1
expect_status over-budget 1
expect_output over-budget stderr 'the cmd/gmc integration suite ran out of its 10m budget' yes
expect_output over-budget stderr 'bystander' yes
expect_output over-budget stderr 'used 600.1s of its 10m budget (100%)' yes
# Absent here: the annotation belongs to CI, and a local run should not print
# GitHub workflow-command syntax at a developer.
expect_output over-budget stdout '::error' no
expect_output over-budget stderr '::error' no

# On CI the annotation is what most readers ever see of a failed step, so the
# disambiguation has to reach it and not just the log.
run_gate breach 1 GITHUB_ACTIONS=true
expect_status over-budget-ci 1
expect_output over-budget-ci stdout '::error title=integration suite over budget::' yes
run_gate fast 0 GITHUB_ACTIONS=true
expect_output green-ci stdout '::error' no

# A cached package ran nothing, so there is no wall clock to report. Reporting
# one would be worse than silence: `(cached)` carries no elapsed, and a budget
# line invented from it would read as a measurement of this run.
run_gate cached 0
expect_status cached 0
expect_argv cached '-count=1' no
expect_output cached stdout 'budget (' no
expect_output cached stderr 'budget (' no

# The budget is a knob, and the knob has to reach `go test` — a wrapper that
# reported one budget and enforced another would be worse than no wrapper.
run_gate fast 0 INTEGRATION_TIMEOUT=3m
expect_argv budget-override '-timeout 3m' yes
expect_output budget-override stdout 'of its 3m budget' yes

# An explicit pattern replaces the default; the module is still where it runs.
run_gate fast 0 -- cmd/agc ./internal/controller/integration/suite_test.go
expect_argv explicit-args 'suite_test.go' yes

# A module the tier does not have is a usage error, not a run.
run_gate fast 0 -- cmd/proxy
expect_status unknown-module 2
expect_argv unknown-module 'go test' no

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall integration suite budget assertions passed\n'
