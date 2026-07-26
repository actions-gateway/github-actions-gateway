"""MkDocs hook that derives the release the docs-site announce bar advertises.

The announce bar in `overrides/main.html` used to hard-code "vX.Y.Z is here",
which made bumping it a manual release step that every stable tag missed:
`v1.0.0` shipped saying "Alpha, pre-1.0", and `v1.1.0` and `v1.2.0` both said
"v1.0.0 is here". A stable tag deploys that tag's docs wholesale, so a stale
banner is published permanently under that version and no later fix to `main`
reaches it (Q393).

This hook resolves the version instead, and exposes it to the template as
`config.extra.release.version`:

1. `$GAG_DOCS_RELEASE`, when set. An escape hatch for builds with no git history
   (a source tarball) and for exercising the template by hand.
2. Otherwise the highest stable `vX.Y.Z` git tag. "Stable" matches the test
   `publish.yml` and `pages.yml` already share: a SemVer core of `0.x`, or any
   `-` suffix (`-rc`/`-alpha`/`-beta`), is a prerelease.
3. Otherwise the empty string, which renders a version-free banner rather than a
   wrong one.

Resolving the NEWEST release, rather than the version being built, is
deliberate: the announce bar is site chrome, and a visitor reading an older
version's docs wants to know what the current release is. `pages.yml` pins
`overrides/` to the current checkout for the same reason.

Wired in `mkdocs.yml` under `hooks:`. Run it directly to see what the banner will
name without building the site:

    python3 hooks/release_version.py

See docs/development/website.md, "The announce bar".
"""

from __future__ import annotations

import os
import re
import subprocess
import sys

# A stable release tag: `vX.Y.Z` with a non-zero major and no prerelease suffix.
_STABLE_TAG = re.compile(r"^v([1-9][0-9]*)\.([0-9]+)\.([0-9]+)$")


def _from_git(repo_dir):
    """Return the highest stable `vX.Y.Z` tag in repo_dir, or "" if there is none.

    Any git failure (no repository, no git binary, a shallow clone with no tags)
    is not an error: the caller falls back to a version-free banner.
    """
    try:
        completed = subprocess.run(
            ["git", "-C", repo_dir, "tag", "--list", "v*"],
            capture_output=True,
            text=True,
            timeout=30,
            check=True,
        )
    except (OSError, subprocess.SubprocessError):
        return ""

    best_key = None
    best_tag = ""
    for tag in completed.stdout.split():
        match = _STABLE_TAG.match(tag)
        if match is None:
            continue
        # Compare numerically, not lexically: `v1.10.0` sorts below `v1.9.0` as
        # a string.
        key = tuple(int(part) for part in match.groups())
        if best_key is None or key > best_key:
            best_key, best_tag = key, tag
    return best_tag


def resolve(repo_dir):
    """Return the release the announce bar should name, or "" if there is none."""
    return os.environ.get("GAG_DOCS_RELEASE", "").strip() or _from_git(repo_dir)


def on_config(config):
    """Populate `config.extra.release.version` before any page is rendered."""
    repo_dir = os.path.dirname(os.path.abspath(config["config_file_path"]))
    config["extra"].setdefault("release", {})["version"] = resolve(repo_dir)
    return config


if __name__ == "__main__":
    print(resolve(sys.argv[1] if len(sys.argv) > 1 else os.getcwd()))
