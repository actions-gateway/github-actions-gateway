"""MkDocs hook that points out-of-docs relative links at the GitHub repository.

The `docs/` tree doubles as in-repo documentation browsed on github.com, where a
relative link like `../cmd/agc/main.go` resolves to the source file. MkDocs has
no such file to serve, so it leaves the link untouched and the published page
404s. Publishing the repo-internal docs on the `dev` version (Q558) made that
load-bearing — `STATUS.md` alone carries 34 source links — but the same links
already shipped dead from `design/` and `operations/`.

This hook rewrites every relative target that escapes `docs_dir` into an
absolute URL under `repo_url`, so one markdown link works in both places:

    ../cmd/agc/main.go     ->  {repo_url}/blob/{ref}/cmd/agc/main.go
    ../cmd/agc/main.go:91  ->  {repo_url}/blob/{ref}/cmd/agc/main.go#L91
    ../cmd/gmc/internal/   ->  {repo_url}/tree/{ref}/cmd/gmc/internal

`{ref}` is `$GAG_DOCS_SOURCE_REF`, defaulting to `main`; `.github/workflows/
pages.yml` sets it to the tag when deploying a release, so each version links to
the source it documents.

A target that does not exist in the working tree is left alone: a typo should
keep failing MkDocs' link check rather than become a plausible-looking 404.

Wired in `mkdocs.yml` under `hooks:`. See docs/development/website.md,
"Links into the source tree".
"""

from __future__ import annotations

import os
import posixpath
import re

# A markdown inline link or image target: the `(...)` of `](...)`. Excludes
# nested parens and whitespace, which is the whole corpus and keeps an optional
# `"title"` suffix out of the captured target.
_TARGET = re.compile(r"(?<=]\()([^()\s]+)(?=[)\s])")

# `scheme://…` or `mailto:…`. Deliberately narrower than RFC 3986: the repo
# writes source references as `path/to/file.go:91`, which a general scheme
# pattern would read as the scheme `path/to/file.go`.
_ABSOLUTE = re.compile(r"[A-Za-z][A-Za-z0-9+.\-]*://|mailto:")

# The `:91` of `main.go:91` — the clickable file:line form used throughout.
_LINE_SUFFIX = re.compile(r":([0-9]+)$")

_DEFAULT_REF = "main"


def _is_relative(target):
    """True for a target MkDocs would resolve against the page's directory."""
    return not target.startswith(("#", "/")) and not _ABSOLUTE.match(target)


def on_config(config):
    """Cache the repo layout the rewrite needs, once per build."""
    repo_root = os.path.dirname(os.path.abspath(config["config_file_path"]))
    config["extra"].setdefault("source_links", {}).update(
        repo_root=repo_root,
        docs_prefix=os.path.relpath(config["docs_dir"], repo_root).replace(os.sep, "/"),
        base=config["repo_url"].rstrip("/"),
        ref=os.environ.get("GAG_DOCS_SOURCE_REF", "").strip() or _DEFAULT_REF,
    )
    return config


def rewrite(markdown, page_dir, repo_root, docs_prefix, base, ref):
    """Return markdown with every out-of-docs relative target absolutized."""

    def replace(match):
        target = match.group(1)
        if not _is_relative(target):
            return target
        path, _, fragment = target.partition("#")
        if not path:
            return target
        line = _LINE_SUFFIX.search(path)
        if line:
            path = path[: line.start()]
            fragment = fragment or f"L{line.group(1)}"
        resolved = posixpath.normpath(posixpath.join(docs_prefix, page_dir, path))
        # Still under docs/ (MkDocs resolves it), or outside the repo entirely.
        if resolved == docs_prefix or resolved.startswith((docs_prefix + "/", "..")):
            return target
        on_disk = os.path.join(repo_root, resolved.replace("/", os.sep))
        if not os.path.exists(on_disk):
            return target
        kind = "tree" if os.path.isdir(on_disk) else "blob"
        url = f"{base}/{kind}/{ref}/{resolved}"
        return f"{url}#{fragment}" if fragment else url

    return _TARGET.sub(replace, markdown)


def on_page_markdown(markdown, page, config, **_kwargs):
    """Rewrite this page's source links before MkDocs resolves them."""
    return rewrite(
        markdown,
        posixpath.dirname(page.file.src_uri),
        **config["extra"]["source_links"],
    )
