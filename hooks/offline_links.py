"""MkDocs hook that keeps hand-written page links working in the offline export.

A link written as raw HTML (`<a href="../operations/foo/">`) never passes
through MkDocs' link resolution, so it keeps the directory form the live site
serves. `make docs-export` builds with `use_directory_urls: false`, where that
directory does not exist and the link opens a folder listing or nothing.

When directory URLs are off, this hook rewrites each relative directory link
that names a published page to that page's `.html` file:

    ../operations/foo/#bar  ->  operations/foo.html#bar   (from why-gag.html)

With directory URLs on it returns the page unchanged, so the live site is
untouched. A directory link naming no page is left alone for the export's own
link check to report.

Wired in `mkdocs.yml` under `hooks:`. See docs/development/website.md,
"Offline export".
"""

from __future__ import annotations

import posixpath
import re

# A relative href ending in `/`, with an optional fragment. Schemes, root-absolute
# paths, and bare fragments are excluded by the lookahead.
_DIR_HREF = re.compile(r'href="(?![A-Za-z][A-Za-z0-9+.-]*:|/|#)([^"#?]*/)(#[^"]*)?"')


def directory_url(src_uri: str) -> str:
    """The URL MkDocs gives a page when directory URLs are on, e.g. `operations/foo/`."""
    stem = src_uri[:-3] if src_uri.endswith(".md") else src_uri
    if posixpath.basename(stem) in ("index", "README"):
        stem = posixpath.dirname(stem)
    return f"{stem}/" if stem else ""


def rewrite(html: str, page_src: str, page_url: str, pages: dict[str, str]) -> str:
    """Rewrite directory links in `html`, a page whose source is `page_src` and
    whose output is `page_url`. `pages` maps each page's directory URL to its
    output URL."""
    here = directory_url(page_src)
    origin = posixpath.dirname(page_url) or "."

    def sub(m: re.Match[str]) -> str:
        target = posixpath.normpath(posixpath.join(here, m.group(1)))
        key = "" if target == "." else f"{target}/"
        if key not in pages:
            return m.group(0)
        return f'href="{posixpath.relpath(pages[key], origin)}{m.group(2) or ""}"'

    return _DIR_HREF.sub(sub, html)


def on_page_content(html, page, config, files, **kwargs):
    if config["use_directory_urls"]:
        return html
    pages = {directory_url(f.src_uri): f.url for f in files.documentation_pages()}
    return rewrite(html, page.file.src_uri, page.file.url, pages)
