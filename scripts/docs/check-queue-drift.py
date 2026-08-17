#!/usr/bin/env python3
"""Fail when docs/queue/ and docs/STATUS.md have stopped agreeing.

Phase 2 of Q889 leaves the repo holding a backlog twice: the table is still
the source every consumer reads, and the store is what phase 3 will switch
them to. Nothing else notices when one is edited and the other is not, and
the failure is silent in the direction that matters -- a session grooming
the table leaves a store that still reads as authoritative.

The comparison is semantic rather than textual. It re-runs `queue.py
migrate` into a throwaway store and compares the loaded items, so it is the
tool's own reading of the table that gets compared, not a second parser that
could drift from it. Reflowing, hand-edited prose wrapping and the store's
README are all invisible to it, because `read_item` normalizes a body back
to one joined note before either side is compared.

Rank *values* are deliberately not compared, only the order they produce: a
re-rank inside the store is a legitimate edit that changes no priority, and
failing it would make the check fire on the one operation the store exists
to allow.

Exit 0 agreed (or nothing to compare), 1 drifted, 2 the comparison could not
be taken. Never 0 for a read that did not happen.
"""
import importlib.util
import pathlib
import shutil
import subprocess
import sys
import tempfile

QUEUE = "scripts/docs/queue.py"
TABLE = "docs/STATUS.md"
STORE = "docs/queue"

# Every field a table row carries into an item. `path` and `rank` are out:
# one is where the file happens to sit, the other is compared as order below.
FIELDS = ("id", "labels", "status", "size", "target", "title", "notes")


class Unreadable(Exception):
    """A read that did not happen. Never a verdict."""


def load_queue_module(root):
    # Loaded under a private name: the file is queue.py, and importing it as
    # `queue` would shadow the standard library module of that name.
    path = root / QUEUE
    if not path.exists():
        raise Unreadable(f"{QUEUE} is missing")
    spec = importlib.util.spec_from_file_location("gag_queue_lib", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def describe(item):
    return {f: getattr(item, f) for f in FIELDS}


def main():
    root = pathlib.Path(subprocess.run(
        ["git", "rev-parse", "--show-toplevel"],
        capture_output=True, text=True, check=True).stdout.strip())

    table, store = root / TABLE, root / STORE
    if not table.exists():
        print(f"check-queue-drift: {TABLE} is gone; the table and store can no "
              f"longer disagree, so this check has done its job and can be retired")
        return 0
    if not list(store.glob("Q*.md")):
        print(f"check-queue-drift: no items under {STORE}; nothing to compare "
              f"(the store has not been created yet)")
        return 0

    mod = load_queue_module(root)
    tmp = pathlib.Path(tempfile.mkdtemp(prefix="queue-drift-"))
    try:
        run = subprocess.run(
            [sys.executable, str(root / QUEUE), "--store", str(tmp),
             "migrate", str(table)],
            capture_output=True, text=True, cwd=root)
        if run.returncode != 0:
            raise Unreadable(
                f"migrating {TABLE} failed: {(run.stderr or run.stdout).strip()}")
        expected, _ = mod.load(tmp)
        actual, _ = mod.load(store)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    if not expected:
        raise Unreadable(f"migrating {TABLE} produced no items")

    exp_ids = [i.id for i in expected]
    act_ids = [i.id for i in actual]
    drift = []

    for qid in sorted(set(exp_ids) - set(act_ids)):
        drift.append(f"{qid} is in {TABLE} but not in {STORE}")
    for qid in sorted(set(act_ids) - set(exp_ids)):
        drift.append(f"{qid} is in {STORE} but not in {TABLE}")

    if not drift and exp_ids != act_ids:
        at = next(i for i, (a, b) in enumerate(zip(exp_ids, act_ids)) if a != b)
        drift.append(f"priority order differs at position {at + 1}: "
                     f"{TABLE} has {exp_ids[at]}, {STORE} has {act_ids[at]}")

    by_id = {i.id: i for i in actual}
    for item in expected:
        other = by_id.get(item.id)
        if other is None:
            continue
        for field, want in describe(item).items():
            got = describe(other)[field]
            if want != got:
                drift.append(f"{item.id}: {field} differs\n"
                             f"    {TABLE}: {want!r}\n"
                             f"    {STORE}: {got!r}")

    if drift:
        print(f"check-queue-drift: {TABLE} and {STORE} disagree", file=sys.stderr)
        for d in drift[:20]:
            print(f"  {d}", file=sys.stderr)
        if len(drift) > 20:
            print(f"  ... and {len(drift) - 20} more", file=sys.stderr)
        print(f"\n  Regenerate the store, or make the same edit on both sides:\n"
              f"    python3 {QUEUE} --store {STORE} migrate {TABLE}\n"
              f"  Until phase 3 switches the consumers, every backlog edit lands "
              f"twice.", file=sys.stderr)
        return 1

    print(f"check-queue-drift: ok ({len(act_ids)} item(s) agree with {TABLE}, "
          f"order and all)")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Unreadable as exc:
        print(f"check-queue-drift: {exc}; refusing to guess", file=sys.stderr)
        sys.exit(2)
