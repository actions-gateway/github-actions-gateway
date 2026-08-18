"""MkDocs hook that renders each roadmap bullet's release commitment as a chip.

The commitment lives in the backlog store as an `X.Y-gate` label, meaning "this
row blocks that tag"; `docs/roadmap.md` is where an adopter reads it. The page
used to say so in prose — "Gating the 1.5 release", typed by hand on three of
twenty-three bullets — which is a second copy of a fact the backlog already
holds, and goes stale the moment a punt drops the label (Q770).

This derives it instead. Every forward-looking bullet already carries the
`<!-- q:QN -->` annotation binding it to its backlog rows, so the gates are a
lookup away:

    - **[Bind each runner set …](…)** <!-- q:Q712 -->

renders with a `1.5` pill beside the title, and loses the pill the day the label
comes off. Nothing on the page names a release, so nothing on the page can
disagree with the backlog about one.

A bullet naming several rows shows each distinct gate, lowest first. A bullet
whose rows carry no gate renders unchanged: no chip is the honest rendering of
no commitment. Every "Exploring / longer-term" entry is ungated, and so is a
near-term item whose revive trigger fired before a release was decided for it,
so an absent chip is not itself a placement finding (Q843).

Two things it deliberately does not do. It never invents a chip for a Q-ID that
is not in the store: a dangling ID means the work shipped, which is
`roadmapcheck` rule 2's finding and not something to paper over here. And an
annotation inside a code fence is prose about the format, not an annotation —
the same reading `roadmapcheck` takes, so the gate and the renderer agree about
what counts.

Wired in `mkdocs.yml` under `hooks:`. Run it directly to see the gates it
resolves without building the site:

    python3 hooks/release_gates.py

See docs/development/website.md, "The release chip".
"""

from __future__ import annotations

import os
import re
import sys

# The one page carrying release commitments. features.md states what shipped and
# needs no gate; the operator docs are versioned with the release they document.
_ROADMAP_SRC = "roadmap.md"
_STORE_DIR = "queue"

# The bullet's binding to its backlog rows. Matches roadmapcheck's own pattern,
# including the `[^-]` window that stops at the comment's closing dashes.
_ANNOTATION = re.compile(r"<!--\s*q:([^-]*)-->")

# An item's YAML frontmatter, and the `labels:` block within it. Scoped to the
# frontmatter so a label named in the body's prose is not read as one the item
# wears — the job the backticks did when labels lived in a table cell.
_FRONTMATTER = re.compile(r"\A---\n(.*?)\n---\n", re.S)
_LABELS_BLOCK = re.compile(r"^labels:(.*?)(?=^\S|\Z)", re.M | re.S)
_GATE_LABEL = re.compile(r"\b([0-9]+\.[0-9]+)-gate\b")
_ITEM = re.compile(r"\AQ[0-9]+\Z")

# A fenced code block's delimiter, at CommonMark's three leading spaces.
_FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")


def item_gates(source):
    """Return the release gates one item's Markdown declares, newest last.

    Read from the `labels:` block of the YAML frontmatter, never from the body:
    an item whose notes discuss a gate is not an item that blocks one. Both
    label shapes are accepted, the block list `migrate` writes and the inline
    list a hand-filed item may use, because the store carries both.
    """
    front = _FRONTMATTER.match(source)
    if front is None:
        return []
    block = _LABELS_BLOCK.search(front.group(1))
    if block is None:
        return []
    found = []
    for gate in _GATE_LABEL.findall(block.group(1)):
        if gate not in found:
            found.append(gate)
    return sorted(found, key=_version_key)


def gates(store_dir):
    """Return {Q-ID: [gate, …]} for every item in the store that carries one."""
    out = {}
    try:
        names = sorted(os.listdir(store_dir))
    except OSError:
        return out
    for name in names:
        if not name.endswith(".md") or not _ITEM.match(name[:-3]):
            continue
        try:
            with open(os.path.join(store_dir, name), encoding="utf-8") as handle:
                source = handle.read()
        except OSError:
            continue
        found = item_gates(source)
        if found:
            out[name[:-3]] = found
    return out


def _version_key(version):
    return tuple(int(part) for part in version.split("."))


def _chips(annotation_body, by_id):
    """Return the chip markup for one annotation's IDs, or "" when ungated."""
    found = []
    for q_id in annotation_body.replace(" ", "").split(","):
        for gate in by_id.get(q_id, ()):
            if gate not in found:
                found.append(gate)
    return "".join(
        '<span class="gag-release-chip" title="Blocks the {v} release">{v}</span>'.format(v=gate)
        for gate in sorted(found, key=_version_key)
    )


def render(markdown, by_id):
    """Return markdown with a release chip beside every gated bullet."""
    out = []
    fence = ""
    for line in markdown.split("\n"):
        match = _FENCE.match(line)
        if match:
            marker = match.group(1)
            if not fence:
                fence = marker[0]
            elif marker[0] == fence:
                fence = ""
        if fence:
            out.append(line)
            continue
        out.append(
            _ANNOTATION.sub(
                lambda m: (_chips(m.group(1), by_id) + " " + m.group(0)).lstrip(),
                line,
            )
        )
    return "\n".join(out)


def on_page_markdown(markdown, page, config, **_kwargs):
    """Render the release chips on the roadmap page, and nowhere else."""
    if page.file.src_uri != _ROADMAP_SRC:
        return markdown
    # No backlog to read is a version-free render, not a build failure: the
    # same stance hooks/release_version.py takes on a missing git history, and
    # gates() returns an empty map rather than raising.
    return render(markdown, gates(os.path.join(config["docs_dir"], _STORE_DIR)))


if __name__ == "__main__":
    docs_dir = sys.argv[1] if len(sys.argv) > 1 else "docs"
    for q_id, found in sorted(gates(os.path.join(docs_dir, _STORE_DIR)).items()):
        print(q_id, " ".join(found))
