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

# Both exemptions have to be uses rather than mentions, or a real launch buys
# itself an exemption by naming one. This is the anchoring question pointed the
# other way: the mention-only cases below are a deny that should be silence,
# and these are a silence that should be a deny. The second is the direction
# the hook exists to prevent, so it is asserted here rather than inferred from
# those.
assert_denies 'wrapper merely mentioned' \
	'make test-race; echo "see scripts/agent/record-launch.sh"'
assert_denies 'override merely mentioned' \
	'make test-race; echo "RECORD_LAUNCH_GUARD_OVERRIDE=x is the escape hatch"'
# Three spellings, because a narrowing that only looks inside quotes passes the
# echo above and fails both of these.
assert_denies 'wrapper in a comment' \
	'make test-race  # unlike record-launch.sh'
assert_denies 'wrapper in an argument' \
	'make test-race ARGS=scripts/agent/record-launch.sh'

# Both exemptions are per command, not per string. As `any(segment is
# exempt)` one wrapped or overridden member covered every other member, so a
# real tier launched beside a token wrapped call with no handle at all. The
# suite could not see it because every exemption fixture above is a single
# command, which is the same blind spot as the chain fixtures had.
assert_denies 'wrapped member, tier beside it' \
	'scripts/agent/record-launch.sh true; make test-race'
assert_denies 'wrapped member, e2e beside it' \
	'scripts/agent/record-launch.sh echo x && make e2e SUITE=single-node'
assert_denies 'wrapped tier, tier beside it' \
	'scripts/agent/record-launch.sh make check; make test-race'
assert_denies 'tier first, wrapped member after' \
	'make test-race; scripts/agent/record-launch.sh true'
assert_denies 'override on another member' \
	'RECORD_LAUNCH_GUARD_OVERRIDE=why true; make test-race'

# And the legitimate forms of the same shapes must still go silent, or the
# tightening above has simply broken the exemptions.
assert_silent 'wrapped, then an echo' \
	'scripts/agent/record-launch.sh make test-race; echo done'
assert_silent 'override, then an echo' \
	'RECORD_LAUNCH_GUARD_OVERRIDE=why make test-race; echo done'
assert_silent 'every tier wrapped' \
	'scripts/agent/record-launch.sh make test-race; scripts/agent/record-launch.sh make e2e'

# Two registered tiers in one command: no single wrapper on the front covers
# both, so there is no paste to hand back.
assert_denies 'two tiers in one chain' 'make test-race; make e2e SUITE=single-node'

# Whitespace bash treats as insignificant. The registry's patterns want exactly
# one space (`make (-C [^ ]+ )?test-race\b`), and `(-C [^ ]+ )?` cannot absorb a
# second one, so all of these run a real tier and match no pattern as written.
# The hook collapses whitespace runs before matching, which closes the gap on
# its own side; the registry keeps it, and that half is Q1123.
assert_denies 'two spaces' 'make  test-race'
assert_denies 'tab separator' "$(printf 'make\ttest-race')"
assert_denies 'escaped newline' "$(printf 'make \\\ntest-race')"
assert_denies 'two spaces after -C' 'make -C  cmd/agc test-integration'
assert_denies 'two spaces in go test' 'go  test -race ./...'

# The one documented exception. It is out by construction — it matches no
# registered pattern — and this pins that, because the watcher's auto-approval
# needs exactly three bare tokens and a deny here would strand an unattended
# worker with its PR unwatched (testing.md#the-pr-sentinel-watcher-is-the-
# exception-no-wrapper-no-redirect).
assert_silent 'pr-sentinel watcher' \
	'bash "/Users/x/.claude/plugins/cache/pr-sentinel/scripts/pr-sentinel-watch.sh" 1234'

# Mention-only. Five of the seven live registry patterns carry no command
# anchor of their own, so these all denied until the hook started matching at
# command position. The first case is the anchored dogfood family; the rest are
# the `make`/`go test` families, which inherit nothing and so are the ones that
# actually exercise the anchoring. Both directions: every command below names a
# registered tier in text, and none of them runs one.
assert_silent 'mention: dogfood read' \
	'git show origin/main:scripts/dogfood/release-sentinel.sh'
assert_silent 'mention: git grep' \
	"git grep -n 'make e2e' docs/"
assert_silent 'mention: grep -r' \
	"grep -rn 'go test -race' docs/"
assert_silent 'mention: sed address' \
	"sed -n '/make test-race/p' docs/development/testing.md"
assert_silent 'mention: commit message' \
	'git commit -m "test: stabilise make test-race flake"'
assert_silent 'mention: pr body' \
	'gh pr create --body "runs make test-integration nightly"'
assert_silent 'mention: quoted semicolon' \
	'git commit -m "check; make test-race"'

# A quoted separator must not split the command: without quote-aware lexing the
# case above becomes a second segment that starts with the tier name.

# Command position survives an assignment prefix, a wrapper, and a chain, so
# anchoring must not cost a real launch.
assert_denies 'leading assignment' 'FOO=1 make test-race'
assert_denies 'env wrapper' 'env FOO=1 make test-race'
assert_denies 'second in chain' 'cd cmd/agc && make test-race'
assert_denies 'after a semicolon' 'make check; make test-race'

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

# A leading `VAR=val` must be hoisted ahead of the wrapper. record-launch.sh
# runs its argv directly, so an assignment left after the wrapper name is
# executed as a program: `record-launch.sh FOO=1 make test-race` exits 127
# with `FOO=1: command not found` and the run never starts. Both dogfood
# patterns match an assignment prefix explicitly, so the deny fires on this
# shape and a paste that cannot run is a deny with no remediation in it.
prefixed_reason="$(drive "${REPO_ROOT}" "$(bg_payload 'FOO=1 make test-race')" |
	python3 -c 'import json,sys
raw = sys.stdin.read()
try:
    print(json.loads(raw)["hookSpecificOutput"]["permissionDecisionReason"])
except (ValueError, KeyError, TypeError):
    print("")')"
[[ "${prefixed_reason}" == *"FOO=1 scripts/agent/record-launch.sh make test-race"* ]] ||
	fail 'a leading assignment must be hoisted ahead of the wrapper'
[[ "${prefixed_reason}" != *"record-launch.sh FOO=1"* ]] ||
	fail 'the paste must not leave an assignment after the wrapper name'

reason_for() {
	drive "${REPO_ROOT}" "$(bg_payload "$1")" |
		python3 -c 'import json,sys
raw = sys.stdin.read()
try:
    print(json.loads(raw)["hookSpecificOutput"]["permissionDecisionReason"])
except (ValueError, KeyError, TypeError):
    print("")'
}

# A trailing `&` must not survive into the paste. record-launch.sh backgrounds
# the run itself and propagates its exit status; echoing the `&` back
# double-backgrounds the wrapper and discards the status the same sentence
# promises the caller.
amp_reason="$(reason_for 'bash scripts/dogfood/release-sentinel.sh &')"
[[ "${amp_reason}" != *"release-sentinel.sh &"* ]] ||
	fail 'the paste must drop a trailing &'

# A redirect is not a chain. Without dropping the redirection target this reads
# as three segments and takes the chain branch below.
redir_reason="$(reason_for 'make test-race > tmp/race.log 2>&1')"
[[ "${redir_reason}" == *"scripts/agent/record-launch.sh make test-race > tmp/race.log"* ]] ||
	fail 'a redirected single command must still be pasted whole'

# A chain gets an instruction, never a front-wrapped paste. Pasting the wrapper
# onto `make check; make test-race` wraps `make check` and leaves the tier
# running unwrapped, so a session that complied would have defeated the hook.
chain_reason="$(reason_for 'make check; make test-race')"
[[ "${chain_reason}" != *"record-launch.sh make check;"* ]] ||
	fail 'the paste must not wrap the first member of a chain'
[[ "${chain_reason}" == *"scripts/agent/record-launch.sh make test-race"* ]] ||
	fail 'a chain deny must name the matching command'
[[ "${chain_reason}" == *'not the first command here'* ]] ||
	fail 'a chain deny must say why the wrapper cannot go on the front'
# shellcheck disable=SC2016  # the backticks are literal markdown in the deny
# text, so the needle must stay unexpanded.
[[ "${chain_reason}" == *'leave `make test-race` running unwrapped'* ]] ||
	fail 'a chain deny must name the command that would be left unwrapped'

# A chain whose FIRST member is the tier still takes the paste, because the
# shell hands the wrapper only that member: `record-launch.sh make test-race;
# echo done` wraps correctly. Declining here withheld a runnable fix, and the
# explanation that came instead named the wrapped command as the one left
# unwrapped, so the two halves of one sentence contradicted each other. Every
# other chain fixture puts the tier second, so nothing else reaches this.
first_reason="$(reason_for 'make test-race; echo "see record-launch.sh"')"
[[ "${first_reason}" == *'scripts/agent/record-launch.sh make test-race; echo'* ]] ||
	fail 'a chain led by the tier must still be pasted'
[[ "${first_reason}" != *'not the first command here'* ]] ||
	fail 'a chain led by the tier must not claim the tier is not first'

# --- the paste must not itself be a launch ----------------------------------

# A general property rather than a case: whatever command the deny hands back,
# feeding it to the hook again must produce silence. A paste that still denies
# is a fix that does not fix, and a paste that goes silent while a registered
# tier still runs unwrapped is worse -- the session complies and the launch
# escapes. That is exactly what `make test-race; make e2e` did: it pasted one
# wrapper on the front, which silenced the hook because the first member was
# then wrapped, while the second tier launched with no handle.
#
# Nothing here had to think of that case, which is the point of asserting a
# property rather than a list of shapes.
#
# Its reach has a limit worth knowing. The round-trip only sees a bad paste
# because the exemption below is per command: with the old per-string
# exemption, the pasted string went silent for the same reason the original
# bypass did, so one defect hid both itself and its detector. A property test
# is only as strong as the predicate it round-trips through.
roundtrip() {
	local label="$1" command="$2" reason pasted out
	reason="$(reason_for "${command}")"
	[[ -n "${reason}" ]] || {
		fail "${label}: expected a deny to round-trip"
		return 0
	}
	pasted="${reason#*tmp/launches/: \`}"
	# No paste offered (the multi-tier branch hands back an instruction), so
	# there is nothing to feed back. `return 0` and not a bare `return`: the
	# latter carries the failed test's status out of the function and `set -e`
	# then kills the suite with no output at all.
	[[ "${pasted}" != "${reason}" ]] || return 0
	pasted="${pasted%%\`*}"
	out="$(drive "${REPO_ROOT}" "$(bg_payload "${pasted}")")"
	[[ -z "${out}" ]] ||
		fail "${label}: the pasted fix is itself denied: ${pasted}"
}

roundtrip 'lone tier'          'make test-race'
roundtrip 'redirected tier'    'make test-race > tmp/race.log 2>&1'
roundtrip 'leading assignment' 'FOO=1 make test-race'
roundtrip 'tier then echo'     'make test-race; echo done'
roundtrip 'tier then wrapped'  'make test-race; scripts/agent/record-launch.sh true'
roundtrip 'two tiers'          'make test-race; make e2e SUITE=single-node'

# --- the lexer's own fallback -----------------------------------------------

# An apostrophe in running text defeats POSIX lexing, and running text reaches
# a command string constantly: a heredoc body, a commit message. These must go
# silent rather than falling through to a whole-string match, which would make
# the mention-matching defect reachable by ordinary English.
assert_silent 'apostrophe in a heredoc' \
	"$(printf 'cat > tmp/note.md <<%sEOF%s\ndon%st run make test-race here\nEOF' "'" "'" "'")"
assert_silent 'apostrophe in a message' \
	"git commit -m \"don't gate on make test-race\""
assert_silent 'unbalanced double quote' \
	'echo "make test-race'

# The second lexing pass has to earn its place, and silence cannot show that:
# the cases above go silent whether or not it runs, because a failed lex and a
# correct read both end in no opinion. This is the case that separates them --
# a real launch, chained ahead of a heredoc whose body holds an apostrophe. On
# POSIX lexing alone the whole string is unreadable and the launch escapes.
assert_denies 'launch ahead of a heredoc' \
	"$(printf 'make test-race > tmp/r.log 2>&1; cat > tmp/note.md <<%sEOF%s\ndon%st forget\nEOF' "'" "'" "'")"

# Likewise the redirect drop: it changes no verdict, only which command the
# chain branch names. Without it the deny reports the first segment with its
# redirection glued on, which is not a command anybody can act on.
redir_chain_reason="$(reason_for 'make check > tmp/a.log; make test-race')"
# shellcheck disable=SC2016  # the backticks are literal markdown in the deny
# text, so the needle must stay unexpanded -- expanding it here would assert
# against the output of `make check` rather than against the reason string.
[[ "${redir_chain_reason}" == *'wrap `make check`'* ]] ||
	fail 'a chain deny must name the first command without its redirection'

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
