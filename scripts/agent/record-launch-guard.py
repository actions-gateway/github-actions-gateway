#!/usr/bin/env python3
r"""record-launch-guard.py — Claude Code PreToolUse hook that makes the launch
record fire, so a heavy background run cannot be started without a stop handle
on disk (Q739).

Q709 shipped record-launch.sh and the doc line telling sessions to use it;
nothing reached for it, so the handle stayed optional and a run launched
without it is back to a kill by pattern, the shape that killed a sibling
worktree's `make check` mid-run in Q690. A prose instruction is the rung that
already failed here, and CI cannot see the event at all: the launch happens in
a dev session, not in a workflow. A PreToolUse hook is the only rung that
observes the moment of launch.

Scope is exactly the gap foreground-guard leaves. That guard owns the
*foreground* slow command and teaches `run_in_background: true` as the fix; a
backgrounded one it exempts outright, so the launch this hook is about passes
it silently. Measured 2026-09-16 against foreground-guard 0.7.0 by driving its
hook with CLAUDE_PROJECT_DIR set: `make test-race` foreground denies and names
`run_in_background: true` without the repo `hint` clause, the same command
backgrounded emits nothing, and a poll form carries the hint. So the `hint`
field reaches Class A only and is not a seam for this.

The verdict is deny rather than ask because the fix is a command that can be
written into the reason: the model applies it in its own loop and nobody is
interrupted. An ask would spend a human on a rewrite the model can make.

The registry is .claude/foreground-guard.json's slow.commands, read rather than
duplicated: one list, and its patterns are already anchored against the
mention-only shapes (`git show`, `grep`, `ls` naming a registered script) with
both directions asserted by foreground-guard-patterns-test.sh. A second list
here would drift from the one those assertions cover. Python for the same
reason that test gives: the patterns are Python `re`, and bash ERE disagrees
with it about `\s`, `\b` and `\S`.

Only the numeric form ({regex: ms}) is read. The target-aware form
({command: {glob: ms}}) needs foreground-guard's own argument matching to
decide, so this hook stays silent on one rather than guessing; the suite pins
that the live config carries none, which is what turns the gap loud the day
somebody adds one.

Fail-open everywhere: an unreadable config, an unparseable payload, a missing
key all exit 0 with no output, which Claude Code reads as no opinion. A hook
must never block a tool call it could not reason about.

Wired up by .claude/settings.json as a PreToolUse hook on the Bash matcher.
"""

import json
import os
import re
import sys

CONFIG = os.path.join('.claude', 'foreground-guard.json')
WRAPPER = 'record-launch.sh'
OVERRIDE = 'RECORD_LAUNCH_GUARD_OVERRIDE'

# The opener every emit carries. A deny leaves no decision record, so the
# reason string is the only evidence this hook ran and the name sits at
# position 0.
# For a repo-local hook the label is the script's own stem.
LABEL = 'record-launch-guard'

DOC = 'docs/development/testing.md#the-launch-record'


def silent():
    """Exit with no opinion. Every path that could not decide lands here."""
    sys.exit(0)


def slow_patterns(project_dir):
    """The numeric-valued slow-command regexes, or [] if none can be read."""
    try:
        with open(os.path.join(project_dir, CONFIG), encoding='utf-8') as fh:
            data = json.load(fh)
    except (OSError, ValueError):
        return []
    if not isinstance(data, dict):
        return []
    slow = data.get('slow')
    if not isinstance(slow, dict) or slow.get('enabled') is False:
        return []
    cmds = slow.get('commands')
    if not isinstance(cmds, dict):
        return []
    return [pat for pat, ms in cmds.items()
            if isinstance(pat, str)
            and isinstance(ms, (int, float)) and not isinstance(ms, bool)]


def first_match(command, patterns):
    """The first registered pattern the command matches, or None.

    `search`, not `match`, because that is what foreground-guard does to these
    same patterns — anchoring is the registry's job and is asserted there.
    An uncompilable pattern is skipped rather than fatal: a typo in the config
    must not take the guard down with it.
    """
    for pat in patterns:
        try:
            if re.compile(pat).search(command):
                return pat
        except re.error:
            continue
    return None


def reason(pattern, command):
    """The deny text: the rewrite first, the override last.

    Leading with the escape hatch is what teaches a session to reach for it
    first, so it goes after the fix it is a fallback for.
    """
    return (
        '%s: this backgrounded run matches the slow-command pattern `%s`, and '
        'it carries no stop handle. A compaction drops the launching task id, '
        'leaving nothing to aim at but a kill by pattern, which reaches every '
        "worktree's copy of the same command. Fix: launch it through the "
        'wrapper, which writes the pid, the worktree and a verbatim stop '
        'command to tmp/launches/: `scripts/agent/%s %s`, redirected to a log '
        'under tmp/ (the wrapper propagates the run\'s exit status, so a '
        '`; rc=$?; echo "EXIT=$rc"; exit $rc` tail still reports the run). '
        'Read the records back with `scripts/agent/%s --list`. See %s. '
        'If this run genuinely must not be wrapped (the pr-sentinel watcher '
        'is the one such case, and it matches no registered pattern), re-run '
        'with a %s=<reason> prefix.'
        % (LABEL, pattern, WRAPPER, command, WRAPPER, DOC, OVERRIDE))


def main():
    try:
        data = json.load(sys.stdin)
    except (ValueError, OSError):
        silent()
    if not isinstance(data, dict) or data.get('tool_name') != 'Bash':
        silent()

    tool_input = data.get('tool_input')
    if not isinstance(tool_input, dict):
        silent()

    # Foreground is foreground-guard's Class B, which teaches backgrounding as
    # its fix. This hook owns only what that fix lands on.
    if tool_input.get('run_in_background') is not True:
        silent()

    command = tool_input.get('command')
    if not isinstance(command, str) or not command.strip():
        silent()

    # Already wrapped, or deliberately exempted.
    if WRAPPER in command or OVERRIDE + '=' in command:
        silent()

    project_dir = os.environ.get('CLAUDE_PROJECT_DIR') or os.getcwd()
    pattern = first_match(command, slow_patterns(project_dir))
    if pattern is None:
        silent()

    json.dump({'hookSpecificOutput': {
        'hookEventName': 'PreToolUse',
        'permissionDecision': 'deny',
        'permissionDecisionReason': reason(pattern, command),
    }}, sys.stdout)
    sys.stdout.write('\n')
    sys.exit(0)


if __name__ == '__main__':
    main()
