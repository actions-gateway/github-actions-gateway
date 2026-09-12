#!/usr/bin/env python3
"""Fail when a prose tier claim drifts from the canonical section it paraphrases (Q848).

`docs/why-gag.md` summarises boundaries that `docs/operations/` states canonically, and
a summary of a tier boundary goes stale in a way no existing gate sees. Measured
2026-08-14: `why-gag.md` called the Pending-reap re-run classic-only for the whole
`v1.4.0` cycle after Q766 had made it reach both tiers. The paragraph's upkeep comment
asked the author to re-read it when a case was *added or removed*, and Q766 neither
added nor removed one, so the instruction never fired. Q776 gates the metric half of
tier drift and the roadmap checker gates the `gag-tier-badge`; prose had nothing.

The rule is a stamp, not a semantic judgement. A paragraph making a tier claim carries

    <!-- tier-source: <path>#<anchor> sha=<8 hex> -->

and this gate recomputes the digest over the **tier-bearing lines** of the section that
annotation names. A tier move upstream changes those lines, so the digest changes and
the gate fails, telling the author to re-read the paraphrase and re-stamp it.

That is deliberately the narrowest trigger that catches the measured failure. A case
added or removed without touching a tier word does not fire it, because the upkeep
comment already asks for that and a gate firing on every edit to a long section would
be re-stamped without reading. Rewording a non-tier line does not fire it either.

Both directions are checked, because a one-directional rule is how the badge checker
ended up unable to see the case that recurs:

1. **A stamp must be current.** Its digest matches the source section's tier lines now.
2. **A tier claim must be stamped.** A paragraph on a watched page that makes a tier
   claim with no `tier-source` annotation is a finding, so deleting the stamp cannot
   quietly turn the gate off.

Usage:
    check-tier-claims.py [--write] [page.md...]

`--write` re-stamps every annotation from the current source, for use after the
paraphrase has actually been re-read. With no page named, the watched pages are used.

Exit status: 0 clean, 1 on any finding, 2 when a watched page or a named source section
is missing, or when a page yielded no paragraph at all — each of which would otherwise
pass by checking nothing.
"""

from __future__ import annotations

import argparse
import hashlib
import pathlib
import re
import subprocess
import sys

# The pages that summarise a tier boundary stated canonically elsewhere. Named rather
# than discovered, for the reason check-card-bullets.sh names its two: a third page
# adopting the pattern should be a deliberate edit here, not a silent enrolment.
WATCHED = ("docs/why-gag.md",)

# A tier claim. Matching the vocabulary rather than parsing meaning is the whole
# design: the gate's job is to notice that a tier sentence exists and that the source
# it came from still says the same thing.
TIER_RE = re.compile(
    r"\bboth (?:acquisition )?tiers\b"
    r"|\bclassic[- ]only\b"
    r"|\bScaleSet[- ]only\b"
    r"|\bscale-set[- ]only\b"
    r"|\b(?:classic|scale-set|ScaleSet) tier\b",
    re.I,
)
STAMP_RE = re.compile(r"<!--\s*tier-source:\s*(\S+?)#(\S+?)\s+sha=([0-9a-f]{8})\s*-->")
COMMENT_RE = re.compile(r"<!--.*?-->", re.S)
HEADING_RE = re.compile(r"^(#{1,6})\s+(.*)$")

DIGEST_LEN = 8


class Refusal(Exception):
    """The gate cannot bear a verdict: its source or anchor is not there.

    Raised rather than counted as a finding, because the two mean different things
    to a caller. A finding says the docs drifted; a refusal says the gate's own
    wiring has come undone, and reporting that as clean is how a check silently
    stops checking.
    """


def repo_root() -> pathlib.Path:
    out = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"],
        capture_output=True,
        text=True,
        check=False,
    )
    return pathlib.Path(out.stdout.strip()) if out.returncode == 0 else pathlib.Path.cwd()


def slug(heading: str) -> str:
    """GitHub's heading-slug algorithm, as check-doc-links.sh resolves anchors."""
    s = re.sub(r"`([^`]*)`", r"\1", heading)
    s = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", s)
    s = re.sub(r"[*_]", "", s).strip().lower()
    s = re.sub(r"[^a-z0-9 _-]", "", s)
    return s.replace(" ", "-")


def section_lines(doc: str, anchor: str) -> list[str] | None:
    """Return the body lines of the section whose heading slugs to anchor."""
    lines = doc.splitlines()
    start = depth = None
    for i, line in enumerate(lines):
        m = HEADING_RE.match(line)
        if not m:
            continue
        if start is None:
            if slug(m.group(2)) == anchor:
                start, depth = i + 1, len(m.group(1))
            continue
        # A heading at the same level or shallower ends the section.
        if len(m.group(1)) <= depth:
            return lines[start:i]
    return None if start is None else lines[start:]


def tier_digest(lines: list[str]) -> str:
    """Digest the section's tier-bearing lines, normalised for whitespace only."""
    bearing = [" ".join(l.split()) for l in lines if TIER_RE.search(l)]
    h = hashlib.sha256("\n".join(bearing).encode()).hexdigest()
    return h[:DIGEST_LEN]


def paragraphs(doc: str) -> list[tuple[int, str]]:
    """Split into blank-line-separated blocks, each with its 1-based start line.

    Fenced code is skipped whole: a tier word in a command is not a claim.
    """
    out: list[tuple[int, str]] = []
    buf: list[str] = []
    start = 1
    fence = False
    for i, line in enumerate(doc.splitlines(), 1):
        if line.lstrip().startswith("```"):
            fence = not fence
        if not fence and not line.strip():
            if buf:
                out.append((start, "\n".join(buf)))
                buf = []
            start = i + 1
            continue
        if not buf:
            start = i
        buf.append("" if fence else line)
    if buf:
        out.append((start, "\n".join(buf)))
    return out


def check_page(root: pathlib.Path, page: pathlib.Path, write: bool) -> tuple[int, bool]:
    """Check one page. Returns (findings, rewritten)."""
    rel = page.relative_to(root)
    doc = page.read_text()
    blocks = paragraphs(doc)
    if not blocks:
        raise Refusal(f"{rel}: no paragraphs found, so this gate would check nothing")

    findings = 0
    rewrites: list[tuple[str, str]] = []

    # A stamp governs the block it sits in and the block that follows it, since an
    # upkeep comment is conventionally written directly above its paragraph and may
    # or may not have a blank line between them.
    for idx, (line, block) in enumerate(blocks):
        stamp = STAMP_RE.search(block)
        prev_stamped = idx > 0 and STAMP_RE.search(blocks[idx - 1][1]) is not None

        if stamp:
            src_path, anchor, want = stamp.group(1), stamp.group(2), stamp.group(3)
            src = root / src_path
            if not src.is_file():
                raise Refusal(f"{rel}:{line}: tier-source names {src_path}, which does not exist")
            lines = section_lines(src.read_text(), anchor)
            if lines is None:
                raise Refusal(
                    f"{rel}:{line}: tier-source names {src_path}#{anchor}, which matches no heading"
                )
            got = tier_digest(lines)
            if got != want:
                if write:
                    rewrites.append((stamp.group(0), STAMP_RE.sub(
                        lambda _m: f"<!-- tier-source: {src_path}#{anchor} sha={got} -->",
                        stamp.group(0))))
                else:
                    findings += 1
                    print(
                        f"{rel}:{line}: the tier claims in {src_path}#{anchor} have changed "
                        f"(stamp {want}, now {got}). Re-read this paragraph against that "
                        f"section, then re-stamp with `make tier-claims-check-write`."
                    )
            continue

        # Rule 2: an unstamped tier claim. The claim is looked for in the block's
        # PROSE, with HTML comments stripped: an upkeep comment and the paragraph it
        # governs are one block whenever no blank line separates them, and exempting
        # any block that opens with a comment would swallow exactly the paragraph
        # this gate exists to watch — which it did, silently, on the first run.
        prose = COMMENT_RE.sub("", block)
        if TIER_RE.search(prose) and not prev_stamped:
            findings += 1
            print(
                f"{rel}:{line}: makes an acquisition-tier claim with no `tier-source` "
                f"annotation, so nothing reconciles it against the section it paraphrases."
            )

    if rewrites:
        for old, new in rewrites:
            doc = doc.replace(old, new)
        page.write_text(doc)
        print(f"{rel}: re-stamped {len(rewrites)} tier-source annotation(s)")
        return findings, True
    return findings, False


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--write", action="store_true", help="re-stamp from the current source")
    ap.add_argument("pages", nargs="*", help="pages to check (default: the watched set)")
    args = ap.parse_args()

    root = repo_root()
    names = args.pages or WATCHED
    pages = [root / n for n in names]
    for p, n in zip(pages, names):
        if not p.is_file():
            print(f"check-tier-claims: {n} does not exist, so this gate would check nothing", file=sys.stderr)
            return 2

    findings = 0
    for page in pages:
        try:
            n, _ = check_page(root, page, args.write)
        except Refusal as exc:
            print(f"check-tier-claims: {exc}", file=sys.stderr)
            return 2
        findings += n

    if findings:
        print(f"\ncheck-tier-claims: {findings} finding(s)", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
