#!/usr/bin/env python3
"""Fail when a published page is wider than the viewport it is read in.

The checker behind scripts/docs/check-render-overflow.sh, which provisions the
browser and builds the site; this serves that build and measures it. See that
script's header for why the gate exists at all.

What it asserts is absolute rather than budgeted: `scrollWidth > clientWidth` on
the document element means the reader gets a horizontal scrollbar, which is a
defect at any width and needs no threshold chosen from the current site. That is
the whole reason this gate can hold a render while the density rules next door
stay proxies over the Markdown (website.md § Card bullets fit on one line).

It has to be a real browser. The pills that motivated the gate are built by
docs/javascripts/extra.js at page load from a `> **Audience:**` blockquote, so
they exist in no built HTML file and no Markdown-AST gate can reach them.

Each finding names the `article` child that owns the overflow, found by hiding
each in turn and watching scrollWidth drop. The largest drop wins: overflow is a
maximum and not a sum, so a page can have one owner masking another, and the
owner reported is the one to fix first.

Exits 0 clean, 1 on any finding, and 2 when it measured nothing -- a gate that
passes by checking no pages reads exactly like a clean one.
"""

import argparse
import asyncio
import contextlib
import functools
import os
import sys
import threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

# Hide each article child in turn; the one whose removal shrinks the document
# most is the owner. Returns {over: 0} for a clean page so the caller can tell
# "measured and clean" from "not measured".
OWNER_JS = """() => {
  const de = document.documentElement;
  const over = de.scrollWidth - de.clientWidth;
  if (over <= 0) return {over: 0};
  const art = document.querySelector('article') || document.body;
  let best = null;
  for (const el of art.children) {
    const w0 = de.scrollWidth, prev = el.style.display;
    el.style.display = 'none';
    const drop = w0 - de.scrollWidth;
    el.style.display = prev;
    if (drop > 0 && (!best || drop > best[1])) {
      best = [(el.className || el.tagName).toString().trim(), drop];
    }
  }
  return {over, owner: best};
}"""


class _QuietHandler(SimpleHTTPRequestHandler):
    """SimpleHTTPRequestHandler without the per-request log line."""

    def log_message(self, fmt, *args):
        pass


@contextlib.contextmanager
def serve(root: Path):
    """Serve `root` over HTTP on an ephemeral port, yielding its base URL.

    Over HTTP rather than `file://` because the two do not agree: measured
    2026-09-14, the same landing page reported 279px of overflow under
    `file://` and none when served, so the cheaper oracle invents findings.
    """
    handler = functools.partial(_QuietHandler, directory=str(root))
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{httpd.server_address[1]}"
    finally:
        httpd.shutdown()
        httpd.server_close()


def discover(root: Path) -> list[str]:
    """URL paths for every page under a built site, as the site serves them."""
    pages = []
    for html in sorted(root.rglob("index.html")):
        rel = html.relative_to(root).parent.as_posix()
        pages.append("/" if rel == "." else f"/{rel}/")
    return pages


async def measure(base: str, pages: list[str], widths: list[int]) -> tuple[list[dict], int]:
    from playwright.async_api import async_playwright

    findings: list[dict] = []
    measured = 0
    async with async_playwright() as pw:
        browser = await pw.chromium.launch()
        # One page reused across every URL and width: navigating once and
        # resizing is ~5x faster than a fresh navigation per width, and a
        # viewport resize is a real one, so `vw`-sized elements re-resolve
        # (website.md warns that resizing a *parent* does not).
        ctx = await browser.new_context(viewport={"width": max(widths), "height": 900})
        page = await ctx.new_page()
        for path in pages:
            await page.goto(base + path, wait_until="load")
            for width in widths:
                await page.set_viewport_size({"width": width, "height": 900})
                result = await page.evaluate(OWNER_JS)
                measured += 1
                if result["over"] > 0:
                    findings.append({"page": path, "width": width, **result})
        await browser.close()
    return findings, measured


def report(findings: list[dict], pages: int, measured: int) -> None:
    annotate = bool(os.environ.get("GITHUB_ACTIONS"))
    for f in findings:
        owner = (f.get("owner") or ["unknown", 0])[0]
        msg = (
            f"{f['page']} is {f['over']}px wider than a {f['width']}px viewport, "
            f"so the page scrolls sideways (owner: .{owner})"
        )
        if annotate:
            print(f"::error::check-render-overflow: {msg}")
        else:
            print(f"check-render-overflow: {msg}", file=sys.stderr)
    verdict = "FAILED" if findings else "ok"
    print(
        f"check-render-overflow: {verdict} "
        f"({pages} pages, {measured} measurements, {len(findings)} finding(s))"
    )


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", required=True, type=Path, help="a built site dir, served by this script")
    ap.add_argument("--widths", default="320,1440", help="comma-separated viewport widths")
    args = ap.parse_args()

    widths = [int(w) for w in args.widths.split(",")]
    pages = discover(args.root)
    if not pages:
        print(
            f"check-render-overflow: no pages found under {args.root}, so nothing was checked.",
            file=sys.stderr,
        )
        return 2

    with serve(args.root) as base:
        findings, measured = asyncio.run(measure(base, pages, widths))
    report(findings, len(pages), measured)
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
