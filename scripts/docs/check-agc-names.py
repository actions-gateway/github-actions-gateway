#!/usr/bin/env python3
"""Hold every AGC Deployment reference in the operator docs to a stated API version (Q1098).

The AGC is one Deployment per namespace under v1, named `actions-gateway-controller`,
and one per gateway under v2, named `<gateway>-agc` from `AGCResourceSuffix`. A command
written with the v1 name names nothing on a v2 tenant, and a reader on the recommended
path gets no signal that it does not apply to them. Thirty such references across eight
pages were measured on 2026-09-12, of which one was correct — a page applying a v1 CR —
and four were bare-name forms (`get deploy … <name>`) that Q1098's own `deploy/` pattern
could not see.

Three rules, all functions of the tree alone:

1. **Every v1 AGC Deployment reference is version-labelled.** A `deploy/…`,
   `deployment/…` or `get deploy … <name>` reference to `actions-gateway-controller`
   must carry `v1` on its own line or on one of the two preceding non-blank lines —
   the `# v1 (legacy)` comment the split blocks use, or prose saying the page is v1.
   The window is deliberately short: a label further away than that is not what a
   reader skimming to a command sees.

2. **The v2 suffix the docs spell matches the one the GMC builds.** Any
   `<gateway>-agc` in the docs is checked against `AGCResourceSuffix` in the GMC
   builder, so renaming the suffix in code cannot leave the docs silently wrong.

3. **Every v1 AGC `app=` selector is version-labelled** (Q1099). The bare `app` label
   carries `<gateway>-agc` under v2 and the fixed name under v1, so
   `-l app=actions-gateway-controller` selects nothing on a v2 tenant. Unlike a
   Deployment name, this one has a version-neutral answer: `app.kubernetes.io/name`
   is `actions-gateway-controller` under both, pinned by
   `TestAGCPodSelectorIsVersionNeutral`. So the finding names that remedy rather than
   a v2/v1 split, which is only right where the selector is what a NetworkPolicy
   matches on and the recommended label will not do.

The version label is deliberately not `\bv1\b`: an upstream version string
(`kindest/node:v1.35.0`) would satisfy that while telling a reader nothing about which
API version the command is for. `v1alpha1` does count, since a page applying a v1 CR is
v1-scoped by that fact.

Scope is the pages an operator or contributor follows: `docs/operations/`,
`docs/development/` and `docs/getting-started.md`. Design docs describe v1's
NetworkPolicy and label set as design, and archived plans are history; neither is a
command anyone runs, so neither is in scope.

Exit status: 0 clean, 1 on any finding, 2 when the scope resolved to no files or the
suffix constant could not be read, either of which would pass by checking nothing.
"""

from __future__ import annotations

import pathlib
import re
import subprocess
import sys

V1_NAME = "actions-gateway-controller"

# A Deployment reference: `deploy/NAME` and `deployment/NAME` (plural tolerated), or
# `deploy NAME` after a verb, with any flags between. A bare NAME elsewhere is a label
# value, a ServiceAccount or prose, none of which this rule speaks for.
DEP_RE = re.compile(
    r"deploy(?:ment)?s?/" + V1_NAME
    + r"|\bdeploy(?:ment)?s?\b(?:\s+-[^\s]+(?:[= ]\S+)?)*\s+" + V1_NAME
)
V1_LABEL_RE = re.compile(r"\bv1(?:alpha\d+)?\b(?!\.\d)")
# A pod/Deployment selector on the bare `app` label. `app.kubernetes.io/name=` does
# not match: the character before `=` is `e`, not the end of a bare `app`.
APP_SELECTOR_RE = re.compile(r"(?<![\w./-])app=" + V1_NAME)
SUFFIX_RE = re.compile(r'AGCResourceSuffix\s*=\s*"([^"]+)"')
# The other per-gateway suffixes the same builder mints. They are not Deployment
# names, but they are spelled `<gateway>-…` in the docs the same way, and several
# of them start with `-agc`, so rule 2 has to know them to leave them alone.
SIBLING_SUFFIX_RE = re.compile(r'agc[A-Za-z]*Suffix\s*=\s*"([^"]+)"')

BUILDER = pathlib.Path("cmd/gmc/internal/controller/actionsgateway_v2_builder.go")
SCOPE = ("docs/operations", "docs/development", "docs/getting-started.md")

# How far back a version label may sit and still be the one a reader sees.
LABEL_WINDOW = 2


def repo_root() -> pathlib.Path:
    out = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"],
        capture_output=True,
        text=True,
        check=False,
    )
    return pathlib.Path(out.stdout.strip()) if out.returncode == 0 else pathlib.Path.cwd()


def scope_files(root: pathlib.Path) -> list[pathlib.Path]:
    files: list[pathlib.Path] = []
    for entry in SCOPE:
        p = root / entry
        if p.is_dir():
            files.extend(sorted(p.rglob("*.md")))
        elif p.is_file():
            files.append(p)
    return [f for f in files if not f.is_symlink()]


def agc_suffix(root: pathlib.Path) -> tuple[str, set[str]] | None:
    """Return the AGC Deployment suffix and every sibling suffix the builder mints."""
    path = root / BUILDER
    if not path.is_file():
        return None
    text = path.read_text()
    m = SUFFIX_RE.search(text)
    if not m:
        return None
    return m.group(1), {m.group(1), *SIBLING_SUFFIX_RE.findall(text)}


def unlabelled(lines: list[str], pattern: re.Pattern[str]) -> list[tuple[int, str]]:
    """Return every match of `pattern` with no version label in its window."""
    found = []
    for i, line in enumerate(lines):
        if not pattern.search(line):
            continue
        preceding = [x for x in lines[:i] if x.strip()][-LABEL_WINDOW:]
        if any(V1_LABEL_RE.search(w) for w in [line, *preceding]):
            continue
        found.append((i + 1, line.strip()))
    return found


def main() -> int:
    root = repo_root()
    files = scope_files(root)
    if not files:
        print("check-agc-names: no docs in scope, so this gate would check nothing", file=sys.stderr)
        return 2

    suffixes = agc_suffix(root)
    if suffixes is None:
        print(
            f"check-agc-names: could not read AGCResourceSuffix from {BUILDER}, "
            "so the v2 name cannot be checked",
            file=sys.stderr,
        )
        return 2
    suffix, known = suffixes
    v2_form = f"<gateway>{suffix}"

    findings = 0
    for f in files:
        rel = f.relative_to(root)
        lines = f.read_text().splitlines()
        for lineno, text in unlabelled(lines, DEP_RE):
            findings += 1
            print(
                f"{rel}:{lineno}: AGC Deployment named for v1 with no version label "
                f"nearby — give it a `# v1 (legacy)` sibling and the v2 form "
                f"`{v2_form}`: {text}"
            )
        for lineno, text in unlabelled(lines, APP_SELECTOR_RE):
            findings += 1
            print(
                f"{rel}:{lineno}: `app={V1_NAME}` selects nothing on a v2 tenant, "
                f"where the label carries `{v2_form}`. Prefer "
                f"`app.kubernetes.io/name={V1_NAME}`, which is correct under both; "
                f"where a NetworkPolicy selector is the subject and only the bare "
                f"label will do, state the version nearby: {text}"
            )
        for i, line in enumerate(lines, 1):
            for m in re.finditer(r"<gateway>(-[a-z0-9-]+)", line):
                # A suffix the builder mints is correct whichever resource it names;
                # anything else beginning `-agc` is a misspelling of the Deployment.
                # A prefix of one counts as that one: the docs write a brace
                # expansion (`<gateway>-agc-metrics-{tls,client}`) for the pair.
                if (
                    m.group(1).startswith("-agc")
                    and not any(s.startswith(m.group(1)) for s in known)
                ):
                    findings += 1
                    print(
                        f"{rel}:{i}: v2 AGC name uses `{m.group(0)}` but "
                        f"AGCResourceSuffix is `{suffix}`"
                    )

    if findings:
        print(f"\ncheck-agc-names: {findings} finding(s)", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
