#!/usr/bin/env python3
"""check-queue-rules.py — the backlog rules `queue.py lint` has no equivalent for.

`queue.py lint` is a pure function of a directory: frontmatter, rank shape,
filename/id agreement, the title cap, unresolvable targets. Three of this
repo's rules cannot be expressed that way, because each is a function of what
the *branch changed* rather than of what the store holds:

  8. A `flake` item may not simply vanish. A shipped mitigation parks it in
     flake watch and only a groom retires it to the ledger; deleting it throws
     away the memory that a fix was already attempted, so a second occurrence
     reads as a fresh find.
  9. Deleting the last item targeting a plan obliges that plan's index row to
     stop reading as open work.
 11. Every label an item wears is declared, so a typo cannot stick silently.

Rule 10 of the table-era linter is deliberately absent. It guarded a
*relocated* row outvoting a *deleted* one, which a line-position merge cannot
tell apart. One file per item makes those a modify and a delete of one path,
which git refuses rather than resolves, so the silent default is gone; the
measurement is in docs/plan/q889-backlog-item-store.md. Rule 12 is
`queue.py claims`.

The baseline is the merge base with origin/main, never its tip: an item `main`
deleted while this branch was behind is absent from the tip and present at the
base, so a tip-keyed check reads it as one this branch removed and demands the
ledger for finished work.

Exit: 0 all rules pass, 1 a rule failed, 2 a read that could not be taken.
"""

import os
import re
import subprocess
import sys
from pathlib import Path

STORE = "docs/queue"
LEDGER = "docs/development/flake-watch-retired.md"
PLAN_INDEX = "docs/plan/README.md"
# A plan row still advertising work. `✅` and `💤` and `ⓘ` do not.
OPEN_MARKERS = ("⚠️", "🚧", "❌", "🔲")
LABELS_LINE = re.compile(r"^\*\*Labels:\*\*(.*)$", re.M)
LABEL = re.compile(r"`([^`]+)`")
FRONTMATTER = re.compile(r"\A---\n(.*?)\n---\n", re.S)


class Unreadable(Exception):
    """A git or file read that did not happen. Never a verdict."""


def git(args):
    p = subprocess.run(["git"] + args, capture_output=True, text=True)
    return p.returncode, p.stdout, p.stderr


def merge_base():
    rc, out, err = git(["merge-base", "HEAD", "origin/main"])
    if rc != 0:
        raise Unreadable(f"the merge base with origin/main: {err.strip()}")
    return out.strip()


def items_at(ref):
    """{id: frontmatter text} for the store at a ref, or {} when it has none."""
    rc, out, _ = git(["ls-tree", "-r", "--name-only", ref, "--", STORE])
    if rc != 0:
        raise Unreadable(f"the store at {ref}")
    found = {}
    for path in out.splitlines():
        name = Path(path).name
        if not re.fullmatch(r"Q\d+\.md", name):
            continue
        rc, body, _ = git(["show", f"{ref}:{path}"])
        if rc != 0:
            raise Unreadable(f"{path} at {ref}")
        found[name[:-3]] = body
    return found


def labels_of(body):
    m = FRONTMATTER.search(body or "")
    if not m:
        return []
    out, inlist = [], False
    for line in m.group(1).splitlines():
        if re.match(r"^labels:\s*\[", line):
            return [s.strip().strip("'\"") for s in
                    line.split("[", 1)[1].rsplit("]", 1)[0].split(",") if s.strip()]
        if re.match(r"^labels:\s*$", line):
            inlist = True
            continue
        if inlist:
            m2 = re.match(r"^\s+-\s*(.+?)\s*$", line)
            if m2:
                out.append(m2.group(1).strip("'\""))
                continue
            inlist = False
    return out


def target_of(body):
    m = FRONTMATTER.search(body or "")
    if not m:
        return ""
    t = re.search(r"^target:\s*(.+?)\s*$", m.group(1), re.M)
    return t.group(1).strip("'\"") if t else ""


def declared_labels(root):
    """The vocabulary, from the store's own README. Absent is not empty."""
    readme = root / STORE / "README.md"
    if not readme.is_file():
        raise Unreadable(f"{STORE}/README.md, which declares the label vocabulary")
    m = LABELS_LINE.search(readme.read_text())
    if not m:
        raise Unreadable(f"a **Labels:** line in {STORE}/README.md")
    return set(LABEL.findall(m.group(1)))


def allow(var):
    return {s for s in os.environ.get(var, "").replace(",", " ").split() if s}


def rule8(base_items, head_items, ledger_text, failures):
    """A flake item that vanished must appear in the retired ledger."""
    excused = allow("QUEUE_ALLOW_FLAKE_DELETE")
    for qid, body in sorted(base_items.items()):
        if qid in head_items or qid in excused or "flake" not in labels_of(body):
            continue
        if re.search(rf"\b{re.escape(qid)}\b", ledger_text):
            continue
        failures.append(
            f"rule 8: {qid} carried `flake` and this branch deletes it, but it is "
            f"not in {LEDGER}. A shipped mitigation parks the item in flake "
            f"watch and only a groom retires it, so a recurrence reads as a "
            f"recurrence rather than a fresh find. Set "
            f"QUEUE_ALLOW_FLAKE_DELETE={qid} for a deliberate drop.")


def rule9(base_items, head_items, index_text, failures):
    """Deleting a plan's last item obliges its index row to stop reading open."""
    excused = allow("QUEUE_ALLOW_PROGRESS_STALE")
    live = {target_of(b) for b in head_items.values() if target_of(b)}
    for qid, body in sorted(base_items.items()):
        target = target_of(body)
        if qid in head_items or not target or target in live:
            continue
        name = Path(target).name
        if name in excused:
            continue
        for line in index_text.splitlines():
            if f"({name})" not in line and f"]({name}" not in line:
                continue
            if any(mark in line for mark in OPEN_MARKERS):
                failures.append(
                    f"rule 9: {qid} was the last item targeting {name}, and its "
                    f"{PLAN_INDEX} row still reads as open work. Flip it, or set "
                    f"QUEUE_ALLOW_PROGRESS_STALE={name} when work genuinely "
                    f"remains elsewhere.")
            break


def rule11(head_items, vocabulary, failures):
    """Every label worn is declared."""
    for qid, body in sorted(head_items.items()):
        for label in labels_of(body):
            if label not in vocabulary:
                failures.append(
                    f"rule 11: {qid} wears `{label}`, which is not on the "
                    f"**Labels:** line in {STORE}/README.md. Adding a label is a "
                    f"deliberate edit to that line, so a typo cannot stick.")


def main(argv=None):
    root = Path(subprocess.run(["git", "rev-parse", "--show-toplevel"],
                               capture_output=True, text=True).stdout.strip() or ".")
    try:
        head_items = items_at("HEAD")
        if not head_items:
            # Not a pass. The store does not exist yet, so there is nothing these
            # rules could have failed on, and saying "ok" would be a clean bill
            # of health for a store never read.
            print(f"check-queue-rules: no items under {STORE}; 0 checked "
                  f"(the store has not been created yet)")
            return 0
        base = merge_base()
        base_items = items_at(base)
        ledger = (root / LEDGER).read_text() if (root / LEDGER).is_file() else ""
        index = (root / PLAN_INDEX).read_text() if (root / PLAN_INDEX).is_file() else ""
        vocabulary = declared_labels(root)
    except Unreadable as e:
        print(f"check-queue-rules: could not read {e}; refusing to guess",
              file=sys.stderr)
        return 2

    failures = []
    rule8(base_items, head_items, ledger, failures)
    rule9(base_items, head_items, index, failures)
    rule11(head_items, vocabulary, failures)

    for f in failures:
        print(f)
    if failures:
        print(f"check-queue-rules: FAILED - {len(failures)} finding(s)")
        return 1
    print(f"check-queue-rules: ok ({len(head_items)} item(s), "
          f"{len(base_items)} at the merge base, rules 8, 9 and 11)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
