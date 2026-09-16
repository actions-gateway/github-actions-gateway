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
duplicated: one list, so a tier added there is covered here the same day. A
second list would drift from the one foreground-guard-patterns-test.sh asserts
over. Python for the same reason that test gives: the patterns are Python `re`,
and bash ERE disagrees with it about `\s`, `\b` and `\S`.

Anchoring is this hook's own job, not the registry's. Five of the seven live
patterns carry no command anchor -- only the two `scripts/dogfood/*.sh` ones do
-- so a `search` over the raw command string denies a backgrounded
`git grep -n 'make e2e' docs/`, a `grep -rn 'go test -race' docs/`, or a
`git commit -m` whose message names a tier. That is this repo's documented
defect class: a pattern naming a command also matches text that merely mentions
it. It is fixed here rather than in the registry because the registry is shared
-- anchoring it would move foreground-guard's matching too, which is a separate
change against a plugin this repo does not own.

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
import shlex
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

# A token that ends one simple command and begins another. shlex returns these
# as their own tokens only when they are unquoted, which is what keeps a
# `git commit -m "...; make test-race"` from splitting inside its own message.
COMMAND_SEPARATOR = re.compile(r'^(?:[;&|]+|[(){}])$')

# A token carrying a redirection. What follows it in the same segment is a
# filename rather than a command, so it is dropped: without this a plain
# `make test-race > tmp/race.log 2>&1` reads as three segments, and the paste
# below would take it for a chain.
REDIRECT = re.compile(r'[<>]')

# A trailing `&`. record-launch.sh backgrounds the run itself and propagates
# its exit status, so echoing one back into the paste double-backgrounds the
# wrapper and throws away the status the reason promises the caller.
TRAILING_BACKGROUND = re.compile(r'\s*&\s*$')

# The override, as an assignment at the head of some command rather than as a
# mention anywhere in the string. `OVERRIDE in command` would let a real launch
# through for quoting the variable's name in an echo, which is the same
# mention-matching defect the anchoring above exists to fix, pointed the other
# way: that one refuses what it should allow, this one allows what it should
# refuse.
OVERRIDE_PREFIX = re.compile(
    r'(?:^|[;&|(]\s*)(?:\w+=\S+\s+)*' + re.escape(OVERRIDE) + r'=')

# Words that take a command as their argument, so the real command word is the
# next one. The registry's own dogfood patterns already step over the first
# four; `env` and `time` are here because they run their argv directly too.
WRAPPERS = frozenset(('bash', 'sh', 'exec', 'nohup', 'env', 'time'))

ASSIGNMENT = re.compile(r'^\w+=')

# Any run of whitespace, including the newline an escaped line break leaves
# behind inside a token.
WHITESPACE = re.compile(r'\s+')

# A leading run of `VAR=val` words, hoisted ahead of the wrapper in the paste.
LEADING_ASSIGNMENTS = re.compile(r'^((?:\w+=\S+\s+)+)(.*)$', re.S)


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


def lex(command):
    """Tokens for `command`, or None when neither mode can read it.

    POSIX mode first: it strips quotes, so a quoted argument can never look
    like a command word. It also rejects an apostrophe in running text, and
    `don't` reaches a command string every time somebody writes a heredoc body
    or a commit message -- so non-POSIX mode is tried next. That keeps the
    quote characters but still groups a quoted run into a single token, which
    is all the anchoring below needs.

    Without the second pass the fallback is reachable by ordinary English
    rather than only by malformed input, which is how `git commit -m "don't
    gate on make test-race"` comes back a deny.
    """
    for posix in (True, False):
        lexer = shlex.shlex(command, posix=posix, punctuation_chars=True)
        lexer.whitespace_split = True
        try:
            return list(lexer)
        except ValueError:
            continue
    return None


def simple_commands(command):
    """Each simple command in `command`, space-joined, from its command word on.

    Leading `VAR=val` assignments and command-taking wrappers are stepped over,
    so the string starts at the word the shell would actually execute. Quoted
    arguments survive lexing as single tokens, which is what keeps a message or
    a search pattern that merely names a tier from ever landing at position 0.

    None when the command cannot be lexed at all. The caller treats that as no
    opinion, per the module's fail-open rule: a string that defeats both lexing
    modes is not a command bash would run either, so silence here gives up no
    real launch.
    """
    tokens = lex(command)
    if tokens is None:
        return None

    groups, current, redirected = [], [], False
    for token in tokens:
        if COMMAND_SEPARATOR.match(token):
            groups.append(current)
            current, redirected = [], False
        elif REDIRECT.search(token):
            redirected = True
        elif not redirected:
            current.append(token)
    groups.append(current)

    out = []
    for words in groups:
        while words and (ASSIGNMENT.match(words[0]) or words[0] in WRAPPERS):
            words = words[1:]
        if words:
            # Runs of whitespace collapse to one space, which is what makes the
            # registry's single-space patterns hold against the spellings bash
            # treats as identical: `make  test-race`, a tab, and an escaped
            # newline, which POSIX lexing leaves sitting inside the token after
            # it. The registry has the same gap and cannot be fixed from here,
            # since it is shared with foreground-guard (Q1123); this closes it
            # on the hook's own side.
            out.append(WHITESPACE.sub(' ', ' '.join(words)).strip())
    return out


def first_match(candidates, patterns):
    """The first registered pattern one of `candidates` matches, or None.

    `match` against each simple command rather than `search` over the whole
    string, for the reason in the module docstring: most of the live patterns
    have no anchor of their own, and a search makes every mention of a tier a
    deny.

    An uncompilable pattern is skipped rather than fatal: a typo in the config
    must not take the guard down with it.
    """
    for pat in patterns:
        try:
            expr = re.compile(pat)
        except re.error:
            continue
        if any(expr.match(candidate) for candidate in candidates):
            return pat
    return None


def paste(command):
    """The wrapper invocation to hand back, ready to run as written.

    A trailing `&` is dropped and any leading `VAR=val` run is hoisted ahead of
    the wrapper. record-launch.sh runs its argv directly (`"$@" &`), so an
    assignment left after the wrapper name is executed as a program: measured
    2026-09-16, `record-launch.sh FOO=1 echo hello` exits 127 with
    `FOO=1: command not found`, while `FOO=1 record-launch.sh env` exits 0 with
    FOO in the child's environment. Both dogfood patterns match an assignment
    prefix explicitly, so the deny fires on that shape and the paste has to
    survive it.
    """
    command = TRAILING_BACKGROUND.sub('', command)
    hoisted = LEADING_ASSIGNMENTS.match(command)
    if hoisted:
        return '%sscripts/agent/%s %s' % (
            hoisted.group(1), WRAPPER, hoisted.group(2))
    return 'scripts/agent/%s %s' % (WRAPPER, command)


def fix_clause(command, pattern):
    """How to say what to do, which depends on whether a paste can be right.

    Only a single simple command can be rewritten mechanically. Pasting the
    wrapper onto the front of a chain wraps its *first* member: for
    `make check; make test-race` that yields
    `record-launch.sh make check; make test-race`, which wraps the wrong
    command and leaves the registered tier running unwrapped. A session that
    ran it would have complied with the deny and defeated the hook, so a chain
    gets an instruction naming the segment instead of a rewrite.
    """
    candidates = simple_commands(command) or []
    if len(candidates) <= 1:
        return ('Fix: launch it through the wrapper, which writes the pid, the '
                'worktree and a verbatim stop command to tmp/launches/: `%s`, '
                'redirected to a log under tmp/' % paste(command))

    matched = next(
        (c for c in candidates if re.compile(pattern).match(c)), candidates[-1])
    return ('Fix: this runs several commands, so the wrapper cannot go on the '
            'front of it -- that would wrap `%s` and leave the registered '
            'tier running unwrapped. Wrap the matching command where it sits, '
            'keeping the rest of the chain around it: '
            '`scripts/agent/%s %s`, redirected to a log under tmp/'
            % (candidates[0], WRAPPER, matched))


def reason(pattern, command):
    """The deny text: the rewrite first, the override last.

    Leading with the escape hatch is what teaches a session to reach for it
    first, so it goes after the fix it is a fallback for.
    """
    return (
        '%s: this backgrounded run matches the slow-command pattern `%s`, and '
        'it carries no stop handle. A compaction drops the launching task id, '
        'leaving nothing to aim at but a kill by pattern, which reaches every '
        "worktree's copy of the same command. %s "
        '(the wrapper propagates the run\'s exit status, so a '
        '`; rc=$?; echo "EXIT=$rc"; exit $rc` tail still reports the run). '
        'Read the records back with `scripts/agent/%s --list`. See %s. '
        'If this run genuinely must not be wrapped (the pr-sentinel watcher '
        'is the one such case, and it matches no registered pattern), re-run '
        'with a %s=<reason> prefix.'
        % (LABEL, pattern, fix_clause(command, pattern), WRAPPER, DOC,
           OVERRIDE))


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

    # Deliberately exempted. Anchored, so quoting the variable's name in an
    # echo beside a real launch does not buy the launch an exemption.
    if OVERRIDE_PREFIX.search(command):
        silent()

    candidates = simple_commands(command)
    if candidates is None:
        silent()

    # Already wrapped. The wrapper has to be some command's own first word: a
    # `git show` of the script, or an echo naming it, is a mention rather than
    # a use, and treating it as one let a launch beside it through.
    if any(c.split(' ', 1)[0].endswith(WRAPPER) for c in candidates):
        silent()

    project_dir = os.environ.get('CLAUDE_PROJECT_DIR') or os.getcwd()
    pattern = first_match(candidates, slow_patterns(project_dir))
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
