#!/usr/bin/env python3
"""check-queue-rules.py — the backlog rules `queue.py lint` has no equivalent for.

`queue.py lint` is a pure function of a directory: frontmatter, rank shape,
filename/id agreement, the title cap, unresolvable targets. This repo's own
rules cannot be expressed that way. Four are functions of what the *branch
changed* rather than of what the store holds:

  8. A `flake` item may not simply vanish. A shipped mitigation parks it in
     flake watch, and it leaves only through the ledger; deleting it throws
     away the memory that a fix was already attempted, so a second occurrence
     reads as a fresh find. A groom retires it once it soaks or goes obsolete,
     but the fixing change may retire it directly when the fix already makes
     it obsolete -- what the rule reads is the ledger entry, not who wrote it.
  9. Deleting the last item targeting a plan obliges that plan's index row to
     stop reading as open work.
 11. Every label an item wears is declared, so a typo cannot stick silently.
 13. An item this branch files cites any near-duplicate the matcher flagged, so
     a warning that fired is a warning somebody answered.

The fifth is a function of the store and of where this repo publishes it, which
is why it is here rather than in the vendored checker:

 14. A link a row carries resolves for MkDocs. `docs/queue/` publishes at
     `/dev/queue/`, so a link that leaves `docs/` and points back into it is one
     MkDocs cannot serve, and `mkdocs --strict` aborts the build. No local gate
     builds the site, so the whole class was invisible until CI (Q1054).

Rule 13 is the one rule here that shells out. `find-duplicate-rows.sh` already
scores a candidate title against a store, and `make queue-id` prints its verdict
at filing time, but that verdict was advisory and left no trace: Q987 was filed
while the matcher scored Q922 at 0.50 and shipped citing it zero times, which is
how three rows came to describe one defect and two sessions collided on it
(Q1045). The matcher is quiet enough to gate on: 25 flagged pairs out of 17,955
across the 190 items at 2eec06d51, measured 2026-08-31 with `--audit`. It names
a commit rather than "the merge base", which is what a rebase moves underneath
it -- this figure was already stale once that way.

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
import posixpath
import re
import subprocess
import sys
import tempfile
from pathlib import Path

STORE = "docs/queue"
LEDGER = "docs/development/flake-watch-retired.md"
PLAN_INDEX = "docs/plan/README.md"
# A plan row still advertising work. `✅` and `💤` and `ⓘ` do not.
OPEN_MARKERS = ("⚠️", "🚧", "❌", "🔲")
LABELS_LINE = re.compile(r"^\*\*Labels:\*\*(.*)$", re.M)
LABEL = re.compile(r"`([^`]+)`")
FRONTMATTER = re.compile(r"\A---\n(.*?)\n---\n", re.S)
TITLE = re.compile(r"^#\s+(.+?)\s*$", re.M)
MATCHER = "scripts/docs/find-duplicate-rows.sh"
# MkDocs resolves a page's relative links from inside `docs_dir`, which is
# `docs/` here (mkdocs.yml sets none, so the default applies). Derived from
# STORE rather than written again, so the two cannot drift apart.
DOCS_DIR = posixpath.dirname(STORE)
PAGE_DIR = posixpath.basename(STORE)
# A markdown inline link or image target: the `(...)` of `](...)`, and the
# destination of a reference-style `[label]: target` definition. Both shapes are
# hooks/source_links.py's, because Python-Markdown resolves the two into the
# same link and a reference-style one ships exactly as dead as an inline one.
INLINE_TARGET = re.compile(r"(?<=]\()([^()\s]+)(?=[)\s])")
REF_TARGET = re.compile(r"(?m)^ {0,3}\[[^\]\n]+\]:[ \t]+(\S+)")
# A fenced block or a code span is rendered as text, so a link inside one is not
# a link and MkDocs never resolves it. Stripping them is what keeps rule 14
# free of an override: a row documenting the rule quotes a bad link on purpose.
FENCE = re.compile(r"(?ms)^([ \t]*)(`{3,}|~{3,}).*?^\1?\2[ \t]*$")
CODE_SPAN = re.compile(r"(`+)(?:.|\n)*?\1")
# `scheme://…` or `mailto:…`, deliberately narrower than RFC 3986: this repo
# writes source references as `path/to/file.go:91`, which a general scheme
# pattern reads as the scheme `path/to/file.go`.
ABSOLUTE = re.compile(r"[A-Za-z][A-Za-z0-9+.\-]*://|mailto:")
# The `:91` of `main.go:91`, the clickable file:line form used throughout.
LINE_SUFFIX = re.compile(r":[0-9]+$")


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
            f"watch and a ledger entry is what retires it, so a recurrence "
            f"reads as a recurrence rather than a fresh find. Retiring at fix "
            f"time is allowed: add the ledger line in this change. Set "
            f"QUEUE_ALLOW_FLAKE_DELETE={qid} for a deliberate drop.")


def plan_file_of(body):
    """The plan file a target names, with any `#anchor` dropped.

    A target may point at a section rather than the whole doc, and that is
    still a reference to the doc: Q539 and Q540 both target
    q408-untrusted-pr-egress.md#6-follow-on-validations-q539-q540. Comparing
    raw target strings made those invisible, so closing Q408 read as the
    plan's last item leaving while two live rows still pointed at it.
    """
    target = target_of(body)
    return Path(target.split("#", 1)[0]).name if target else ""


def rule9(base_items, head_items, index_text, failures):
    """Deleting a plan's last item obliges its index row to stop reading open."""
    excused = allow("QUEUE_ALLOW_PROGRESS_STALE")
    live = {plan_file_of(b) for b in head_items.values() if plan_file_of(b)}
    for qid, body in sorted(base_items.items()):
        name = plan_file_of(body)
        if qid in head_items or not name or name in live:
            continue
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


def prose_of(body):
    """The item minus its frontmatter, which is metadata rather than an answer."""
    m = FRONTMATTER.search(body or "")
    return (body or "")[m.end():] if m else (body or "")


def title_of(body):
    m = TITLE.search(body or "")
    return m.group(1).strip() if m else ""


def flagged_for(root, base_dir, body):
    """Ids the matcher scores against the store as it stood at the merge base."""
    title = title_of(body)
    if not title:
        return []
    cmd = [str(root / MATCHER), "--porcelain", "--store", str(base_dir)]
    target = target_of(body)
    if target:
        cmd += ["--target", target]
    try:
        p = subprocess.run(cmd + [title], capture_output=True, text=True)
    except OSError as e:
        raise Unreadable(f"{MATCHER}, which rule 13 scores against: {e}")
    if p.returncode != 0:
        raise Unreadable(f"{MATCHER}: {p.stderr.strip() or p.returncode}")
    hits = []
    for line in p.stdout.splitlines():
        f = line.split("\t")
        if len(f) >= 2 and re.fullmatch(r"Q\d+", f[1]):
            hits.append(f[1])
    return hits


def rule13(base_items, head_items, root, base_dir, failures):
    """An item this branch files answers any near-duplicate the matcher found."""
    excused = allow("QUEUE_ALLOW_UNCITED_DUPLICATE")
    for qid, body in sorted(head_items.items()):
        if qid in base_items or qid in excused:
            continue
        hits = [h for h in flagged_for(root, base_dir, body) if h != qid]
        if not hits:
            continue
        # Citing any one is enough. What this asks is that the warning was read,
        # not that every candidate is a duplicate: the matcher's own calibration
        # expects false positives, and Q922 answering Q830 is the shape wanted.
        #
        # Prose only. A `target:` naming the flagged item would otherwise clear
        # the rule without a word written, and a target is chosen for where the
        # work lands rather than to answer a warning.
        if any(re.search(rf"\b{re.escape(h)}\b", prose_of(body)) for h in hits):
            continue
        failures.append(
            f"rule 13: {qid} is filed while the near-duplicate search flags "
            f"{', '.join(hits)}, and its body names none of them. Say how it "
            f"differs, or fold it into that row -- `make queue-id` prints the "
            f"same verdict at filing time. Set "
            f"QUEUE_ALLOW_UNCITED_DUPLICATE={qid} for a deliberate pass.")


def unservable(target):
    """`(reason, suggestion)` when MkDocs cannot serve this link, else None.

    MkDocs resolves a `docs/queue/` page's links from `queue/` inside
    `docs_dir`, so enough `../` to leave `docs/` puts the link outside anything
    this build can serve. Two ways that ends badly, and one way it does not:

    - It points back *into* `docs/`. `hooks/source_links.py` sees a resolved
      path under `docs/` that the `dev` build publishes and leaves it alone, so
      the dead link reaches MkDocs and `--strict` aborts. The suggestion is the
      same file written relative to the store.
    - It leaves the repository altogether. source_links builds no URL for a
      path it cannot resolve under the root, so that link is dead too. There is
      no rewrite to suggest.
    - It escapes and stays inside the repo — `../../scripts/go/coverage.sh`, a
      workflow, `../../.mdreflow.yaml`. This is the ordinary case and it is
      fine: source_links rewrites it to a `repo_url` blob URL.
    """
    path = target.split("#", 1)[0]
    path = LINE_SUFFIX.sub("", path)
    if not path or path.startswith(("#", "/")) or ABSOLUTE.match(path):
        return None
    if not posixpath.normpath(posixpath.join(PAGE_DIR, path)).startswith(".."):
        return None  # MkDocs resolves it inside docs_dir
    reached = posixpath.normpath(posixpath.join(STORE, path))
    if reached == DOCS_DIR or reached.startswith(DOCS_DIR + "/"):
        return "re-enters it", posixpath.relpath(reached, STORE)
    if reached.startswith(".."):
        return "leaves the repository", None
    return None  # escapes and stays in the repo, which source_links absolutizes


def links_of(body):
    """(where, target) for every relative link a row carries.

    The frontmatter `target:` renders as the item's link in the `/dev/queue/`
    table, and the item page publishes on `dev` too, so a prose link breaks the
    build the same way from the same directory. Fenced blocks and code spans are
    stripped first: they render as text, so a link quoted inside one is not a
    link, and firing on it would be a wrong deny with no way out.
    """
    found = []
    target = target_of(body)
    if target:
        found.append(("its frontmatter `target:`", target))
    prose = CODE_SPAN.sub("", FENCE.sub("", prose_of(body)))
    for pattern in (INLINE_TARGET, REF_TARGET):
        for hit in pattern.findall(prose):
            found.append(("its notes", hit))
    return found


def rule14(head_items, failures):
    """Every link a row carries is one MkDocs can resolve.

    No override: unlike rules 8 and 13 this encodes no judgement about the
    item, only whether the published site can serve the link. There is no
    deliberate way to write one it cannot.
    """
    for qid, body in sorted(head_items.items()):
        for where, target in links_of(body):
            verdict = unservable(target)
            if verdict is None:
                continue
            reason, fixed = verdict
            fragment = target.split("#", 1)[1] if "#" in target else ""
            remedy = (f"Write it relative to the store: "
                      f"`{fixed}{'#' + fragment if fragment else ''}`."
                      if fixed else
                      f"Point it at something inside the repository.")
            failures.append(
                f"rule 14: {qid} links `{target}` in {where}, which leaves "
                f"`{DOCS_DIR}/` and {reason}. MkDocs resolves a `{STORE}/` "
                f"page's links from inside `{DOCS_DIR}/`, so it cannot serve "
                f"that and `mkdocs --strict` aborts -- a class no local gate "
                f"builds the site to catch. {remedy}")


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
    rule14(head_items, failures)
    # Rule 13 scores against the store as it stood at the base, so a row filed
    # by this branch is not scored against its own siblings -- two items filed
    # together are one editorial act, and the matcher would pair them with each
    # other rather than with what was already there.
    try:
        with tempfile.TemporaryDirectory() as tmp:
            base_dir = Path(tmp)
            for qid, body in base_items.items():
                (base_dir / f"{qid}.md").write_text(body)
            rule13(base_items, head_items, root, base_dir, failures)
    except Unreadable as e:
        print(f"check-queue-rules: could not read {e}; refusing to guess",
              file=sys.stderr)
        return 2

    for f in failures:
        print(f)
    if failures:
        print(f"check-queue-rules: FAILED - {len(failures)} finding(s)")
        return 1
    print(f"check-queue-rules: ok ({len(head_items)} item(s), "
          f"{len(base_items)} at the merge base, rules 8, 9, 11, 13 and 14)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
