#!/usr/bin/env python3
"""Hold each chart's README values table to its values.yaml, in both directions.

A chart README ships inside the chart tarball, so a key it never documents is
a gap every release publishes, and fixing it after a release candidate is cut
costs a new candidate. Six of the actions-gateway chart's egress and priority
allowlists shipped undocumented through v1.10.0 that way.

For every charts/<name>/values.yaml that declares a top-level key:

- each top-level key must be named by a row of the README's `## Values` table;
- each row must name a top-level key that values.yaml still declares.

A row names a key by its first backticked token's leading segment, so
`agc.image.{repository,tag,digest}` names `agc`, and a row may name several
(`nodeSelector` / `tolerations` / `affinity`). Sub-keys are not checked.

Exits 1 on a finding, 2 when an input cannot be read or parses to nothing.
"""

import re
import sys
from pathlib import Path

TOP_KEY = re.compile(r"^([A-Za-z_][\w-]*):")
TOKEN = re.compile(r"`([^`]+)`")
ROOT = re.compile(r"[A-Za-z_][\w-]*")


def values_keys(path):
    return [m.group(1) for line in path.read_text().splitlines() if (m := TOP_KEY.match(line))]


def table_roots(path):
    """Return {root: row line number} from the `## Values` table, or None if absent."""
    lines = path.read_text().splitlines()
    try:
        start = next(i for i, line in enumerate(lines) if line.strip() == "## Values")
    except StopIteration:
        return None
    roots = {}
    for n, line in enumerate(lines[start + 1 :], start=start + 2):
        if line.startswith("#"):
            break
        if not line.startswith("|") or set(line) <= set("|-: "):
            continue
        first = line.split("|")[1]
        for token in TOKEN.findall(first):
            if m := ROOT.match(token):
                roots.setdefault(m.group(0), n)
    return roots


def main():
    charts = sorted(p.parent for p in Path("charts").glob("*/values.yaml"))
    if not charts:
        print("check-chart-values-docs: no charts/*/values.yaml found", file=sys.stderr)
        return 2
    findings, checked = [], 0
    for chart in charts:
        keys = values_keys(chart / "values.yaml")
        if not keys:
            continue
        readme = chart / "README.md"
        if not readme.is_file():
            findings.append(f"{readme}: missing, and values.yaml declares {len(keys)} key(s)")
            continue
        roots = table_roots(readme)
        if roots is None:
            findings.append(f"{readme}: no `## Values` table, and values.yaml declares {len(keys)} key(s)")
            continue
        if not roots:
            print(f"check-chart-values-docs: {readme}: `## Values` table parsed to no rows", file=sys.stderr)
            return 2
        checked += 1
        for key in keys:
            if key not in roots:
                findings.append(f"{readme}: values key `{key}` has no row in the `## Values` table")
        for root, line in sorted(roots.items(), key=lambda kv: kv[1]):
            if root not in keys:
                findings.append(f"{readme}:{line}: row names `{root}`, which {chart}/values.yaml does not declare")
    for f in findings:
        print(f, file=sys.stderr)
    if findings:
        print(f"check-chart-values-docs: {len(findings)} finding(s)", file=sys.stderr)
        return 1
    print(f"check-chart-values-docs: ok ({checked} chart README(s) cover their values.yaml)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
