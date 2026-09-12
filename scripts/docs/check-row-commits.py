#!/usr/bin/env python3
"""Require a deleted backlog row to be recorded by a commit that names it (Q1100).

`queue.py metrics` attributes each removal to a closure verb, read off the
`docs(queue):`/`docs(status):` line that names the row. A `git rm` committed inline
with the work carries no such line, so the row falls to the residual however clearly
the pull request describes it, and the summary's `completed`/`pruned`/`retired`
figures under-count by whatever the residual holds.

The convention that fixes it is already written down: an isolated row commit is what
`session-backlog` asks for and what `CLAUDE.md` states. Nothing checked it.
`check-queue-rules.py` scores the store's *tree* at base against head, so commit shape
is outside every rule it has, and the repo had no commit-message gate of any kind.

**The rung is a branch-commit walk, because no tree check can see this.** Staging is
one tree; the defect is which commit the deletion lands in. So the gate resolves the
same merge-base `check-design-scope.sh` does and reads the commits above it.

**It asks the metric, rather than re-deriving what the metric asks.** The verb test is
`queue.py`'s own `_closure_verb`, imported from the vendored copy, so the gate and the
summary can never disagree about what counts: a row line naming *other* rows is a
sibling's and is declined for this one, an unnamed row line is the single-row case, and
the verb table is whatever upstream ships. A re-vendor that renames the function is a
refusal here (exit 2), never a silent pass.

Measured 2026-09-12 at `fb0ca1aa5`, after the Q959 re-vendor made the metric readable:
33 of 221 removals are unclassified. Twenty have no row line at all, and the rate there
has fallen off, the last one landing 2026-09-04. The other thirteen carry a row line
naming other rows, and **five of those are one pull request merged the same morning
this gate was written** (#1913, which deleted five rows inside their feature commits
while its only `docs(queue):` line filed a sixth). That is the case for the gate: the
shape recurs from an author who knows the rule, which is the shape prose cannot hold.

Usage:
    check-row-commits.py [--base REV]

Exit status: 0 clean, 1 when a row this branch deletes has no commit recording it,
2 when a base was required and could not be resolved, or the verb test could not be
imported.
"""

from __future__ import annotations

import argparse
import importlib.util
import os
import pathlib
import subprocess
import sys

QUEUE_PY = pathlib.Path("scripts/docs/queue.py")
ROW_PREFIX = "docs/queue/Q"


def git(*args: str) -> tuple[int, str]:
    p = subprocess.run(["git", *args], capture_output=True, text=True, check=False)
    return p.returncode, p.stdout


def load_verb_test():
    """Import `_closure_verb` and `ROW_COMMIT_RE` from the vendored queue.py."""
    if not QUEUE_PY.is_file():
        return None
    spec = importlib.util.spec_from_file_location("queue_py", QUEUE_PY)
    if spec is None or spec.loader is None:
        return None
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    except Exception:  # noqa: BLE001 - any import failure is a refusal, not a pass
        return None
    if not hasattr(mod, "_closure_verb") or not hasattr(mod, "ROW_COMMIT_RE"):
        return None
    return mod


def resolve_base(explicit: str | None) -> str | None:
    if explicit:
        return explicit
    rc, out = git("merge-base", "HEAD", "origin/main")
    return out.strip() if rc == 0 and out.strip() else None


def branch_commits(base: str) -> list[tuple[str, str]]:
    """Return (sha, full message) for every commit this branch adds over `base`."""
    rc, out = git("log", "--format=%x02%H%x01%B", f"{base}..HEAD")
    if rc != 0:
        return []
    commits = []
    for rec in out.split("\x02"):
        if not rec.strip():
            continue
        sha, _, msg = rec.partition("\x01")
        commits.append((sha.strip(), msg))
    return commits


def deleted_rows(base: str) -> list[str]:
    rc, out = git("diff", "--name-only", "--diff-filter=D", base, "HEAD", "--", "docs/queue/")
    if rc != 0:
        return []
    return [
        pathlib.Path(f).stem
        for f in out.splitlines()
        if f.startswith(ROW_PREFIX) and f.endswith(".md")
    ]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", help="revision to diff against, in place of the merge-base")
    args = ap.parse_args()

    rc, root = git("rev-parse", "--show-toplevel")
    if rc == 0 and root.strip():
        os.chdir(root.strip())

    mod = load_verb_test()
    if mod is None:
        print(
            f"check-row-commits: could not load the closure-verb test from {QUEUE_PY}; "
            "refusing rather than passing by checking nothing",
            file=sys.stderr,
        )
        return 2

    base = resolve_base(args.base)
    if base is None:
        msg = "check-row-commits: no merge-base with origin/main resolved"
        if os.environ.get("ROW_COMMITS_REQUIRE_BASE") == "1":
            print(f"{msg}, and a base was required", file=sys.stderr)
            return 2
        print(f"{msg}; skipping (shallow clone?)", file=sys.stderr)
        return 0

    rows = deleted_rows(base)
    if not rows:
        print("check-row-commits: ok (this branch deletes no backlog row)")
        return 0

    commits = branch_commits(base)
    unrecorded = []
    for row in sorted(set(rows)):
        if not any(mod._closure_verb(row, msg) for _, msg in commits):
            unrecorded.append(row)

    if not unrecorded:
        print(f"check-row-commits: ok ({len(set(rows))} deleted row(s), each recorded by a commit naming it)")
        return 0

    print(
        f"check-row-commits: {len(unrecorded)} deleted row(s) are recorded by no commit "
        f"on this branch: {', '.join(unrecorded)}",
        file=sys.stderr,
    )
    print(
        "A squash merge folds this branch into one message, and `queue.py metrics` reads "
        "the closure verb off the `docs(queue):`/`docs(status):` lines in it. A row "
        "deleted inside a feature commit carries no such line, so it lands in the "
        "residual however clearly the pull request describes it. Give each deleted row "
        "its own commit, `docs(queue): <verb> QNNN ...`, with a verb from the table in "
        "scripts/docs/queue.py. A row commit naming other rows does not count for this "
        "one: a groom that retires two rows must not spend its verb on whatever else "
        "the diff holds.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
