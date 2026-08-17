"""MkDocs hook that renders the ordered backlog into the `/dev/queue/` page.

`docs/STATUS.md` published its Queue table at `/dev/STATUS/`; the item store has
no such page, because the order lives in each item's `rank` and exists only once
something reads all of them (Q889, decision 2).

So the page is rendered at build time rather than committed. It is appended to
`docs/queue/README.md`, which MkDocs already serves as `/dev/queue/index.html`,
which means there is no generated file in the tree: nothing to gitignore, and no
gate needed to keep a stale committed copy from reappearing. On github.com the
same README renders as the conventions page alone, which is the right reading
there, since a table of 177 rows is not what a reader browsing the source wants.

The table comes from `queue.py render`, the command a human runs, rather than
from a second renderer here. A page that formats items itself would be free to
disagree with the CLI about order, truncation or status, and nothing would
notice: the two are read by different people at different times.

Relative links in that output are already written for a page living in
`docs/queue/`, which is exactly where this one lives.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

_PAGE = "queue/README.md"
_HEADING = "\n\n## The backlog, in priority order\n\n"


def on_page_markdown(markdown, page, config, **kwargs):
    """Append the rendered backlog to the store's index page."""
    if page.file.src_uri != _PAGE:
        return None

    root = Path(config["docs_dir"]).parent
    proc = subprocess.run(
        ["python3", str(root / "scripts" / "docs" / "queue.py"),
         "--store", str(root / "docs" / "queue"), "render",
         "--format", "table", "--all"],
        capture_output=True, text=True, cwd=root,
    )
    # Fail the build rather than publishing a page whose backlog silently
    # vanished: an empty section renders as a clean, current, empty backlog.
    #
    # Counting rows, not bytes. `render` against a store that is absent or empty
    # exits 0 and prints the two header lines, so a non-empty-output check reads
    # 67 characters of table furniture as a healthy render. Measured: pointing
    # this hook at a missing directory built green until the guard counted rows.
    items = [ln for ln in proc.stdout.splitlines() if ln.startswith("| [Q")]
    if proc.returncode != 0 or not items:
        raise RuntimeError(
            f"queue render produced {len(items)} item(s) for {_PAGE}: "
            f"rc={proc.returncode} {(proc.stderr or proc.stdout).strip()[:400]}"
        )

    note = (
        "Rendered from `docs/queue/` at build time, so it is the order this "
        "site was built from rather than a committed snapshot. "
        "Notes are truncated here; open an item for the whole note.\n\n"
    )
    return markdown + _HEADING + note + proc.stdout.strip() + "\n"
