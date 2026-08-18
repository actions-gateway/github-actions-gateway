"""MkDocs hook that tells the theme whether this build carries the backlog page.

The backlog store publishes on the `dev` version only (Q558), so a link to it
from shared site chrome has to vanish on every other version or it ships a 404.
Rather than a second flag that can drift out of step with `exclude_docs`, this
derives the answer from the build itself: the link renders exactly when the page
is in the file set.

Exposed to the templates as `config.extra.backlog_page`, read by the version
banner in `overrides/main.html`. See docs/development/website.md § Publication
scope.
"""

from __future__ import annotations

_BACKLOG_SRC = "queue/README.md"


def on_files(files, config):
    """Record whether the backlog page survived `exclude_docs` for this build.

    `exclude_docs` does not drop the file — MkDocs keeps it and marks it
    `InclusionLevel.EXCLUDED` — so presence in `files` proves nothing and the
    inclusion level is what has to be read.
    """
    page = files.get_file_from_path(_BACKLOG_SRC)
    config["extra"]["backlog_page"] = page is not None and page.inclusion.is_included()
    return files
