#!/usr/bin/env python3
"""check-dashboard-render.py — a dashboard change must ship its screenshot (Q868).

deploy/monitoring/preview/README.md tells a contributor to re-render and copy
the PNG into docs/assets/ after changing a dashboard, and nothing enforced it.
#1526 added a series to the platform dashboard's fleet-conditions panel with no
render, so the published screenshot lost that series and read as the current
dashboard for weeks. The drift is silent by construction: the page still shows a
plausible dashboard, and only somebody holding the JSON open can tell.

Two properties a naive gate gets wrong, both from the row:

  Judge the PR's whole diff, not a commit. A branch that lands the JSON in one
  commit and the PNG in a later one is a legitimate split, and a per-commit
  check fails it. The baseline is therefore the merge base with origin/main —
  never its tip, so a dashboard `main` changed while this branch was behind is
  not read as this branch's work.

  A description-only edit is exempt. A panel `description` is an info-icon
  tooltip that no screenshot carries, so demanding a kind cluster and a
  fifteen-minute render for one is friction with nothing behind it. #1531
  changed exactly one description string. The exemption is semantic rather than
  textual: both sides are parsed and compared with every `description` key
  removed at any depth, so it survives reformatting and cannot be widened by a
  hunk that merely looks description-shaped.

HEAD is the subject, not the working tree, like the sibling branch-shaped gate
in scripts/docs/check-queue-rules.py: the failure this exists to catch is a
merged PR, and the PR's verdict is the one that counts.

Exit: 0 the renders are in step, 1 a dashboard changed without one, 2 a read
that could not be taken.
"""

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path

DASHBOARD_DIR = "deploy/monitoring"
DASHBOARD_PREFIX = "grafana-dashboard-"
ASSET_DIR = "docs/assets"
README = "deploy/monitoring/preview/README.md"
OVERRIDE = "DASHBOARD_ALLOW_STALE_RENDER"


class Unreadable(Exception):
    """A git or parse read that did not happen. Never a verdict."""


def git(args):
    p = subprocess.run(["git"] + args, capture_output=True, text=True)
    return p.returncode, p.stdout, p.stderr


def merge_base():
    rc, out, err = git(["merge-base", "HEAD", "origin/main"])
    if rc != 0:
        raise Unreadable(f"the merge base with origin/main: {err.strip()}")
    return out.strip()


def blob_oid(ref, path):
    """The blob's object id at ref, or None when the path is absent there."""
    rc, out, _ = git(["rev-parse", f"{ref}:{path}"])
    return out.strip() if rc == 0 else None


def dashboards_at(ref):
    """The dashboard paths at ref, or [] when the directory has none."""
    rc, out, _ = git(["ls-tree", "-r", "--name-only", ref, "--", DASHBOARD_DIR])
    if rc != 0:
        raise Unreadable(f"{DASHBOARD_DIR} at {ref}")
    return sorted(
        p for p in out.splitlines()
        if Path(p).name.startswith(DASHBOARD_PREFIX) and p.endswith(".json")
    )


def undescribed(ref, path):
    """The dashboard at ref with every `description` removed, canonicalized."""
    rc, out, err = git(["show", f"{ref}:{path}"])
    if rc != 0:
        raise Unreadable(f"{path} at {ref}: {err.strip()}")
    try:
        doc = json.loads(out)
    except json.JSONDecodeError as e:
        raise Unreadable(f"{path} at {ref} as JSON: {e}") from e
    return json.dumps(strip_descriptions(doc), sort_keys=True)


def strip_descriptions(node):
    if isinstance(node, dict):
        return {k: strip_descriptions(v) for k, v in node.items()
                if k != "description"}
    if isinstance(node, list):
        return [strip_descriptions(v) for v in node]
    return node


def excused():
    raw = os.environ.get(OVERRIDE, "").replace(",", " ")
    return {s for s in raw.split() if s}


def check(base, head, failures):
    allowed = excused()
    checked = 0
    found = dashboards_at(head)
    if not found:
        # A selection that matches nothing passes every dashboard in silence,
        # which is the one failure this gate cannot report on itself. The
        # dashboards are shipped artifacts, so an empty match means they moved
        # or were renamed out from under the glob, not that there is nothing
        # to check.
        raise Unreadable(
            f"any {DASHBOARD_PREFIX}*.json under {DASHBOARD_DIR} at {head}; "
            f"the selection matched nothing, so every dashboard would pass "
            f"unchecked")
    for path in found:
        name = Path(path).name
        asset = f"{ASSET_DIR}/{Path(path).stem}.png"
        head_json, base_json = blob_oid(head, path), blob_oid(base, path)
        if head_json == base_json:
            continue
        checked += 1
        # Parsed unconditionally, so a dashboard with no version at the base is
        # refused for the same malformed JSON that refuses one with a version
        # there. Hanging the parse off the exemption would make the refusal a
        # side effect of which branch happened to run.
        head_shape = undescribed(head, path)
        if base_json is not None and undescribed(base, path) == head_shape:
            continue
        if name in allowed:
            print(f"note: {name} changed with no new render, excused by "
                  f"{OVERRIDE}")
            continue
        head_png, base_png = blob_oid(head, asset), blob_oid(base, asset)
        if head_png is None:
            failures.append(
                f"{path} changed and {asset} does not exist. The docs embed one "
                f"screenshot per dashboard, named for it; render this one and "
                f"commit it — see {README}.")
        elif head_png == base_png:
            failures.append(
                f"{path} changed beyond its panel descriptions and {asset} did "
                f"not, so the published screenshot still shows the old panels — "
                f"the drift #1526 shipped. Re-render with "
                f"`deploy/monitoring/preview/render.sh shot` and copy the PNG "
                f"per {README}. A change that provably cannot alter the render "
                f"sets {OVERRIDE}={name}.")
    return checked


def main(argv=None):
    ap = argparse.ArgumentParser(add_help=True, description=__doc__)
    ap.add_argument("--base", help="baseline ref (default: merge base with origin/main)")
    ap.add_argument("--head", default="HEAD", help="ref under test (default: HEAD)")
    args = ap.parse_args(argv)

    try:
        base = args.base or merge_base()
        failures = []
        checked = check(base, args.head, failures)
    except Unreadable as e:
        print(f"check-dashboard-render: could not read {e}; refusing to guess",
              file=sys.stderr)
        return 2

    for f in failures:
        print(f)
    if failures:
        print(f"check-dashboard-render: FAILED - {len(failures)} finding(s)")
        return 1
    print(f"check-dashboard-render: ok ({checked} dashboard(s) changed vs "
          f"{base[:12]})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
