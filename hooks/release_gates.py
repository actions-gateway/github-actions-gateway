"""MkDocs hook that renders each roadmap bullet's release commitment as a chip.

The commitment lives in `docs/STATUS.md` as an `X.Y-gate` label, meaning "this
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
whose rows carry no gate — every "Exploring / longer-term" entry — renders
unchanged: no chip is the honest rendering of no commitment.

Two things it deliberately does not do. It never invents a chip for a Q-ID that
is not in `STATUS.md`: a dangling ID means the work shipped, which is
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
_STATUS_SRC = "STATUS.md"

# The bullet's binding to its backlog rows. Matches roadmapcheck's own pattern,
# including the `[^-]` window that stops at the comment's closing dashes.
_ANNOTATION = re.compile(r"<!--\s*q:([^-]*)-->")

# The STATUS.md ID cell, `<a id="QN"></a>QN`, and the `X.Y-gate` label. The
# backticks are load-bearing: they separate the label from a mention of one in
# prose.
_ROW_ID = re.compile(r'<a id="(Q[0-9]+)"></a>')
_GATE_LABEL = re.compile(r"`([0-9]+\.[0-9]+)-gate`")

# A fenced code block's delimiter, at CommonMark's three leading spaces.
_FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")

_LABELS_COLUMN = "labels"


def _cells(row):
    """Split one GFM table row into its cells.

    Cells here never contain an escaped `|` — one would end the column — so the
    naive split is exact for this file, and the `-gate` labels it reads are the
    same ones devtools/docs/roadmapcheck reads off a real Markdown AST.
    """
    return [cell.strip() for cell in row.strip().strip("|").split("|")]


def gates(status_markdown):
    """Return {Q-ID: [gate, …]} for every row in the backlog that carries one.

    The Labels column is located from each table's header rather than assumed,
    so a column added ahead of it moves the read instead of silently pointing
    the gate lookup at the wrong cell.
    """
    out = {}
    labels_at = None
    for line in status_markdown.splitlines():
        if not line.lstrip().startswith("|"):
            labels_at = None
            continue
        cells = _cells(line)
        match = _ROW_ID.search(line)
        if match is None:
            lowered = [cell.lower() for cell in cells]
            if _LABELS_COLUMN in lowered:
                labels_at = lowered.index(_LABELS_COLUMN)
            continue
        if labels_at is None or labels_at >= len(cells):
            continue
        found = []
        for gate in _GATE_LABEL.findall(cells[labels_at]):
            if gate not in found:
                found.append(gate)
        if found:
            out[match.group(1)] = sorted(found, key=_version_key)
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
    status = os.path.join(config["docs_dir"], _STATUS_SRC)
    try:
        with open(status, encoding="utf-8") as handle:
            source = handle.read()
    except OSError:
        # No backlog to read is a version-free render, not a build failure: the
        # same stance hooks/release_version.py takes on a missing git history.
        return markdown
    return render(markdown, gates(source))


if __name__ == "__main__":
    docs_dir = sys.argv[1] if len(sys.argv) > 1 else "docs"
    with open(os.path.join(docs_dir, _STATUS_SRC), encoding="utf-8") as handle:
        for q_id, found in sorted(gates(handle.read()).items()):
            print(q_id, " ".join(found))
