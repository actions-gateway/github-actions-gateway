#!/usr/bin/env python3
"""pr-mergeability-watch.py — wake the orchestrator when a handed-off PR stops
being mergeable.

A worker's own watcher exits at `ready`, so a PR handed back for review sits
open with nothing watching it. That is exactly the window a sibling merge turns
it DIRTY, and no CI event fires when it happens.

Deliberately narrower than a full PR watcher rather than a second copy of one:

- It reads `state`, `mergeStateStatus`, `baseRefName` and `headRefOid`
  **only**. Never the PR body, review comments or issue comments, so no text a
  third party can write reaches the session that acts on the exit. What admits
  a field is whether its value can carry authored characters, not how few
  fields there are: `baseRefName` is a branch in the target repository, and
  `headRefOid` is an object name git computed from content, so an author
  chooses which commit and never which characters. Each is refused unless it
  matches its own pattern — a conservative refname for the base, forty hex
  digits for the head, which is the narrower of the two. The one further read,
  `gh pr list --head <base>`, fires once on a retarget and admits two machine
  values under the same patterns.
- It carries no CI output. Check failures stay with the worker that owns the
  PR, so an orchestrator watching a whole batch does not accumulate logs for
  work it is not fixing.
- It sleeps between polls, which relaunching a `ready`-terminated watcher
  cannot: that re-evaluates at once, reports ready again, and spins.

The head is read because a worker's own push moves nothing else. The PR stays
mergeable, so no other field changes, and the orchestrator's only notice is a
handback message — a channel that can arrive late or not at all. Every
verification pinned to the old head is void meanwhile. The wake prints both
object names in full because `gh run list --commit` matches the whole name and
returns an empty list at exit 0 for a prefix, which reads as a commit nothing
has run on.

The base is read **and armed**, because a stack's shape is not in any single
reading of it. A PR that targets its parent's branch reverts to the trunk the
moment that parent merges, since GitHub retargets a PR whose base branch is
deleted — so the base a conflict reports is `main` for a stacked child and for
an ordinary one alike, and the difference is only visible against the base the
watch armed on. Measured 2026-09-04 on `karlkfi/claude-bouncer`: three stacked
children all read `baseRefName` `main` at conflict time and all three carry
`automatic_base_change_succeeded`, so all three were retargeted rather than
trunk-based.

That distinction decides the remedy, and getting it wrong is destructive.
Where the parent squash-merged — the only strategy available in a repository
with `mergeCommitAllowed: false` — its work is on the trunk as one new object
that no descendant matches, so `git rebase origin/main` on the child replays
commits the trunk already carries. The `--onto` form is the repair, it needs
the parent's pre-merge head, and that head survives the branch deletion at
`refs/pull/<n>/head`: verified 2026-09-04 on six merged-and-deleted branches
across two repositories, including three sixteen days old. So the wake emits
the `--onto` line rather than refusing to, and names the fork point outright
where `gh pr list --head <old base> --state merged` recovers it.

Every conflict wake carries the reading that permits its command, because a
`--onto` from a wrong base replays nothing, reports no error, and leaves a
branch that looks rebased. A remedy that decays silently has to be checkable by
the reader rather than merely expiring.

`UNKNOWN` is not a conflict. GitHub computes mergeability asynchronously and
reports UNKNOWN while it does, so treating it as DIRTY would wake the
orchestrator for every freshly pushed PR.

The budget counts time this process spent sleeping, not wall clock. One clock
means a stubbed sleeper advances the accounting deterministically, so the
timeout is assertable without a second timebase to disagree with it.

It takes a PR number and pins to whatever head it reads at its first poll, so
there is no --head to assert one with. An unrecognised flag is a usage error
rather than an ignored argument, because a watch launched in the background
prints nothing where the caller is looking and an ignored flag would read as
armed while the head moved unwatched.

Exit: 0 having printed exactly one event — conflict, head_change, closed,
      timeout or error.
      2 on a usage error.
"""

import argparse
import json
import re
import subprocess
import sys
import time

MAX_CONSECUTIVE_FAILURES = 5
FIELDS = ("state", "mergeStateStatus", "baseRefName", "headRefOid")
# A refname git would accept, and nothing else. An unreadable base drops to the
# branchless wording rather than being interpolated into the wake.
REFNAME = re.compile(r"^[A-Za-z0-9._][A-Za-z0-9._/-]*$")
# A whole object name. A head that is not one is a reading not taken, so it
# arms nothing and fires nothing rather than being compared or quoted.
OID = re.compile(r"^[0-9a-f]{40}$")


class GhError(Exception):
    """A `gh` invocation that failed, carrying what it printed."""


def gh_runner(pr, repo=None):
    """Read the fields from GitHub. Raises GhError on a failed call.

    Asks for `--json` and parses here rather than delegating to `gh --jq`. A
    jq expression inside a gh invocation is invisible to a test that stubs gh,
    so a malformed one fails in production instead of in the suite; and gh's
    `--jq` prints nothing for a JSON null where `jq -r` prints "null", which is
    a discrepancy this avoids having to reproduce at all.
    """
    cmd = ["gh", "pr", "view", str(pr), "--json", ",".join(FIELDS)]
    if repo:
        cmd += ["--repo", repo]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise GhError((proc.stderr or proc.stdout).strip())
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError as e:
        raise GhError(f"unparseable JSON from gh: {e}") from e


def gh_parent(branch, repo=None):
    """(number, head) of the merged PR whose head was `branch`, or (0, "").

    Called once, on a retarget, so the wake can name a fork point instead of
    handing over a template. Both values are machine-made and each is refused
    unless it matches the pattern the corresponding field already has. Any
    failure degrades to (0, ""): the wake then names the command that recovers
    the pair, which is the same answer one step later.
    """
    cmd = ["gh", "pr", "list", "--head", branch, "--state", "merged",
           "--limit", "1", "--json", "number,headRefOid"]
    if repo:
        cmd += ["--repo", repo]
    try:
        proc = subprocess.run(cmd, capture_output=True, text=True)
        if proc.returncode != 0:
            return 0, ""
        rows = json.loads(proc.stdout)
    except (OSError, json.JSONDecodeError):
        return 0, ""
    if not isinstance(rows, list) or not rows:
        return 0, ""
    number = rows[0].get("number")
    head = rows[0].get("headRefOid") or ""
    if not isinstance(number, int) or number <= 0 or not OID.match(head):
        return 0, ""
    return number, head


def read_fields(payload):
    """(state, merge_state, base, head), base or head blank if unusable."""
    state = payload.get("state") or ""
    merge_state = payload.get("mergeStateStatus") or ""
    base = payload.get("baseRefName") or ""
    if not REFNAME.match(base):
        base = ""
    head = payload.get("headRefOid") or ""
    if not OID.match(head):
        head = ""
    return state, merge_state, base, head


def permit(trunk, gate):
    """The tail every conflict wake carries.

    Two jobs, and the wake is wrong without either. It states the reading that
    permits the command, because a `--onto` from a wrong base is silent — it
    replays nothing, reports no error, and leaves a branch that looks rebased.
    And it states the condition that voids the instruction, rather than telling
    the relay to think of one: the rebase target resolves when the command
    runs, so anything that moves it in between is the condition.
    """
    return (f"Before pushing, confirm `git log --oneline origin/{trunk}..HEAD` "
            f"lists only this item's own commits and `git diff --stat "
            f"origin/{trunk}...HEAD` only its files. Then re-run {gate} and "
            f"force-push with lease — any verification taken before the rebase "
            f"is void. The instruction itself is void if {trunk} moves again "
            f"before the push, or if the branch has been rebased since this "
            f"was sent: the rebase target is resolved when the command runs, "
            f"not when it was written.")


def onto_repair(trunk, base, number=0, head=""):
    """The `--onto` clause, complete wherever the fork point was recovered.

    Handing over a template is what the case behind this rule went wrong on: a
    `--onto` from a wrong base replays nothing and reports no error, so a blank
    left in the command gets filled with something plausible and the branch
    ends up looking rebased. Name the commit where it can be named.
    """
    if number and head:
        return (f"rebase with `git rebase --onto origin/{trunk} {head}` — "
                f"#{number}'s head from before it merged, which "
                f"`refs/pull/{number}/head` still resolves to now that branch "
                f"is deleted")
    where = (f"`gh pr list --head {base} --state merged --json "
             f"number,headRefOid`" if base else "that parent PR's `headRefOid`")
    return (f"rebase with `git rebase --onto origin/{trunk} <the parent's "
            f"pre-merge head>` instead, taking that head from {where}; "
            f"`refs/pull/<number>/head` still resolves to it after the branch "
            f"is deleted")


def conflict_detail(merge_state, base, armed_base, trunk, gate, parent):
    """What to tell the worker, which depends on why the PR went dirty."""
    tail = permit(trunk, gate)
    ordinary = (f"Where it lists only this item's own commits, an ordinary "
                f"rebase onto origin/{trunk} is right.")
    discriminate = (f"run `git fetch origin && git log --oneline "
                    f"origin/{trunk}..HEAD` on the branch first")

    if not base:
        return (f"mergeStateStatus is {merge_state}. Wake the owning worker to "
                f"rebase onto the branch this PR targets, which this watch "
                f"could not read — take it from `gh pr view` rather than "
                f"assuming {trunk}. If that turns out to be {trunk}, "
                f"{discriminate}: {ordinary} Where it lists a parent item's, "
                f"{onto_repair(trunk, None)}. {tail}")

    if base != trunk:
        return (f"mergeStateStatus is {merge_state}. The PR is stacked: it "
                f"targets {base}, not {trunk}, so rebasing onto {trunk} would "
                f"absorb its base. Wake the owning worker to rebase onto "
                f"origin/{base}. If origin/{base} is gone by the time the wake "
                f"is read, its PR has merged and GitHub has retargeted this "
                f"one — do not substitute origin/{trunk}, which replays the "
                f"parent's commits, but {onto_repair(trunk, base)}. {tail}")

    if armed_base and armed_base != base:
        number, head = parent(armed_base)
        return (f"mergeStateStatus is {merge_state}. This PR's base moved from "
                f"{armed_base} to {trunk} while the watch was armed, which is "
                f"what GitHub does when {armed_base} is deleted — so read it "
                f"as the parent having merged. A squash puts the parent's work "
                f"on {trunk} as one object no descendant matches, so `git "
                f"rebase origin/{trunk}` replays commits {trunk} already "
                f"carries: the destructive move rather than the repair. Wake "
                f"the owning worker to {discriminate}. {ordinary} Where it "
                f"lists the parent's, "
                f"{onto_repair(trunk, armed_base, number, head)}. {tail}")

    return (f"mergeStateStatus is {merge_state}. This watch cannot tell a "
            f"branch stacked on an already-merged parent from an ordinary one "
            f"that has fallen behind, so wake the owning worker to "
            f"{discriminate}. {ordinary} Where it lists another item's, the "
            f"parent has squash-merged and the ordinary rebase replays what "
            f"{trunk} already carries — {onto_repair(trunk, None)}. {tail}")


def head_detail(old, new):
    return (f"The head moved from {old} to {new}. Whatever was verified against "
            f"{old} is void, and nothing else signalled the move — the PR "
            f"stayed mergeable throughout. Re-verify against {new}, cite that "
            f"head rather than the one checked earlier, and arm a fresh watch.")


def watch(pr, runner, sleeper, interval=60, budget=21600, trunk="main",
          gate="the repo's gate", parent=None):
    """Poll until something is worth waking for. Returns (event, detail)."""
    parent = parent or (lambda branch: (0, ""))
    slept = 0
    failures = 0
    armed_head = ""
    armed_base = ""
    while True:
        try:
            state, merge_state, base, head = read_fields(runner())
        except GhError as e:
            failures += 1
            if failures >= MAX_CONSECUTIVE_FAILURES:
                return "error", (f"gh failed {failures} times in a row; "
                                 f"last output: {e}")
            sleeper(interval)
            slept += interval
            continue
        failures = 0

        if state != "OPEN":
            return "closed", (f"The PR is {state}. Nothing left to watch; drop "
                              f"it from the tracker.")

        if merge_state in ("DIRTY", "BEHIND"):
            return "conflict", conflict_detail(merge_state, base, armed_base,
                                               trunk, gate, parent)

        # Conflict outranks a head move because the rebase it asks for moves
        # the head anyway, so that signal regenerates; a head move on a
        # mergeable PR is the only signal there is. The arming head is the
        # first one that could be read, so an unreadable head arms nothing,
        # and a later unreadable one is a missing reading rather than a move.
        # The base arms on the same terms and fires nothing of its own: a
        # retarget is only ever read alongside a conflict, and on a PR that
        # stays mergeable it changes no advice.
        if not armed_head:
            armed_head = head
        elif head and head != armed_head:
            return "head_change", head_detail(armed_head, head)
        if not armed_base:
            armed_base = base

        if slept >= budget:
            return "timeout", (f"Budget of {budget}s of polling elapsed with "
                               f"the PR still OPEN and {merge_state}. Relaunch "
                               f"if it is still awaiting merge.")

        sleeper(interval)
        slept += interval


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("pr", type=int, help="pull request number")
    p.add_argument("--repo", help="OWNER/NAME (default: whatever gh resolves)")
    p.add_argument("--interval", type=int, default=60, help="seconds between polls")
    p.add_argument("--timeout", type=int, default=21600, help="polling budget, seconds")
    p.add_argument("--trunk", default="main", help="the branch a normal PR targets")
    p.add_argument("--gate", default="the repo's gate",
                   help="what the wake tells the worker to re-run")
    args = p.parse_args(argv)
    if args.pr <= 0:
        p.error("pr must be a positive integer")

    event, detail = watch(
        args.pr,
        runner=lambda: gh_runner(args.pr, args.repo),
        sleeper=time.sleep,
        interval=args.interval,
        budget=args.timeout,
        trunk=args.trunk,
        gate=args.gate,
        parent=lambda branch: gh_parent(branch, args.repo),
    )
    # The orchestrator reads this as the background task's output, so it names
    # the next action rather than a status.
    print(f"event: {event}")
    print(f"pr: #{args.pr}")
    print(detail)
    return 0


if __name__ == "__main__":
    sys.exit(main())
