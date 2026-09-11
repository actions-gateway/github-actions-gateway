#!/usr/bin/env python3
"""Compare the dashboard doc's panel tables to the shipped dashboard JSON (Q1091).

`docs/operations/observability-dashboards.md` documents each dashboard as a
sequence of `**Row N — Title**` markers, each followed by a table of its panels.
Both sides are machine-readable and nothing compared them, so Q961 reconciled
all four by hand: the tenant section numbered nine rows 1..7, **7**, 8, and a
Cross-tenant Throughput panel had escaped its table to render as literal
pipe-separated text mid-paragraph. `promql-check` parses panel expressions out
of the JSON and never reads the prose; `dashboard-render-check` is a screenshot
gate. Neither instance was visible to either.

What is asserted:

  1. The doc's rows and the dashboard's rows are the same count, in the same
     order, with the same titles.
  2. The doc numbers its rows 1..N with no gap and no repeat — the duplicate
     `Row 7` above.
  3. Each row's table holds one line per panel the JSON puts in that row — the
     escaped panel above, which leaves its table one row short.

Panel *titles* are deliberately not compared. Seven panels carry a shortened
label in the doc (`p95 latency` for `p95 pod creation latency`, and the `(1h)`
window suffixes the Job Throughput table drops), and those labels read better in
a table that already carries the query beside them, so verbatim parity would be
a rewrite of the page rather than a gate on it. The count rule catches both of
Q961's instances without it.

The row title *is* compared, because it is the heading an operator matches
against Grafana rather than a label beside a query. One row was written
non-verbatim when this gate was added (`Cross-tenant Throughput`, whose
parenthetical sat outside the bold and so read as prose); it was aligned rather
than exempted.

Usage:
    check-dashboard-tables.py <doc.md> <dashboard.json>...

Findings print as `file:line: message`, or as GitHub `::error::` annotations
when GITHUB_ACTIONS is set. Exits 1 on any finding, and 2 when the doc holds no
row marker at all, since the gate would otherwise pass by checking nothing.
"""

import json
import os
import re
import sys

# A row marker: a bold run opening with `Row N`, then a separator, then the
# title. Both separators ship today, an em-dash in three sections and a colon in
# the fourth, so the gate reads the page as written rather than legislating one.
ROW_RE = re.compile(r"^\*\*Row (\d+)\s*[-—–:]\s*(.+?)\*\*(.*)$")
# A section opens one dashboard, and the word before "dashboard" names its file.
SECTION_RE = re.compile(r"^## (\S+) dashboard\s*$", re.IGNORECASE)
# A table divider (`|---|---|`), which is not a panel.
DIVIDER_RE = re.compile(r"^\|[\s\-:|]+\|$")


def doc_rows(path):
    """Return {section: [(number, title, trailing, panels, line)]} from the doc."""
    sections = {}
    current = None
    with open(path, encoding="utf-8") as fh:
        lines = fh.read().split("\n")
    for i, line in enumerate(lines, start=1):
        section = SECTION_RE.match(line)
        if section:
            current = section.group(1).lower()
            sections.setdefault(current, [])
            continue
        if current is None:
            continue
        marker = ROW_RE.match(line)
        if marker:
            sections[current].append(
                {
                    "number": int(marker.group(1)),
                    "title": marker.group(2),
                    "trailing": marker.group(3).strip(),
                    "panels": 0,
                    "line": i,
                }
            )
            continue
        rows = sections[current]
        if rows and line.startswith("| ") and not line.startswith("| Panel") and not DIVIDER_RE.match(line):
            rows[-1]["panels"] += 1
    return sections


def json_rows(path):
    """Return [(title, panel count)] for one dashboard, in row order.

    A panel before the first row belongs to no row and is not counted: Grafana
    renders it above them, and the doc's tables start at Row 1.
    """
    with open(path, encoding="utf-8") as fh:
        dashboard = json.load(fh)
    rows = []
    for panel in dashboard.get("panels", []):
        if panel.get("type") == "row":
            rows.append([panel.get("title", ""), 0])
        elif rows:
            rows[-1][1] += 1
    return rows


def compare(doc, section, shipped, findings):
    """Compare one section's rows to one dashboard's, appending findings."""
    documented = doc.get(section, [])
    for index, want in enumerate(shipped):
        if index >= len(documented):
            findings.append(
                (
                    documented[-1]["line"] if documented else 1,
                    f"the {section} dashboard ships a row {index + 1} "
                    f"({want[0]!r}, {want[1]} panel(s)) that the doc does not document",
                )
            )
            continue
        got = documented[index]
        if got["number"] != index + 1:
            findings.append(
                (
                    got["line"],
                    f"the {section} dashboard's row {index + 1} is numbered "
                    f"`Row {got['number']}`, so the doc's numbering has a gap or a repeat",
                )
            )
        if got["title"] != want[0]:
            detail = f" (the rest of the title sits outside the bold: {got['trailing']!r})" if got["trailing"] else ""
            findings.append(
                (
                    got["line"],
                    f"the {section} dashboard names this row {want[0]!r} and the doc writes "
                    f"{got['title']!r}{detail}",
                )
            )
        if got["panels"] != want[1]:
            findings.append(
                (
                    got["line"],
                    f"the {section} dashboard puts {want[1]} panel(s) in {want[0]!r} and "
                    f"the doc's table holds {got['panels']}",
                )
            )
    for extra in documented[len(shipped) :]:
        findings.append(
            (
                extra["line"],
                f"the doc documents a row {extra['number']} ({extra['title']!r}) that the "
                f"{section} dashboard does not ship",
            )
        )


def main(argv):
    if len(argv) < 3:
        print("usage: check-dashboard-tables.py <doc.md> <dashboard.json>...", file=sys.stderr)
        return 2
    doc_path, dashboards = argv[1], argv[2:]
    doc = doc_rows(doc_path)
    documented = sum(len(rows) for rows in doc.values())
    if documented == 0:
        print(
            f"check-dashboard-tables: {doc_path} holds no `**Row N — Title**` marker, "
            "so this gate would check nothing",
            file=sys.stderr,
        )
        return 2

    findings = []
    checked = 0
    for path in dashboards:
        # grafana-dashboard-<section>.json pairs with `## <Section> dashboard`.
        section = os.path.basename(path).removeprefix("grafana-dashboard-").removesuffix(".json").lower()
        if section not in doc:
            print(
                f"check-dashboard-tables: {doc_path} has no `## {section.title()} dashboard` "
                f"section for {path}, so this gate would check nothing about it",
                file=sys.stderr,
            )
            return 2
        shipped = json_rows(path)
        checked += len(shipped)
        compare(doc, section, shipped, findings)

    gha = bool(os.environ.get("GITHUB_ACTIONS"))
    for line, message in sorted(findings):
        if gha:
            print(f"::error file={doc_path},line={line}::{message}")
        else:
            print(f"{doc_path}:{line}: {message}")
    if findings:
        print(
            f"check-dashboard-tables: FAILED - {len(findings)} finding(s) between "
            f"{doc_path} and {len(dashboards)} dashboard(s)"
        )
        return 1
    print(
        f"check-dashboard-tables: ok ({checked} row(s) across {len(dashboards)} dashboard(s), "
        f"{documented} documented)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
