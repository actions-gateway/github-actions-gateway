#!/usr/bin/env python3
"""queue.py — read, check, order and migrate a per-item backlog store.

One file per item under `docs/queue/`, priority held in each item's `rank`
key rather than in its position in a table. Items never share a file, so two
sessions editing different items cannot conflict, whatever the merge algorithm
and with no merge driver installed.

Subcommands:
  render    the ordered backlog — the read path, one call for the whole queue
  next      the top ready item, as a session kickoff prompt
            (skipping any item an open pull request already claims)
  lint      check the store (frontmatter, ids, ranks, references)
  claims    check every id this branch adds holds a claim on the remote
  metrics   replay git history into flow metrics
  migrate   convert a legacy `docs/STATUS.md` Queue/Deferred table into items
  rank      compute an order key for an insertion

Ranks are base-36 order keys compared as plain strings, using the same
magnitude-head scheme as github-actions-gateway's queuestore, so a store
written by either tool reads and extends under the other.
"""

import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

DIGITS = "0123456789abcdefghijklmnopqrstuvwxyz"
STATUSES = ("ready", "blocked", "deferred")
ID_RE = re.compile(r"^Q\d+$")

# A reference is a link at an item's file, not a bare id in prose. The two are
# different claims: a link becomes a live href in a rendered index and dangles
# once the item ships, while "the Q2 audit" is a sentence about history that
# stays true forever. Matching both made the store noisier with every item it
# cleared, which is backwards for a store that is supposed to drain.
ITEM_LINK_RE = re.compile(r"\[[^\]]*\]\((?:\./)?(Q\d+)\.md(?:#[^)]*)?\)")
# What a blocked row waits on is what it opens with — `Blocked by [Q3](Q3.md)`,
# or the same sentence in prose where the blocker is not an item. Anchored on
# purpose: an id quoted anywhere in the note is an example, and a check an
# unrelated example satisfies cannot fail when it should. `[\s*]` rather than
# `\s` because the trigger check below marks its opener in bold.
BLOCKER_RE = re.compile(r"[\s*]*Blocked[\s*]+(?:by|on)[\s*]+\S")
# Backticks are the store's escape for exhibiting syntax rather than using it,
# so a quoted id or link is neither a reference nor a blocker. The site build
# honours the same escape.
CODE_SPAN_RE = re.compile(r"`[^`]*`")

# The bottom of the space: head 'A' takes 26 digits after it. It is reserved
# rather than usable — fractional room sits above an integer and never below
# one, so a key occupying the lowest integer would be one nothing could be
# inserted below.
SMALLEST_INTEGER = "A" + DIGITS[0] * 26


# --- rank algebra ---------------------------------------------------------
#
# A rank is an order key: a magnitude head, an integer part whose length the
# head fixes, and an optional fraction. Plain string comparison orders two
# ranks, so placing an item names a string between its neighbours and writes
# only that item's own file.
#
# The head is what keeps keys short. Midpointing alone never runs out but
# degrades exactly where this process pushes hardest: inserting below the
# smallest key prepends a digit every few insertions, and flakes-first sends
# every new flake to the top. A head lets the integer part step whole
# magnitudes instead — "a0" to "Zz" to "Zy" — so head and tail insertion cost
# no length at all until a magnitude is exhausted. Heads 'a'..'z' carry integer
# lengths 2..27 upward, 'Z'..'A' the same downward, and uppercase sorting below
# lowercase is what puts the descending magnitudes underneath.
#
# Ported from the Go implementation in karlkfi/github-actions-gateway's
# devtools/docs/queuestore so a store written by either reads and extends
# under the other.

def integer_length(head):
    if "a" <= head <= "z":
        return ord(head) - ord("a") + 2
    if "A" <= head <= "Z":
        return ord("Z") - ord(head) + 2
    raise ValueError(f"rank head {head!r} is not a magnitude character")


def integer_part(rank):
    n = integer_length(rank[0])
    if n > len(rank):
        raise ValueError(
            f"rank {rank!r} is shorter than the {n} characters its head requires")
    return rank[:n]


def check_rank(rank):
    """Raise ValueError unless rank is a well-formed order key."""
    if not rank:
        raise ValueError("rank is empty")
    if rank == SMALLEST_INTEGER:
        raise ValueError(f"rank {rank!r} is the reserved bottom of the space")
    frac = rank[len(integer_part(rank)):]
    if frac.strip(DIGITS):
        raise ValueError(
            f"rank {rank!r} holds a character outside base-36 after its integer part")
    # "x0" and "x" denote the same value, and midpointing toward a trailing
    # zero would not terminate.
    if frac and frac[-1] == DIGITS[0]:
        raise ValueError(
            f"rank {rank!r} ends in {DIGITS[0]!r}, which denotes the same value "
            f"as the rank without it")


def increment_integer(x):
    head, digs = x[0], list(x[1:])
    carry = True
    for i in range(len(digs) - 1, -1, -1):
        if not carry:
            break
        d = DIGITS.index(digs[i]) + 1
        if d == len(DIGITS):
            digs[i] = DIGITS[0]
            continue
        digs[i] = DIGITS[d]
        carry = False
    if not carry:
        return head + "".join(digs)
    if head == "Z":
        return "a" + DIGITS[0]
    if head == "z":
        raise ValueError(f"rank {x!r} is at the top of the space")
    nxt = chr(ord(head) + 1)
    if nxt > "a":
        digs.append(DIGITS[0])
    else:
        digs = digs[:-1]
    return nxt + "".join(digs)


def decrement_integer(x):
    head, digs = x[0], list(x[1:])
    borrow = True
    for i in range(len(digs) - 1, -1, -1):
        if not borrow:
            break
        d = DIGITS.index(digs[i]) - 1
        if d == -1:
            digs[i] = DIGITS[-1]
            continue
        digs[i] = DIGITS[d]
        borrow = False
    if not borrow:
        return head + "".join(digs)
    if head == "a":
        return "Z" + DIGITS[-1]
    if head == "A":
        raise ValueError(f"rank {x!r} is at the bottom of the space")
    prev = chr(ord(head) - 1)
    if prev < "Z":
        digs.append(DIGITS[-1])
    else:
        digs = digs[:-1]
    return prev + "".join(digs)


def _digit_at(s, i):
    """s[i], or the lowest digit once s has ended — what an unwritten
    fractional digit denotes."""
    return s[i] if i < len(s) else DIGITS[0]


def midpoint(lo, hi):
    """A fraction strictly between lo and hi, an empty hi meaning the top."""
    if hi:
        # Descend through the shared prefix: it constrains nothing, and
        # dropping it keeps the result minimal-length.
        n = 0
        while n < len(hi) and _digit_at(lo, n) == hi[n]:
            n += 1
        if n > 0:
            return hi[:n] + midpoint(lo[n:], hi[n:])
    lead = DIGITS.index(lo[0]) if lo else 0
    limit = DIGITS.index(hi[0]) if hi else len(DIGITS)
    # A gap in the leading digit is the common case, and ends it in one digit.
    if limit - lead > 1:
        return DIGITS[(lead + limit) // 2]
    # Leading digits are adjacent. Where hi has more to say, its own leading
    # digit already sits above lo and below hi.
    if len(hi) > 1:
        return hi[:1]
    # hi is a single digit or absent, so the room is below it: keep lo's
    # leading digit and place the rest above lo's tail.
    return DIGITS[lead] + midpoint(lo[1:], "")


def rank_between(lo, hi):
    """A rank strictly between lo and hi. None or "" means open-ended."""
    lo, hi = lo or "", hi or ""
    for name, r in (("lo", lo), ("hi", hi)):
        if r:
            try:
                check_rank(r)
            except ValueError as e:
                raise ValueError(f"{name}: {e}") from e
    if lo and hi and lo >= hi:
        raise ValueError(f"rank {lo!r} is not below {hi!r}")

    if not lo:
        if not hi:
            return "a" + DIGITS[0]
        ih = integer_part(hi)
        if ih == SMALLEST_INTEGER:
            return ih + midpoint("", hi[len(ih):])
        # Where hi carries a fraction its integer part already sits below it,
        # which costs no length.
        if ih < hi:
            return ih
        below = decrement_integer(ih)
        if below == SMALLEST_INTEGER:
            # The bottom magnitude is reserved, so the room left is fractional.
            return below + midpoint("", "")
        return below

    il = integer_part(lo)
    fl = lo[len(il):]

    if not hi:
        try:
            return increment_integer(il)
        except ValueError:
            # The top magnitude is exhausted, so the room left is fractional.
            return il + midpoint(fl, "")

    ih = integer_part(hi)
    if il == ih:
        return il + midpoint(fl, hi[len(ih):])
    nxt = increment_integer(il)
    if nxt < hi:
        return nxt
    return il + midpoint(fl, "")


def rank_series(count):
    """Successive keys for a bulk import, in order."""
    out = []
    cur = "a" + DIGITS[0]
    for _ in range(count):
        out.append(cur)
        cur = increment_integer(integer_part(cur))
    return out


# --- the store ------------------------------------------------------------

class Item:
    __slots__ = ("id", "rank", "labels", "status", "size", "target",
                 "title", "notes", "path")

    def __init__(self, **kw):
        for k in self.__slots__:
            setattr(self, k, kw.get(k))

    def sort_key(self):
        # Ties break by numeric id so two sessions that never saw each other
        # cannot produce an order that depends on which side merged first.
        return (self.rank or "", int(self.id[1:]) if ID_RE.match(self.id or "") else 0)


def _parse_frontmatter(text, path):
    """Minimal YAML reader for the shapes this store writes. Returns (dict, body)."""
    problems = []
    if not text.startswith("---\n"):
        return None, "", [f"{path}: no frontmatter (line 1 is not '---')"]
    end = text.find("\n---\n", 3)
    if end == -1:
        return None, "", [f"{path}: frontmatter not closed with '---'"]
    head, body = text[4:end + 1], text[end + 5:]
    data, key = {}, None
    for raw in head.split("\n"):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        if raw.startswith((" ", "\t")) and raw.lstrip().startswith("- "):
            if key is None:
                problems.append(f"{path}: list item before any key")
                continue
            data.setdefault(key, [])
            if not isinstance(data[key], list):
                data[key] = []
            data[key].append(raw.lstrip()[2:].strip())
            continue
        if ":" not in raw:
            problems.append(f"{path}: frontmatter line is not 'key: value': {raw!r}")
            continue
        key, _, val = raw.partition(":")
        key, val = key.strip(), val.strip()
        if val.startswith("[") and val.endswith("]"):
            data[key] = [v.strip() for v in val[1:-1].split(",") if v.strip()]
        elif val == "":
            data[key] = []
        else:
            data[key] = val
    return data, body, problems


def read_item(path):
    text = path.read_text(encoding="utf-8")
    data, body, problems = _parse_frontmatter(text, path.name)
    if data is None:
        return None, problems
    title, notes = "", ""
    for line in body.split("\n"):
        if line.startswith("# ") and not title:
            title = line[2:].strip()
        elif title and line.strip():
            notes += (" " if notes else "") + line.strip()
    labels = data.get("labels") or []
    if isinstance(labels, str):
        labels = [labels]
    item = Item(id=data.get("id"), rank=data.get("rank"), labels=labels,
                status=data.get("status"), size=data.get("size"),
                target=data.get("target") or None, title=title,
                notes=notes.strip(), path=path)
    return item, problems


def store_dir(root=None):
    root = Path(root) if root else Path(
        subprocess.run(["git", "rev-parse", "--show-toplevel"],
                       capture_output=True, text=True, check=True).stdout.strip())
    return root / "docs" / "queue"


def load(store):
    items, problems = [], []
    for path in sorted(Path(store).glob("Q*.md")):
        item, probs = read_item(path)
        problems.extend(probs)
        if item:
            items.append(item)
    items.sort(key=Item.sort_key)
    return items, problems


def write_item(store, item):
    lines = [f"id: {item.id}", f"rank: {item.rank}"]
    if item.labels:
        lines.append("labels:")
        lines += [f"    - {label}" for label in item.labels]
    lines.append(f"status: {item.status}")
    if item.size:
        lines.append(f"size: {item.size}")
    if item.target:
        lines.append(f"target: {item.target}")
    body = f"# {item.title}\n"
    if item.notes:
        body += f"\n{item.notes}\n"
    path = Path(store) / f"{item.id}.md"
    path.write_text("---\n" + "\n".join(lines) + "\n---\n\n" + body, encoding="utf-8")
    return path


# --- subcommands ----------------------------------------------------------

# An index cell is not the item. The store deliberately retired the Notes
# length cap so an item can hold its full context, and a table that renders
# that in full is squeezed by its own longest row: a browser sizes columns by
# content, so one long cell claims the width and every other column wraps into
# a ribbon. The full text is one click away on the item's own page.
NOTES_IN_TABLE = 140

# The title is capped where the note is not, because their homes differ. A note
# has a page of its own where length costs nothing, and the index summarizes it.
# A title has no such page: it renders whole in every index row, in `next`'s
# kickoff prompt, and in any session named after the item. 72 is the
# conventional commit-message wrap, doing the same job — one line that has to
# survive a list.
TITLE_MAX = 72


def summarize(notes, limit=NOTES_IN_TABLE):
    """The first sentence, or a clean truncation, whichever comes first."""
    notes = " ".join((notes or "").split())
    if len(notes) <= limit:
        return notes
    stop = notes.find(". ")
    if 0 < stop <= limit:
        return notes[:stop + 1]
    cut = notes.rfind(" ", 0, limit)
    return notes[:cut if cut > 0 else limit].rstrip(",;:") + " …"


def cmd_render(args):
    items, problems = load(args.store or store_dir())
    for p in problems:
        print(f"queue: {p}", file=sys.stderr)
    shown = [i for i in items if args.all or i.status != "deferred"]
    if args.format == "table":
        print("| ID | Item | Labels | St | Sz | Notes |")
        print("|---|---|---|---|---|---|")
        mark = {"ready": "🔲", "blocked": "🚫", "deferred": "💤"}
        for i in shown:
            title = f"[{i.title}]({i.target})" if i.target else i.title
            labels = " ".join(f"`{label}`" for label in i.labels)
            notes = summarize(i.notes).replace("|", r"\|")
            # The id links to the item's own page: this table is what a reader
            # meets first, and the page is where the full text lives.
            print(f"| [{i.id}]({i.id}.md) | {title} | {labels} "
                  f"| {mark.get(i.status, '?')} | {i.size or ''} | {notes} |")
    else:
        for i in shown:
            labels = ",".join(i.labels)
            print(f"{i.status:<8} {i.id:<6} {i.size or '-':<2} {i.title}"
                  + (f"   [{labels}]" if labels else ""))
    return 1 if problems else 0


def cmd_next(args):
    store = Path(args.store or store_dir())
    items, _ = load(store)
    ready = [i for i in items if i.status == "ready"]
    if not ready:
        print("queue: no ready item", file=sys.stderr)
        return 1

    # --title and the full prompt are two processes in the invocation the docs
    # teach, so both run the check: a title that skipped it would name the
    # session after one item while the prompt handed out another.
    allow = set(args.allow or [])
    checked = False
    if args.no_pr_check:
        top = ready[0]
        print(f"queue: next: --no-pr-check, so nothing asked whether {top.id} "
              f"is already claimed", file=sys.stderr)
    else:
        prs, why = _gh_open_prs(store)
        # Loud rather than a fallthrough. An unverified pick is the state this
        # check exists to end, and once it is in the prompt it reads exactly
        # like a verified one.
        if prs is None:
            print(f"queue: next: cannot ask GitHub what is already claimed: {why}",
                  file=sys.stderr)
            print("queue: next: refusing to hand out an unverified pick; "
                  "fix gh, or pass --no-pr-check to take it anyway",
                  file=sys.stderr)
            return 1
        for cand in ready:
            if cand.id in allow:
                top = cand
                break
            naming = _prs_naming(cand.id, prs)
            if not naming:
                top, checked = cand, True
                break
            for pr in naming:
                print(f"queue: next: skipping {cand.id}: #{pr['number']} is "
                      f"open and names it — {pr['title']} ({pr['url']})",
                      file=sys.stderr)
            # A hit is a candidate rather than a verdict: an id is cited by
            # neighbouring rows and by the retro that filed it, so a PR can
            # name one it is not implementing. Reading the PR closes a hit,
            # and --allow is how a reader says it did.
            print(f"queue: next: a PR that merely cites {cand.id} has not "
                  f"claimed it — read those, then --allow {cand.id} to take it "
                  f"anyway", file=sys.stderr)
        else:
            print("queue: next: every ready item has an open PR naming it; "
                  "read them, then --allow the one that is really free",
                  file=sys.stderr)
            return 1

    if args.title:
        print(f"{top.id}: {top.title}")
        return 0
    pick = ("no open PR named it when this prompt was printed, so verify any "
            "blockers" if checked else
            "check for an open PR first, verify any blockers")
    print(f"{top.id}: {top.title} — take this item from the top of the "
          f"backlog and work it per the repo process: {pick}, do the work, "
          f"then delete docs/queue/{top.id}.md in the PR that completes it."
          + (f" Notes: {top.notes}" if top.notes else "")
          + (f" See: {top.target}" if top.target else ""))
    return 0


def cmd_lint(args):
    store = Path(args.store or store_dir())
    items, problems = load(store)
    seen_id, seen_rank = {}, {}
    ids = {i.id for i in items}
    for i in items:
        where = i.path.name
        if not i.id or not ID_RE.match(i.id):
            problems.append(f"{where}: id {i.id!r} is not QNNN")
        elif i.path.stem != i.id:
            problems.append(f"{where}: filename does not match id {i.id}")
        elif i.id in seen_id:
            problems.append(f"{where}: duplicate id, also in {seen_id[i.id]}")
        else:
            seen_id[i.id] = where
        try:
            check_rank(i.rank or "")
        except ValueError as e:
            problems.append(f"{where}: {e}")
        else:
            if i.rank in seen_rank:
                # Legal — ties break by id — but worth surfacing as drift.
                print(f"queue: note: {where} shares rank {i.rank} with "
                      f"{seen_rank[i.rank]}; order falls back to id",
                      file=sys.stderr)
            else:
                seen_rank[i.rank] = where
        if i.status not in STATUSES:
            problems.append(f"{where}: status {i.status!r} not one of {STATUSES}")
        if not i.title:
            problems.append(f"{where}: no title (body has no '# ' heading)")
        elif len(i.title) > TITLE_MAX:
            problems.append(
                f"{where}: title is {len(i.title)} characters (max {TITLE_MAX}); "
                f"move the detail into the body, which has no cap")
        prose = CODE_SPAN_RE.sub("", i.notes or "")
        for ref in ITEM_LINK_RE.findall(prose):
            if ref not in ids and ref != i.id:
                print(f"queue: note: {where} links {ref}.md, which is not in "
                      f"the store (shipped, or a typo); the href dangles",
                      file=sys.stderr)
        # An item can legitimately be blocked on something that is not another
        # item — a release landing, an upstream fix, a SHA that does not exist
        # yet — so the script asks only that the note open by saying what it
        # waits on, and leaves whether the condition is real to a reader.
        if i.status == "blocked" and not BLOCKER_RE.match(prose):
            print(f"queue: note: {where} is blocked but does not open with what "
                  f"it waits on; start the note `Blocked by …` or `Blocked on …`",
                  file=sys.stderr)
        if i.target:
            resolved = (i.path.parent / i.target.split("#")[0]).resolve()
            if not resolved.exists():
                problems.append(f"{where}: target does not resolve: {i.target}")
        # A parked item is a standing query against the world that nothing
        # re-runs, so one with no stated trigger can never come back by a check.
        # A note rather than an error: whether the prose names a real condition
        # is a reader's call, and the table linter does not fail on it either.
        if i.status == "deferred" and not re.search(
                r"\*\*(Demand|Event|Decision)", i.notes or ""):
            print(f"queue: note: {where} is deferred but names no trigger; "
                  f"say what would revive it", file=sys.stderr)
        # A `file.ext:N` pointer rots silently as the code moves. Warn rather
        # than fail, matching the table linter: a bare filename is genuinely
        # ambiguous about which directory it was written against.
        # The lookbehind stands in for `\b`, which never holds before a
        # leading `.`: under `\b` a `.github/` path matched from `github/`
        # and resolved from neither base.
        for ref in re.findall(
                r"(?<![\w./-])([\w./-]+"
                r"\.(?:go|py|sh|md|ya?ml|json|ts|js|rs|java)):(\d+)\b",
                i.notes or ""):
            path, line = ref
            if any((base / path).exists() for base in (store.parent, store.parent.parent)):
                continue
            print(f"queue: note: {where} cites {path}:{line}, which does not "
                  f"resolve from {store.parent}; re-point or drop it",
                  file=sys.stderr)
    for p in problems:
        print(f"queue: {p}", file=sys.stderr)
    if problems:
        return 1
    # An empty store is legal — every item may have shipped — so this is a note
    # rather than a failure. It is worth saying because the usual cause is a
    # --store pointed somewhere with no items in it, a table directory being the
    # likely one, and that reads as a clean pass on a store never loaded.
    if not items:
        print(f"queue: note: no Q*.md under {store}; either the backlog is "
              f"empty or --store is pointed at the wrong directory",
              file=sys.stderr)
    print(f"queue: {len(items)} item(s) OK")
    return 0


def _git(args, cwd):
    return subprocess.run(["git"] + args, cwd=cwd, capture_output=True,
                          text=True, check=True).stdout


def cmd_metrics(args):
    # Both sides get resolved before the relative_to: git reports the real
    # path, and on macOS the usual temp roots (/tmp, /var/folders) are symlinks,
    # so an unresolved store path is not relative to the root git names.
    store = Path(args.store or store_dir()).resolve()
    root = Path(_git(["rev-parse", "--show-toplevel"], store).strip()).resolve()
    rel = store.relative_to(root)
    log = _git(["log", "--diff-filter=AD", "--name-status", "--date=short",
                "--pretty=format:C\t%H\t%ad\t%s", "--", str(rel)], root)
    filed, closed, reason = {}, {}, {}
    date, subject = None, ""
    for line in log.split("\n"):
        if line.startswith("C\t"):
            _, _, date, subject = line.split("\t", 3)
            continue
        if not line.strip():
            continue
        status, _, path = line.partition("\t")
        item = Path(path).stem
        if not ID_RE.match(item):
            continue
        if status.startswith("A"):
            filed[item] = date               # log is newest-first; last wins
        elif status.startswith("D"):
            if item not in closed:
                closed[item] = date
                verb = re.search(r"\b(complete|prune|merge|defer)\w*\b",
                                 subject, re.I)
                reason[item] = verb.group(1).lower() if verb else "removed"
    if args.events:
        print("id\tfiled\tclosed\tdays\treason")
        for item in sorted(filed, key=lambda q: int(q[1:])):
            c = closed.get(item, "")
            days = _days(filed[item], c) if c else ""
            print(f"{item}\t{filed[item]}\t{c}\t{days}\t{reason.get(item, 'open')}")
        return 0
    done = [q for q in closed if reason.get(q) == "complete"]
    pruned = [q for q in closed if reason.get(q) in ("prune", "merge")]
    spans = sorted(_days(filed[q], closed[q]) for q in closed if q in filed)
    open_now = [q for q in filed if q not in closed]
    print(f"filed        {len(filed)}")
    print(f"completed    {len(done)}")
    print(f"pruned       {len(pruned)}")
    print(f"open         {len(open_now)}")
    if spans:
        print(f"cycle time   median {spans[len(spans) // 2]}d  "
              f"mean {sum(spans) // len(spans)}d")
    if closed:
        print(f"prune ratio  {100 * len(pruned) // len(closed)}%")
    return 0


def _days(a, b):
    from datetime import date
    ya, ma, da = (int(x) for x in a.split("-"))
    yb, mb, db = (int(x) for x in b.split("-"))
    return (date(yb, mb, db) - date(ya, ma, da)).days


# --- claims ---------------------------------------------------------------
#
# `alloc-queue-id.sh` reserves an ID by creating `refs/queue-ids/QN` on the
# remote, which binds only the sessions that call it. This is the other half:
# every ID a branch *adds* must hold a claim, so a number someone read off the
# store and incremented fails at the gate that files the row rather than at the
# rebase that collides with it.
#
# Three properties it is built around.
#
#   1. NEW IS MEASURED AGAINST THE MERGE BASE, NEVER origin/main's TIP. A row
#      `main` deleted while this branch was behind is absent from the tip and
#      present at the base, so against the tip it reads as one this branch
#      filed — and the rule would then demand a claim for finished work.
#   2. AN UNREADABLE REMOTE SKIPS RATHER THAN FAILS, so an offline clone still
#      runs the gate. `--strict` turns every skip into a failure, and CI — which
#      always has a network — is where it is passed. Without that, the one place
#      the check is guaranteed to run is also a place it can silently not run.
#   3. IT IS ITS OWN SUBCOMMAND. `lint` is a pure function of a directory: no
#      git, no network, correct against any store, which is what makes it safe
#      in an edit loop and in tests over temp dirs. This check is a function of
#      the *branch* instead, so folding it in would put a merge base and a
#      network round trip behind every one of those calls.

REF_NS = "refs/queue-ids"

# git gets a deadline because the failure here is a hang rather than an error:
# an ssh remote that is not there sits in connect() for minutes, and an https
# one stops to ask for credentials — which GIT_TERMINAL_PROMPT=0 refuses.
REMOTE_TIMEOUT = 20


def _git_read(args, cwd, timeout=None):
    """(stdout, ok) — never raises, because every read here is one to skip on."""
    try:
        p = subprocess.run(["git"] + args, cwd=cwd, capture_output=True,
                           text=True, timeout=timeout,
                           env={**os.environ, "GIT_TERMINAL_PROMPT": "0"})
    except (OSError, subprocess.SubprocessError):
        return "", False
    return p.stdout, p.returncode == 0


def _ids_at(rev, rel, root):
    out, ok = _git_read(["ls-tree", "-r", "--name-only", rev, "--", rel], root)
    if not ok:
        return None
    return {p for p in (Path(x).stem for x in out.split("\n")) if ID_RE.match(p)}


# Whether an item is already being worked is a fact about GitHub rather than
# about the store, so `next` reads it and `lint` never does: the gates that run
# lint have no network, and a checker reaching for one fails on the wrong axis.
#
# The whole open list rather than one `gh pr list --search Q<n>` per candidate.
# `--search` answers from GitHub's search index rather than from the pull
# request records, so its freshness is a property of that index; the list
# endpoint reads what the PR page reads, and the PR this check most needs to
# catch is the one opened minutes ago. It is also one call covering every
# candidate rather than one per candidate.
GH_PR_LIMIT = 200


def _gh_timeout():
    """The deadline for the gh call, overridable through QUEUE_GH_TIMEOUT.

    A slow link should be able to wait longer rather than reach for
    --no-pr-check, which does not relax the check but turns it off. A value
    that is not a positive integer falls back to the default rather than
    failing, because a mistyped deadline is not a reason to refuse the pick.
    """
    try:
        seconds = int(os.environ.get("QUEUE_GH_TIMEOUT") or 0)
    except ValueError:
        return REMOTE_TIMEOUT
    return seconds if seconds > 0 else REMOTE_TIMEOUT


def _gh_open_prs(cwd, timeout=None):
    """(prs, why) — every open pull request, or (None, reason) if none was read.

    None is not an empty list. A read that never happened has to stay
    distinguishable from a remote that answered "nothing", because the caller
    hands out work on the strength of the answer.
    """
    timeout = timeout or _gh_timeout()
    try:
        p = subprocess.run(
            ["gh", "pr", "list", "--state", "open",
             "--limit", str(GH_PR_LIMIT), "--json", "number,title,url,body"],
            cwd=cwd, capture_output=True, text=True, timeout=timeout,
            env={**os.environ, "GH_PAGER": "cat", "GH_PROMPT_DISABLED": "1"})
    except FileNotFoundError:
        return None, "gh is not on PATH"
    except subprocess.TimeoutExpired:
        return None, f"gh did not answer within {timeout}s"
    except (OSError, subprocess.SubprocessError) as e:
        return None, f"gh could not be run: {e}"
    if p.returncode != 0:
        return None, (p.stderr.strip().splitlines() or ["gh exited "
                      f"{p.returncode} and said nothing"])[0]
    try:
        prs = json.loads(p.stdout or "[]")
    except ValueError:
        return None, f"gh printed no readable json: {p.stdout.strip()[:80]!r}"
    # A full page is a truncated read, and a truncated read that answered
    # "nothing claims this" is the silent pass this check exists to remove.
    if len(prs) >= GH_PR_LIMIT:
        return None, (f"{len(prs)} open PRs came back, which is the whole page "
                      f"— the rest went unread, so a claim could be among them")
    return prs, ""


def _prs_naming(qid, prs):
    """The PRs whose title or body carries `qid` as a whole token.

    Whole token so Q23 does not match Q231. The id is matched anywhere in
    either field, which is what a claiming PR does and also what a neighbouring
    row's citation does, so a hit is a candidate rather than a verdict.
    """
    at = re.compile(rf"\b{re.escape(qid)}\b")
    return [p for p in prs
            if at.search(p.get("title") or "") or at.search(p.get("body") or "")]


def cmd_claims(args):
    def skip(why):
        print(f"queue: claims: {why}", file=sys.stderr)
        if args.strict:
            print("queue: claims: --strict was passed, so that is a failure",
                  file=sys.stderr)
            return 1
        return 0

    store = Path(args.store or store_dir()).resolve()
    out, ok = _git_read(["rev-parse", "--show-toplevel"], store)
    if not ok:
        return skip(f"{store} is not in a git repository, so there is no branch "
                    f"to measure against")
    root = Path(out.strip()).resolve()
    try:
        rel = str(store.relative_to(root))
    except ValueError:
        return skip(f"{store} is outside {root}; point --store inside the repo")

    base, ok = _git_read(["merge-base", args.base, "HEAD"], root)
    if not ok:
        return skip(f"no merge base between {args.base} and HEAD — fetch it, or "
                    f"deepen a shallow clone, or pass --base")
    base = base.strip()
    before = _ids_at(base, rel, root)
    if before is None:
        return skip(f"cannot read {rel} at {base[:12]}")
    # The working tree rather than HEAD: the gate runs over a row that has been
    # written and not yet committed, which is when a hand-picked ID is cheapest
    # to fix — and it is the same reason the Makefile's file lists carry
    # --others.
    now = {p.stem for p in store.glob("Q*.md") if ID_RE.match(p.stem)}
    added = sorted(now - before, key=lambda q: int(q[1:]))
    if not added:
        print(f"queue: claims: no ids added since {base[:12]}")
        return 0

    out, ok = _git_read(["ls-remote", args.remote, f"{REF_NS}/*"], root,
                        timeout=REMOTE_TIMEOUT)
    if not ok:
        return skip(f"{args.remote} did not answer, so its claims are unknown "
                    f"and {len(added)} added id(s) went unchecked")
    # ls-remote exits non-zero when it cannot reach the remote, so exit 0 means
    # the remote answered and an empty list is a real "nothing is claimed"
    # rather than a read that never happened.
    claimed = set(re.findall(r"(Q\d+)$", out, re.M))
    raw = args.allow or [os.environ.get("QUEUE_CLAIMS_ALLOW", "")]
    allowed = set(",".join(raw).replace(",", " ").split())
    missing = [q for q in added if q not in claimed and q not in allowed]
    for q in missing:
        print(f"queue: {q}.md files an id holding no {REF_NS}/{q} on "
              f"{args.remote}: allocate one with alloc-queue-id.sh and rename "
              f"the file, or pass --allow {q} if it was claimed elsewhere",
              file=sys.stderr)
    if missing:
        return 1
    print(f"queue: claims: {len(added)} added id(s) hold a claim on {args.remote}")
    return 0


# Split a table row on unescaped pipes only. An escaped `\|` inside a cell is
# content, and splitting on it shifts every later column.
def _row_cells(line):
    out, cur, i = [], "", 0
    while i < len(line):
        ch = line[i]
        if ch == "\\" and i + 1 < len(line) and line[i + 1] in "|\\":
            cur += line[i + 1]
            i += 2
            continue
        if ch == "|":
            out.append(cur.strip())
            cur = ""
        else:
            cur += ch
        i += 1
    out.append(cur.strip())
    return out


def rebase_link(target):
    """Rewrite a link written relative to `docs/` for a file in `docs/queue/`.

    An item page sits one directory below the table that held it, so every
    relative destination gains a `../`. A bare `#QNNN` anchor pointed at a row
    in the same table; the row is now a sibling page, so it becomes `QNNN.md` —
    which, unlike the anchor, also resolves on github.com.
    """
    if not target:
        return None
    if re.match(r"^[a-z][a-z0-9+.-]*://", target) or target.startswith("/"):
        return target
    if target.startswith("#"):
        ref = target[1:]
        return f"{ref}.md" if ID_RE.match(ref) else target
    return "../" + target


def rebase_body_links(text):
    """Rewrite every markdown link in a Notes cell for its new depth.

    The `target:` field is not the only link a row carries: Notes routinely
    cite sibling rows as `#QNNN`, and those anchors exist only in the table.
    Left alone they resolve to nothing the moment the table is deleted, and
    nothing about the resulting page looks broken until someone clicks.
    """
    return re.sub(r"\]\(([^)]*)\)",
                  lambda m: f"]({rebase_link(m.group(1)) or m.group(1)})",
                  text)


def cmd_migrate(args):
    src = Path(args.source)
    store = Path(args.store or store_dir())
    store.mkdir(parents=True, exist_ok=True)
    legacy = []
    section, rows = None, []
    for line in src.read_text(encoding="utf-8").split("\n"):
        if line.startswith("## "):
            head = line[3:].strip().lower()
            section = head if head in ("queue", "deferred") else None
            continue
        if not section or not line.startswith("|"):
            continue
        cells = _row_cells(line)
        if len(cells) < 4:
            continue
        raw_id = re.sub(r"<[^>]*>", "", cells[1]).strip()
        if not ID_RE.match(raw_id):
            continue
        # The status test below asks only whether the cell holds 🚫, so the
        # pre-counter format's ✅/▶/💤 all fall through to `ready` and put
        # shipped work back in the backlog. Unambiguous, too: the Deferred
        # table has no status column for one of these to appear in.
        if section == "queue" and len(cells) > 4:
            legacy += [(raw_id, m) for m in ("✅", "▶", "💤") if m in cells[4]]
        rows.append((section, cells, raw_id))
    if legacy:
        for item_id, mark in legacy:
            print(f"queue: {src.name}: {item_id} is {mark}, an old-format state "
                  f"with no mapping here; it would land as status: ready",
                  file=sys.stderr)
        print("queue: normalize the table to 🔲/🚫 with a ## Deferred section "
              "first, then migrate", file=sys.stderr)
        return 1
    ranks = rank_series(len(rows))
    written = 0
    for rank, (section, cells, item_id) in zip(ranks, rows):
        item_cell = cells[2]
        link = re.search(r"\[([^\]]*)\]\(([^)]*)\)", item_cell)
        target = rebase_link(link.group(2)) if link else None
        # The link is usually only *part* of the cell — "Audit [x](y) on the
        # eleven dimensions" — so the title is the whole cell with link markup
        # flattened to its text. Taking the link text alone silently drops
        # everything around it, and a truncated title still looks like a title.
        title = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", item_cell)
        labels = re.findall(r"`([^`]+)`", cells[3]) if len(cells) > 3 else []
        if section == "queue":
            st = cells[4] if len(cells) > 4 else ""
            status = "blocked" if "🚫" in st else "ready"
            size = cells[5] if len(cells) > 5 else ""
            notes = cells[6] if len(cells) > 6 else ""
        else:
            status = "deferred"
            size = cells[4] if len(cells) > 4 else ""
            notes = cells[5] if len(cells) > 5 else ""
        item = Item(id=item_id, rank=rank, labels=labels, status=status,
                    size=size or None, target=target, title=title.strip(),
                    notes=rebase_body_links(notes.strip()))
        write_item(store, item)
        written += 1
    print(f"queue: wrote {written} item(s) to {store}")
    return 0


def cmd_rank(args):
    items, _ = load(args.store or store_dir())
    ranks = [i.rank for i in items if i.rank]
    if args.head:
        print(rank_between(None, ranks[0] if ranks else None))
    elif args.tail:
        print(rank_between(ranks[-1] if ranks else None, None))
    else:
        print(rank_between(args.after, args.before))
    return 0


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--store", help="item directory (default: <repo>/docs/queue)")
    sub = p.add_subparsers(dest="cmd", required=True)

    r = sub.add_parser("render", help="print the ordered backlog")
    r.add_argument("--format", choices=("text", "table"), default="text")
    r.add_argument("--all", action="store_true", help="include deferred items")
    r.set_defaults(fn=cmd_render)

    n = sub.add_parser("next", help="print the top ready item")
    n.add_argument("--title", action="store_true")
    n.add_argument("--no-pr-check", action="store_true",
                   help="hand out the top item without asking GitHub whether "
                        "an open PR already claims it")
    n.add_argument("--allow", action="append", metavar="QNNN",
                   help="take this id even though an open PR names it")
    n.set_defaults(fn=cmd_next)

    sub.add_parser("lint", help="check the store").set_defaults(fn=cmd_lint)

    c = sub.add_parser(
        "claims",
        help="check every id this branch adds holds a claim on the remote",
        description="Every id this branch adds against its merge base with "
                    "--base must hold a refs/queue-ids/QN ref on --remote, "
                    "which is what alloc-queue-id.sh creates. A read that "
                    "cannot be taken skips, so an offline clone still runs the "
                    "gate; pass --strict where a network is guaranteed.")
    c.add_argument("--remote", default="origin", help="holds the claims")
    c.add_argument("--base", default="origin/main",
                   help="branch this one is measured against")
    c.add_argument("--allow", action="append", metavar="QNNN",
                   help="an id claimed outside this remote; repeatable, and "
                        "QUEUE_CLAIMS_ALLOW is a comma-separated default")
    c.add_argument("--strict", action="store_true",
                   help="fail rather than skip when a read cannot be taken")
    c.set_defaults(fn=cmd_claims)

    m = sub.add_parser("metrics", help="flow metrics from git history")
    m.add_argument("--events", action="store_true")
    m.set_defaults(fn=cmd_metrics)

    g = sub.add_parser("migrate", help="convert a legacy STATUS.md table")
    g.add_argument("source", help="path to the old STATUS.md")
    g.set_defaults(fn=cmd_migrate)

    k = sub.add_parser(
        "rank",
        help="compute an order key",
        description="Generates a magnitude-head base-36 order key, the same "
                    "scheme github-actions-gateway's queuestore uses, so a key "
                    "minted here interleaves correctly with one minted there.")
    k.add_argument("--after", help="rank of the item this goes below")
    k.add_argument("--before", help="rank of the item this goes above")
    k.add_argument("--head", action="store_true", help="before every item")
    k.add_argument("--tail", action="store_true", help="after every item")
    k.set_defaults(fn=cmd_rank)

    args = p.parse_args(argv)
    return args.fn(args)


if __name__ == "__main__":
    sys.exit(main())
