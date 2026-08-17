#!/usr/bin/env python3
"""pr-mergeability-watch.py — wake the orchestrator when a handed-off PR stops
being mergeable.

A worker's own watcher exits at `ready`, so a PR handed back for review sits
open with nothing watching it. That is exactly the window a sibling merge turns
it DIRTY, and no CI event fires when it happens.

Deliberately narrower than a full PR watcher rather than a second copy of one:

- It reads `state`, `mergeStateStatus` and `baseRefName` **only**. Never the PR
  body, review comments or issue comments, so no text a third party can write
  reaches the session that acts on the exit. `baseRefName` is a branch in the
  target repository rather than authored text, and it is refused unless it
  matches a conservative refname pattern.
- It carries no CI output. Check failures stay with the worker that owns the
  PR, so an orchestrator watching a whole batch does not accumulate logs for
  work it is not fixing.
- It sleeps between polls, which relaunching a `ready`-terminated watcher
  cannot: that re-evaluates at once, reports ready again, and spins.

The base is read because a stacked PR rebased onto the trunk absorbs its own
base into its diff. The wake names the base branch and never a `git rebase
--onto` line: that needs the old base head, which `merge-base` cannot recover
once the base has been force-pushed, so emitting one would hand over a command
that is wrong exactly when it matters.

`UNKNOWN` is not a conflict. GitHub computes mergeability asynchronously and
reports UNKNOWN while it does, so treating it as DIRTY would wake the
orchestrator for every freshly pushed PR.

The budget counts time this process spent sleeping, not wall clock. One clock
means a stubbed sleeper advances the accounting deterministically, so the
timeout is assertable without a second timebase to disagree with it.

Exit: 0 having printed exactly one event — conflict, closed, timeout or error.
      2 on a usage error.
"""

import argparse
import json
import re
import subprocess
import sys
import time

MAX_CONSECUTIVE_FAILURES = 5
FIELDS = ("state", "mergeStateStatus", "baseRefName")
# A refname git would accept, and nothing else. An unreadable base drops to the
# branchless wording rather than being interpolated into the wake.
REFNAME = re.compile(r"^[A-Za-z0-9._][A-Za-z0-9._/-]*$")


class GhError(Exception):
    """A `gh` invocation that failed, carrying what it printed."""


def gh_runner(pr, repo=None):
    """Read the three fields from GitHub. Raises GhError on a failed call.

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


def read_fields(payload):
    """(state, merge_state, base) from a gh payload, base blank if unusable."""
    state = payload.get("state") or ""
    merge_state = payload.get("mergeStateStatus") or ""
    base = payload.get("baseRefName") or ""
    if not REFNAME.match(base):
        base = ""
    return state, merge_state, base


def conflict_detail(merge_state, base, trunk, gate):
    if not base:
        return (f"mergeStateStatus is {merge_state}. Wake the owning worker to "
                f"rebase onto the branch this PR targets, which this watch "
                f"could not read, re-run {gate}, and force-push with lease. "
                f"State the condition that invalidates the instruction, since "
                f"delivery timing is not bounded.")
    if base == trunk:
        return (f"mergeStateStatus is {merge_state}. Wake the owning worker to "
                f"rebase onto origin/{trunk}, re-run {gate}, and force-push "
                f"with lease. State the condition that invalidates the "
                f"instruction, since delivery timing is not bounded.")
    return (f"mergeStateStatus is {merge_state}. The PR is stacked: it targets "
            f"{base}, not {trunk}, so rebasing onto {trunk} would absorb its "
            f"base. Wake the owning worker to rebase onto origin/{base}, or "
            f"onto origin/{trunk} if {base} has already merged by the time the "
            f"wake is read, then re-run {gate} and force-push with lease.")


def watch(pr, runner, sleeper, interval=60, budget=21600, trunk="main",
          gate="the repo's gate"):
    """Poll until something is worth waking for. Returns (event, detail)."""
    slept = 0
    failures = 0
    while True:
        try:
            state, merge_state, base = read_fields(runner())
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
            return "conflict", conflict_detail(merge_state, base, trunk, gate)

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
    )
    # The orchestrator reads this as the background task's output, so it names
    # the next action rather than a status.
    print(f"event: {event}")
    print(f"pr: #{args.pr}")
    print(detail)
    return 0


if __name__ == "__main__":
    sys.exit(main())
