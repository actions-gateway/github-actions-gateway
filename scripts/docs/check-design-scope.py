#!/usr/bin/env python3
"""Fail a branch that states an operator-visible scope in docs/design/ and nowhere else (Q774).

[doc-update-matrix.md](../../docs/development/doc-update-matrix.md) calls this "the
classic miss" in so many words — "A design-doc-only update is the classic miss: the
operator who hits the rejection never reads `docs/design/`" — and nothing enforced it.
The 1.4 cycle missed it three times.

**The trigger is a scope phrase this branch ADDED, not the file pair.** Keying on "a
`docs/design/` file changed, so a `docs/operations/` file must too" would fire on every
typo fix and be waived into meaninglessness within a week. Keying on the sentence that
states something an operator can trip fires only when there is an operator surface to
propagate, which is the case the matrix row is about.

The vocabulary is calibrated rather than guessed. Measured 2026-09-12 over the whole
`docs/design/` tree: 65 lines match, across 10 of its files — dense enough that the
patterns find real statements, sparse enough that a branch adding one has genuinely
added a claim about operator-visible behaviour. Two categories carry all 65:

  * **admission-reject** (29) — a request the API server or a webhook refuses. The
    operator meets it as an error on `kubectl apply`, and the matrix row names the two
    places it has to be written down: a troubleshooting runbook keyed on the exact
    message, and the usage doc for the action that now gets rejected.
  * **default-change** (36) — what the system does when the operator says nothing. A
    changed default is the one change that reaches every existing tenant without anyone
    editing a manifest.

Three further categories were drafted and cut, because each matched zero lines in the
corpus: a pattern never validated against real prose is a guess about how this project
writes, and a gate is a bad place to keep one. Add one here when a real sentence needs
it, with the count that justified it.

**The escape is inline and reviewable**, following `no-plan-refs`: a line carrying
`operator-surface: <reason>` is silenced, and only that line. A design doc restating a
rejection an operations page already documents is the legitimate case, and it should
show up in the diff rather than being covered by a whole-file allowlist.

Base resolution and the fail-open posture follow `check-em-dash.sh`: no merge-base means
a shallow clone, and a gate that reddened every PR whenever its inputs were missing
would be switched off. `DESIGN_SCOPE_REQUIRE_BASE=1` turns that skip into a hard error
for a caller that knows it arranged a base.

Usage:
    check-design-scope.py [--base REV]

Exit status: 0 clean, 1 when the branch adds a scope statement and touches no operator
doc, 2 when a base was required and could not be resolved.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys

DESIGN_DIR = "docs/design/"
OPERATOR_DIR = "docs/operations/"

# Calibrated over docs/design/ on 2026-09-12; see the module docstring for the counts
# and for why three further categories were cut.
SCOPE_PATTERNS = {
    "admission-reject": re.compile(
        r"\b(is rejected|are rejected|rejects the|admission (?:rejects|rejection)"
        r"|denied by admission)\b",
        re.I,
    ),
    "default-change": re.compile(r"\b(defaults? to|the default is|by default,)\b", re.I),
}
EXEMPT_RE = re.compile(r"operator-surface:\s*\S")


def git(*args: str) -> tuple[int, str]:
    p = subprocess.run(["git", *args], capture_output=True, text=True, check=False)
    return p.returncode, p.stdout


def resolve_base(explicit: str | None) -> str | None:
    if explicit:
        return explicit
    rc, out = git("merge-base", "HEAD", "origin/main")
    return out.strip() if rc == 0 and out.strip() else None


def added_lines(base: str, path: str) -> list[tuple[int, str]]:
    """Return (line number in the new file, text) for every line this branch added."""
    rc, out = git("diff", "--unified=0", base, "--", path)
    if rc != 0:
        return []
    added: list[tuple[int, str]] = []
    lineno = 0
    for line in out.splitlines():
        m = re.match(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@", line)
        if m:
            lineno = int(m.group(1))
            continue
        if line.startswith("+++") or line.startswith("---"):
            continue
        if line.startswith("+"):
            added.append((lineno, line[1:]))
            lineno += 1
        elif not line.startswith("-"):
            lineno += 1
    return added


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", help="revision to diff against, in place of the merge-base")
    args = ap.parse_args()

    rc, root = git("rev-parse", "--show-toplevel")
    if rc == 0 and root.strip():
        os.chdir(root.strip())

    base = resolve_base(args.base)
    if base is None:
        msg = "check-design-scope: no merge-base with origin/main resolved"
        if os.environ.get("DESIGN_SCOPE_REQUIRE_BASE") == "1":
            print(f"{msg}, and a base was required", file=sys.stderr)
            return 2
        print(f"{msg}; skipping (shallow clone?)", file=sys.stderr)
        return 0

    rc, out = git("diff", "--name-only", base)
    changed = [f for f in out.splitlines() if f]
    design = [f for f in changed if f.startswith(DESIGN_DIR)]
    touched_operator = any(f.startswith(OPERATOR_DIR) for f in changed)

    findings: list[tuple[str, int, str, str]] = []
    for path in design:
        for lineno, text in added_lines(base, path):
            if EXEMPT_RE.search(text):
                continue
            for kind, pat in SCOPE_PATTERNS.items():
                if pat.search(text):
                    findings.append((path, lineno, kind, text.strip()))
                    break

    if not findings:
        print(
            f"check-design-scope: ok ({len(design)} design file(s) changed, "
            f"no operator-visible scope statement added)"
        )
        return 0

    if touched_operator:
        print(
            f"check-design-scope: ok ({len(findings)} scope statement(s) added under "
            f"{DESIGN_DIR}, and this branch updates {OPERATOR_DIR} too)"
        )
        return 0

    print(
        f"check-design-scope: this branch adds {len(findings)} operator-visible scope "
        f"statement(s) under {DESIGN_DIR} and changes nothing under {OPERATOR_DIR}.",
        file=sys.stderr,
    )
    print(
        "A design-doc-only update is the classic miss: the operator who hits the "
        "rejection never reads docs/design/. See "
        "docs/development/doc-update-matrix.md. Write the operator half — a "
        "troubleshooting runbook keyed on the exact message, and the usage doc for "
        "the action that changed — or, where an operations page already says this, "
        "mark the line `operator-surface: <reason>`.",
        file=sys.stderr,
    )
    for path, lineno, kind, text in findings:
        print(f"  {path}:{lineno}: [{kind}] {text[:120]}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
