"""MkDocs hook that renders `\\|` inside a table cell's code span as `|`, the way GitHub does.

A pipe inside a table cell is written `\\|`. GitHub unescapes it everywhere in
the row, code spans included, so `` `agc\\|proxy` `` shows `agc|proxy`.
Python-Markdown unescapes it only outside a code span, so the same source
publishes `agc\\|proxy`, and a PromQL query pasted from the site matches a
literal pipe (Q1269). Writing a bare `|` instead renders here and splits the
cell on GitHub, so the source keeps GitHub's form and this hook unescapes it.

Two halves:

- `on_page_markdown` rewrites `\\|` to `|` inside every code span on a table row
  outside a fenced block. `\\\\|` becomes `\\|`, as on GitHub.
- `on_page_content` warns on any `\\|` still inside a `<code>` in a table cell,
  which `--strict` turns into a failed build. A cell that means a literal
  backslash-pipe writes it as `<code>\\&#124;</code>`, which this check does not
  read as one.

Wired last in `mkdocs.yml` under `hooks:`, so it also sees the backlog table
`hooks/queue_page.py` appends. See docs/development/website.md.
"""

from __future__ import annotations

import logging
import re

log = logging.getLogger("mkdocs.hooks.table_pipes")

_FENCE = re.compile(r"^\s*(?:>\s*)*(`{3,}|~{3,})")
_ROW = re.compile(r"^\s*(?:>\s*)*\|")
_SPAN = re.compile(r"(?<!`)(?:(?<!\\)|(?<=\\\\))(`+)(?!`)(.+?)(?<!`)\1(?!`)")
_CELL = re.compile(r"<t([dh])\b[^>]*>(.*?)</t\1>", re.S)
_CODE = re.compile(r"<code\b[^>]*>(.*?)</code>", re.S)


def unescape(markdown: str) -> str:
    """Rewrite `\\|` to `|` inside code spans on table rows outside fences."""
    out = []
    fence = None
    for line in markdown.split("\n"):
        m = _FENCE.match(line)
        if m:
            marker = m.group(1)
            if fence is None:
                fence = marker
            elif marker[0] == fence[0] and len(marker) >= len(fence):
                fence = None
        elif fence is None and _ROW.match(line):
            line = _SPAN.sub(lambda s: s.group(0).replace("\\|", "|"), line)
        out.append(line)
    return "\n".join(out)


def escaped_pipes(html: str) -> list[str]:
    """Return the text of each table-cell `<code>` that still holds `\\|`."""
    return [
        code
        for cell in _CELL.finditer(html)
        for code in _CODE.findall(cell.group(2))
        if "\\|" in code
    ]


def on_page_markdown(markdown, page, config, **kwargs):
    return unescape(markdown)


def on_page_content(html, page, config, files, **kwargs):
    for code in escaped_pipes(html):
        log.warning(
            "table_pipes: %s: a table cell's code renders a literal \\| "
            "(GitHub shows |): %s",
            page.file.src_uri,
            code[:120],
        )
    return html
