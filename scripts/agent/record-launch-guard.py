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

# Words that take a command as their argument, so the real command word is a
# later one. An allowlist rather than a heuristic, the same rule and mostly the
# same table as devtools/agent/gothrottle/decide.go: a name absent here stops
# the peel, because peeling a word that is not a wrapper would let an ordinary
# argument reach command position and be matched as a launch.
#
# `flags` names the options that take a separate value, so the value is stepped
# over with them. Any other `-` word is stepped over alone, which covers the
# attached forms (`-oL`, `-I{}`, `-lc`). `operands` is how many plain words the
# wrapper takes before its command; `assigns` allows `VAR=val` ahead of it;
# `shell` means the command arrives as one quoted argument rather than as the
# remaining argv, so it is re-lexed rather than stepped over.
#
# gothrottle omits the throttling wrappers deliberately, because its own
# alreadyThrottled answers those. That reason does not transfer: `nice -n 10
# make test-race` is a real launch and this hook has to see it.
WRAPPER_SPECS = {
    'timeout': {'flags': ('-s', '--signal', '-k', '--kill-after'),
                'operands': 1},
    'env': {'flags': ('-u', '--unset'), 'assigns': True},
    'stdbuf': {'flags': ('-i', '-o', '-e', '--input', '--output', '--error')},
    'nice': {'flags': ('-n', '--adjustment')},
    'xargs': {'flags': ('-I', '-i', '-n', '-P', '-d', '-E', '-s',
                        '--replace', '--max-args', '--max-procs')},
    'nohup': {},
    'command': {},
    'exec': {},
    'time': {},
    'bash': {'shell': True},
    'sh': {'shell': True},
    'zsh': {'shell': True},
    'dash': {'shell': True},
}

# How deep a `bash -c` inside a `bash -c` is followed. Two is already more
# nesting than anything in this repo writes. Past the bound the hook stops
# looking and stays silent rather than denying: it cannot tell whether a tier
# is in there, so a deny would fire on depth alone and name no fix, which is
# how a guard teaches override-by-reflex.
#
# The bound caps work, not a crash. Each level re-quotes the one inside it, so
# the string doubles per level past about six: depth 12 is 4,206 characters and
# Python's own recursion limit sits at a depth no command string reaches.
MAX_NESTING = 3

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


def peel(words):
    """Step over leading wrapper words, returning (remaining, nested scripts).

    Wrappers nest (`nohup timeout 600 make test-race`), so this loops. A shell
    wrapper ends the loop: its command is one quoted argument, handed back to
    be re-lexed rather than stepped over, because `bash -c 'make check; make
    test-race'` is a whole script inside a single token.

    Anchoring made this necessary. Before it, a whole-string search caught
    `timeout 600 make test-race` for the wrong reason; matching at command
    position is right and made the wrapper invisible, which is the expensive
    direction: the tier launches, no record is written, and nothing is said.
    """
    nested = []
    while words:
        spec = WRAPPER_SPECS.get(words[0])
        if spec is None:
            break
        words = words[1:]

        while words and words[0].startswith('-') and words[0] != '-':
            if words[0] in spec.get('flags', ()) and len(words) > 1:
                words = words[2:]
            else:
                words = words[1:]

        if spec.get('assigns'):
            while words and ASSIGNMENT.match(words[0]):
                words = words[1:]

        words = words[spec.get('operands', 0):]

        if spec.get('shell'):
            if words:
                nested.append(words[0])
            return [], nested

    return words, nested


def simple_commands(command, depth=0):
    """The simple commands in `command`, as (assignments, command) pairs.

    Leading `VAR=val` assignments and command-taking wrappers are stepped over,
    so the string starts at the word the shell would actually execute, and the
    assignments come back beside it because an override prefix belongs to the
    command it prefixes rather than to the whole string.

    Quoted arguments survive lexing as single tokens, which is what keeps a
    message or a search pattern that merely names a tier from ever landing at
    position 0.

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
        assignments = []
        while words and ASSIGNMENT.match(words[0]):
            assignments.append(words[0])
            words = words[1:]

        words, nested = peel(words)
        for script in nested:
            if depth < MAX_NESTING:
                out.extend(simple_commands(script, depth + 1) or [])

        if words:
            # Runs of whitespace collapse to one space, which is what makes the
            # registry's single-space patterns hold against the spellings bash
            # treats as identical: `make  test-race`, a tab, and an escaped
            # newline, which POSIX lexing leaves sitting inside the token after
            # it. The registry has the same gap and cannot be fixed from here,
            # since it is shared with foreground-guard (Q1123); this closes it
            # on the hook's own side.
            out.append((assignments,
                        WHITESPACE.sub(' ', ' '.join(words)).strip()))
    return out


def exempt(assignments, cmd):
    """Whether this one command is already handled, on its own terms.

    Per command, not per string. The exemption used to be `any segment is
    wrapped`, which let one wrapped member silence every other member:
    `record-launch.sh true; make test-race` went silent while the tier beside
    it launched with no handle. Same for the override, which belongs to the
    command it prefixes.
    """
    if cmd.split(' ', 1)[0].endswith(WRAPPER):
        return True
    return any(a.startswith(OVERRIDE + '=') for a in assignments)


def unwrapped_matches(candidates, patterns):
    """(pattern, [command, ...]) -- every un-exempt command matching any
    registered pattern, and the first pattern that matched one.

    All of them, not just the first: a fix that wraps one member of a chain
    leaves the others launching, so the caller has to know whether wrapping a
    single command would actually finish the job.
    """
    live = [cmd for assignments, cmd in candidates
            if not exempt(assignments, cmd)]

    exprs = []
    for pat in patterns:
        try:
            exprs.append((pat, re.compile(pat)))
        except re.error:
            continue

    hits, named = [], None
    for cmd in live:
        for pat, expr in exprs:
            if expr.match(cmd):
                hits.append(cmd)
                if named is None:
                    named = pat
                break
    return named, hits


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


def fix_clause(command, candidates, hits):
    """How to say what to do, which depends on whether a paste can be right.

    The front paste is right only when wrapping the first command finishes the
    job: one unwrapped tier, and it is that first command. The shell hands
    record-launch.sh only the first member, so `make test-race; echo done`
    wraps correctly, while `make check; make test-race` would wrap `make check`
    and leave the tier running, and `make test-race; make e2e` would wrap one
    tier and leave the other. All three used to take the paste at some point,
    and the last is the sharp one: a session that complied with the deny still
    launched a registered tier with no handle.
    """
    first = candidates[0][1] if candidates else None
    if len(hits) == 1 and hits[0] == first:
        return ('Fix: launch it through the wrapper, which writes the pid, the '
                'worktree and a verbatim stop command to tmp/launches/: `%s`, '
                'redirected to a log under tmp/' % paste(command))

    if len(hits) > 1:
        listed = ', '.join('`scripts/agent/%s %s`' % (WRAPPER, h) for h in hits)
        return ('Fix: %d registered tiers run here, so no single wrapper on '
                'the front covers them -- it would wrap `%s` and leave the '
                'rest launching with no handle. Wrap each one where it sits: '
                '%s, each redirected to its own log under tmp/'
                % (len(hits), first, listed))

    return ('Fix: the registered tier is not the first command here, so the '
            'wrapper cannot go on the front -- that would wrap `%s` and leave '
            '`%s` running unwrapped. Wrap the matching command where it sits, '
            'keeping the rest of the chain around it: '
            '`scripts/agent/%s %s`, redirected to a log under tmp/'
            % (first, hits[0], WRAPPER, hits[0]))


def reason(pattern, command, candidates, hits):
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
        % (LABEL, pattern, fix_clause(command, candidates, hits), WRAPPER, DOC,
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

    candidates = simple_commands(command)
    if candidates is None:
        silent()

    # Both exemptions are applied per command inside unwrapped_matches, not to
    # the string as a whole: one wrapped or overridden member must not cover a
    # real launch standing beside it.
    project_dir = os.environ.get('CLAUDE_PROJECT_DIR') or os.getcwd()
    pattern, hits = unwrapped_matches(candidates, slow_patterns(project_dir))
    if pattern is None:
        silent()

    json.dump({'hookSpecificOutput': {
        'hookEventName': 'PreToolUse',
        'permissionDecision': 'deny',
        'permissionDecisionReason': reason(pattern, command, candidates, hits),
    }}, sys.stdout)
    sys.stdout.write('\n')
    sys.exit(0)


if __name__ == '__main__':
    main()
