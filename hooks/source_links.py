"""MkDocs hook that points links this build cannot serve at the GitHub repository.

The `docs/` tree doubles as in-repo documentation browsed on github.com, where a
relative link like `../cmd/agc/main.go` resolves to the source file. MkDocs has
no such file to serve, so it leaves the link untouched and the published page
404s. Publishing the repo-internal docs on the `dev` version (Q558) made that
load-bearing — the backlog alone carries dozens of source links — but the same links
already shipped dead from `design/` and `operations/`.

This hook rewrites every relative target the build does not publish into an
absolute URL under `repo_url`, so one markdown link works in both places:

    ../cmd/agc/main.go     ->  {repo_url}/blob/{ref}/cmd/agc/main.go
    ../cmd/agc/main.go:91  ->  {repo_url}/blob/{ref}/cmd/agc/main.go#L91
    ../cmd/gmc/internal/   ->  {repo_url}/tree/{ref}/cmd/gmc/internal

Two kinds of target qualify. Anything escaping `docs_dir` is never a page. So is
a page this build's own scope drops: publication is per version (Q558), so a
`design/` page linking `../plan/milestone-4.md` resolves on `dev` and 404s on
every release (Q561). MkDocs reports the latter below warning level whatever
`validation` says, so no gate can catch it — hence deciding it here, per build,
from the file set.

`{ref}` is `$GAG_DOCS_SOURCE_REF`, defaulting to `main`; `.github/workflows/
pages.yml` sets it to the tag when deploying a release, so each version links to
the source it documents.

Both link syntaxes are covered — inline `](target)` and the reference-style
`[label]: target` definition Python-Markdown resolves into one.

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

# The destination of a reference-style definition — `[label]: target`, up to
# CommonMark's three leading spaces, with any `"title"` left after the match.
# Python-Markdown resolves these into ordinary links, so a target skipped here
# ships exactly as dead as an inline one; the label group is preserved verbatim.
_REF_TARGET = re.compile(r"(?m)^( {0,3}\[[^\]\n]+\]:[ \t]+)(\S+)")

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
    # `published` is deliberately not seeded here: on_files always runs before a
    # page renders, and an empty default would quietly absolutize every in-docs
    # link instead of raising.
    return config


def on_files(files, config):
    """Record which `docs/` paths this build actually serves.

    Two ways a file under `docs/` ends up with no page. `exclude_docs` keeps it
    in `files` marked `InclusionLevel.EXCLUDED`, so membership proves nothing —
    the inclusion level is what has to be read. `README.md` is dropped outright
    where an `index.md` shares the directory, so it is absent instead.
    """
    config["extra"]["source_links"]["published"] = frozenset(
        file.src_uri for file in files if file.inclusion.is_included()
    )
    return files


def rewrite(markdown, page_dir, repo_root, docs_prefix, base, ref, published):
    """Return markdown with every unserved relative target absolutized."""

    def absolutize(target):
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
        if resolved.startswith(".."):  # outside the repo entirely — no URL to build
            return target
        on_disk = os.path.join(repo_root, resolved.replace("/", os.sep))
        if not os.path.exists(on_disk):
            return target
        # Under docs/, MkDocs serves the target unless this build's scope drops
        # it. Directories are left to the link gates either way: a bare `dir/`
        # resolves to that directory's page, which the tree cannot answer for.
        if resolved == docs_prefix or resolved.startswith(docs_prefix + "/"):
            if os.path.isdir(on_disk) or resolved[len(docs_prefix) + 1 :] in published:
                return target
        kind = "tree" if os.path.isdir(on_disk) else "blob"
        url = f"{base}/{kind}/{ref}/{resolved}"
        return f"{url}#{fragment}" if fragment else url

    markdown = _TARGET.sub(lambda m: absolutize(m.group(1)), markdown)
    return _REF_TARGET.sub(lambda m: m.group(1) + absolutize(m.group(2)), markdown)


def on_page_markdown(markdown, page, config, **_kwargs):
    """Rewrite this page's source links before MkDocs resolves them."""
    return rewrite(
        markdown,
        posixpath.dirname(page.file.src_uri),
        **config["extra"]["source_links"],
    )
