#!/usr/bin/env bash
#
# Behavioural tests for scripts/agent/record-launch-guard.py — the hook that
# makes the launch record fire (Q739).
#
# Both directions are asserted because both fail silently. A hook that stopped
# denying leaves every heavy background run without a stop handle, which is the
# state Q739 was filed about and which nothing else reports; one that denies too
# much turns an ordinary background call into a refusal the session has to
# override, and a guard overridden by reflex has stopped meaning anything.
#
# The hook is driven as a subprocess against synthetic payloads, the way
# claude-go-throttle-hook-test.sh drives its binary — the JSON contract with
# Claude Code is what is under test, so it is exercised through the same
# boundary Claude Code uses.
#
# Two of the assertions are over the real repository rather than a fixture,
# because a fixture can only hold what its author thought of: the live
# .claude/foreground-guard.json must carry at least one numeric slow pattern
# (with none, every deny case below would pass for the wrong reason) and no
# target-aware one (which the hook cannot decide and stays silent on).
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

HOOK="${REPO_ROOT}/scripts/agent/record-launch-guard.py"
SCRATCH="${REPO_ROOT}/tmp/record-launch-guard-test.$$"
trap 'rm -rf "${SCRATCH}"' EXIT
mkdir -p "${SCRATCH}"

fails=0

fail() {
	printf 'record-launch-guard-test: FAIL: %s\n' "$1" >&2
	fails=$((fails + 1))
}

# drive runs the hook against one payload and prints its stdout. project_dir is
# explicit on every call so no case depends on an ambient CLAUDE_PROJECT_DIR.
drive() {
	local project_dir="$1" payload="$2"
	CLAUDE_PROJECT_DIR="${project_dir}" python3 "${HOOK}" <<<"${payload}"
}

# bg_payload builds the ordinary shape: a backgrounded Bash call.
bg_payload() {
	python3 -c 'import json,sys; print(json.dumps({"tool_name":"Bash","tool_input":{"command":sys.argv[1],"run_in_background":True}}))' "$1"
}

assert_denies() {
	local label="$1" out
	out="$(drive "${REPO_ROOT}" "$(bg_payload "$2")")"
	[[ -n "${out}" ]] || {
		fail "${label}: expected a deny, got silence"
		return
	}
	local decision
	decision="$(printf '%s' "${out}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["hookSpecificOutput"]["permissionDecision"])')"
	[[ "${decision}" == "deny" ]] || fail "${label}: expected deny, got ${decision}"
}

assert_silent() {
	local label="$1" out
	out="$(drive "${REPO_ROOT}" "$(bg_payload "$2")")"
	[[ -z "${out}" ]] || fail "${label}: expected silence, got: ${out}"
}

# --- must deny: the registered heavy tiers, backgrounded and unwrapped -------

assert_denies 'make test-race' 'make test-race'
assert_denies 'make -C test-integration' 'make -C cmd/agc test-integration'
assert_denies 'make e2e' 'make e2e SUITE=single-node'
assert_denies 'go test -race' 'go test -race ./...'
assert_denies 'dogfood sentinel' 'bash scripts/dogfood/release-sentinel.sh'

# The shape exit-status-guard requires. The wrapper propagates the run's status,
# so the tail still reports the run and this must still be denied for the
# missing handle rather than waved through for looking careful.
# shellcheck disable=SC2016  # the payload is a command string the hook parses,
# so `$?` and `$rc` must reach it as literal text — expanding them here would
# assert against a command no session would ever send.
assert_denies 'exit-status shape' \
	'make test-race > tmp/race.log 2>&1; rc=$?; echo "EXIT=$rc"; exit $rc'

# --- must stay silent -------------------------------------------------------

assert_silent 'already wrapped' \
	'scripts/agent/record-launch.sh make test-race > tmp/race.log 2>&1'
assert_silent 'override prefix' \
	'RECORD_LAUNCH_GUARD_OVERRIDE=measuring-the-guard make test-race'
assert_silent 'unregistered command' 'make check > tmp/check.log 2>&1'

# The one documented exception. It is out by construction — it matches no
# registered pattern — and this pins that, because the watcher's auto-approval
# needs exactly three bare tokens and a deny here would strand an unattended
# worker with its PR unwatched (testing.md#the-pr-sentinel-watcher-is-the-
# exception-no-wrapper-no-redirect).
assert_silent 'pr-sentinel watcher' \
	'bash "/Users/x/.claude/plugins/cache/pr-sentinel/scripts/pr-sentinel-watch.sh" 1234'

# Mention-only: the registry's patterns are anchored so a read of a registered
# script is not an invocation of it. Asserted here too because this hook fires
# on a different event than the one foreground-guard-patterns-test.sh covers.
assert_silent 'mention only' \
	'git show origin/main:scripts/dogfood/release-sentinel.sh'

# A foreground call is foreground-guard's Class B, not this hook's.
out="$(drive "${REPO_ROOT}" '{"tool_name":"Bash","tool_input":{"command":"make test-race"}}')"
[[ -z "${out}" ]] || fail "foreground call: expected silence, got: ${out}"

out="$(drive "${REPO_ROOT}" '{"tool_name":"Edit","tool_input":{"command":"make test-race","run_in_background":true}}')"
[[ -z "${out}" ]] || fail "non-Bash tool: expected silence, got: ${out}"

out="$(drive "${REPO_ROOT}" 'not json at all')"
[[ -z "${out}" ]] || fail "malformed payload: expected silence, got: ${out}"

# --- the deny's own contract ------------------------------------------------

# Read via a default rather than letting json.load raise: with the emit path
# broken every case above already names itself, and a traceback here would
# replace those findings with one stack trace.
deny_reason="$(drive "${REPO_ROOT}" "$(bg_payload 'make test-race')" |
	python3 -c 'import json,sys
raw = sys.stdin.read()
try:
    print(json.loads(raw)["hookSpecificOutput"]["permissionDecisionReason"])
except (ValueError, KeyError, TypeError):
    print("")')"

# Position 0, because a deny leaves no decision record and the reason string is
# the only evidence this hook ran.
[[ "${deny_reason}" == record-launch-guard:\ * ]] ||
	fail "deny reason must open 'record-launch-guard: ', got: ${deny_reason:0:40}"

# The fix has to be runnable as written, and the override has to come after it.
[[ "${deny_reason}" == *"scripts/agent/record-launch.sh make test-race"* ]] ||
	fail 'deny reason must paste the wrapped command'
[[ "${deny_reason}" == *RECORD_LAUNCH_GUARD_OVERRIDE=* ]] ||
	fail 'deny reason must name the override'
wrapper_at="${deny_reason%%scripts/agent/record-launch.sh*}"
override_at="${deny_reason%%RECORD_LAUNCH_GUARD_OVERRIDE=*}"
((${#wrapper_at} < ${#override_at})) ||
	fail 'the override must be named after the fix, not before it'

# --- controls ---------------------------------------------------------------

# The deny depends on the registry: empty it and the same command goes silent.
# Without this, every deny above is consistent with a hook that refuses
# unconditionally.
mkdir -p "${SCRATCH}/empty/.claude"
printf '%s\n' '{"slow": {"commands": {}}}' >"${SCRATCH}/empty/.claude/foreground-guard.json"
out="$(drive "${SCRATCH}/empty" "$(bg_payload 'make test-race')")"
[[ -z "${out}" ]] || fail "empty registry: expected silence, got: ${out}"

# And a project with no config at all fails open rather than denying.
mkdir -p "${SCRATCH}/none"
out="$(drive "${SCRATCH}/none" "$(bg_payload 'make test-race')")"
[[ -z "${out}" ]] || fail "absent config: expected silence, got: ${out}"

# --- over the real repository -----------------------------------------------

python3 - <<'PY' || fails=$((fails + 1))
import json
import sys

with open('.claude/foreground-guard.json', encoding='utf-8') as fh:
    cmds = json.load(fh)['slow']['commands']

numeric = [p for p, v in cmds.items() if isinstance(v, (int, float)) and not isinstance(v, bool)]
target_aware = [p for p, v in cmds.items() if isinstance(v, dict)]

ok = True
if not numeric:
    print('record-launch-guard-test: FAIL: the live slow registry has no numeric '
          'pattern, so every deny case in this suite passes for the wrong reason',
          file=sys.stderr)
    ok = False
if target_aware:
    # The hook reads only {regex: ms}. A {command: {glob: ms}} entry needs
    # foreground-guard's argument matching to decide, so the hook stays silent
    # on one — a gap that must be closed deliberately, not discovered later.
    print('record-launch-guard-test: FAIL: target-aware slow entries %s are '
          'invisible to record-launch-guard.py — teach it the form or register '
          'them as plain regexes' % target_aware, file=sys.stderr)
    ok = False
sys.exit(0 if ok else 1)
PY

if ((fails > 0)); then
	echo "record-launch-guard-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "record-launch-guard-test: ok"
