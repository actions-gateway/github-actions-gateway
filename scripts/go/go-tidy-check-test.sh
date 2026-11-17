#!/usr/bin/env bash
#
# Unit tests for scripts/go/go-tidy-check.sh and the go_retry_transient helper
# it shares with vendor-check.sh (Q1314). One property: a dropped
# module-proxy stream during the tidy flow is retried, and never reads as
# untidy module files.
#
# The tidy flow is run against a PATH shim that answers `go mod tidy` and
# `go work sync` itself and forwards every other `go` subcommand to the real
# toolchain. The shim fails its first N tidies with stderr copied from a real CI
# sighting, so the network is never touched and no tracked file is rewritten.
# The gate's own `git diff` still runs, so the module files in this checkout
# must be clean. Runs under `make check` (via `make scripts-test`).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

fails=0
WORK="$REPO_ROOT/tmp/go-tidy-check-test.$$"
trap 'rm -rf "$WORK"' EXIT INT TERM
mkdir -p "$WORK/bin"

REAL_GO="$(command -v go)"
CALLS="$WORK/tidy-calls"

# Verbatim from unit-test.yml run 44003152766, job tidy-check (2026-11-17).
STREAM_ERR='go: gonum.org/v1/gonum/stat/distuv: gonum.org/v1/gonum@v0.17.0: read "https://proxy.golang.org/gonum.org/v1/gonum/@v/v0.17.0.zip": stream error: stream ID 241; INTERNAL_ERROR; received from peer'
OTHER_ERR='go: example.com/missing@v1.0.0: reading example.com/missing/go.mod: 404 Not Found'

# The shim fails the first $SHIM_FAILS tidies with $SHIM_ERR, then succeeds.
cat >"$WORK/bin/go" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if [[ "\${1:-}" == mod && "\${2:-}" == tidy ]]; then
	n=\$(( \$(cat "$CALLS") + 1 ))
	echo "\$n" >"$CALLS"
	if (( n <= SHIM_FAILS )); then
		printf '%s\n' "\$SHIM_ERR" >&2
		exit 1
	fi
	exit 0
fi
if [[ "\${1:-}" == work && "\${2:-}" == sync ]]; then
	exit 0
fi
exec "$REAL_GO" "\$@"
EOF
chmod +x "$WORK/bin/go"

# check NAME FAILS ERR WANT_RC WANT_CALLS WANT_TEXT — run the gate with the shim
# failing FAILS tidies with ERR; assert the exit status (0 or nonzero), how many
# tidies ran, and that the output holds WANT_TEXT.
check() {
	local name="$1" shim_fails="$2" shim_err="$3" want_rc="$4" want_calls="$5" want_text="$6"
	local rc=0 out calls
	echo 0 >"$CALLS"
	out="$(PATH="$WORK/bin:$PATH" SHIM_FAILS="$shim_fails" SHIM_ERR="$shim_err" \
		GO_FETCH_ATTEMPTS=3 GO_FETCH_RETRY_DELAY=0 \
		scripts/go/go-tidy-check.sh 2>&1)" || rc=$?
	calls="$(cat "$CALLS")"
	local got_rc=0
	(( rc != 0 )) && got_rc=nonzero
	if [[ "$got_rc" != "$want_rc" ]]; then
		printf 'FAIL %-40s exit want=%s got=%s\n%s\n' "$name" "$want_rc" "$rc" "$out" >&2
		fails=$((fails + 1))
	elif (( calls != want_calls )); then
		printf 'FAIL %-40s tidies want=%d got=%d\n%s\n' "$name" "$want_calls" "$calls" "$out" >&2
		fails=$((fails + 1))
	elif [[ "$out" != *"$want_text"* ]]; then
		printf 'FAIL %-40s output lacks %q\n%s\n' "$name" "$want_text" "$out" >&2
		fails=$((fails + 1))
	else
		printf 'ok   %-40s exit=%s tidies=%d\n' "$name" "$rc" "$calls"
	fi
}

# The tidy flow runs one `go mod tidy` per module; count them from the checkout
# so a new module does not break the expected call counts.
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
modules="$( (workspace_modules; nonworkspace_modules) | grep -c .)"

# Control: with no injected failure the gate is green and tidies each module once.
check 'clean run' 0 "$STREAM_ERR" 0 "$modules" 'are tidy'

# One dropped stream: the failed pass stops at the first module, and the retry
# re-runs the whole flow, so the gate goes green.
check 'one stream error is retried' 1 "$STREAM_ERR" 0 "$((modules + 1))" 'retrying'

# The proxy stays down: three attempts, then a failure naming the network.
check 'persistent stream error is bounded' 99 "$STREAM_ERR" nonzero 3 'not untidy or out-of-sync module files'

# Any other tidy failure is deterministic: one attempt, no retry message.
check 'other errors are not retried' 1 "$OTHER_ERR" nonzero 1 '404 Not Found'

if (( fails > 0 )); then
	echo "go-tidy-check-test: $fails failure(s)" >&2
	exit 1
fi
echo "go-tidy-check-test: all assertions passed"
