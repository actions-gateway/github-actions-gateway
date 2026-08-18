#!/usr/bin/env python3
"""check-test-cache-inputs.py — unit tests may not read outside their module root.

Go's test-result cache keys a run on the files the test opened, but it drops
every `open`/`stat` whose path resolves outside the package's *module* root
(cmd/go/internal/test: "Do not recheck files outside the module, GOPATH, or
GOROOT root"). A unit test asserting against a repo file one level up is
therefore invisible to the cache key: change the file alone and `go test`
replays a cached pass.

Measured 2026-08-17 (Q895): `make check` reported `pipedgate (cached)` and
exited 0 while the package run directly failed 5 assertions, as did CI. The
same shape silently disarmed the root-Dockerfile runner-version lockstep gate —
bumping the pinned tag left `cmd/agc/names` cached and green, which is the
drift #197 introduced arriving through the gate written to catch it.

The fix is a committed symlink under the package's own testdata/, so the read
lands inside the module root and the file becomes a real cache input. This gate
fails when a unit test reaches out directly instead.

Scope is the cached unit tier. Files behind //go:build integration or e2e are
skipped: those tiers have the same defect but a different invocation, tracked
as Q902.

Usage: check-test-cache-inputs.py [--list]
  --list  Print every escaping read found, allowlisted or not, and exit 0.
"""

import os
import re
import subprocess
import sys

# Escaping reads that are not the defect, each with the judgement that says why.
# A lexical sweep cannot tell a path literal that is read from one that is data,
# so "this one does not count" is stated rather than assumed.
ALLOW = {
    ("cmd/worker/worker_test.go", "../../etc/passwd"):
        "a map key in a path-traversal fixture; nothing opens it",
}

TAGGED_OUT = re.compile(r"^//go:build\b.*\b(integration|e2e)\b", re.M)
STRING_LIT = re.compile(r'"((?:[^"\\]|\\.)*)"')
JOIN_CALL = re.compile(r"filepath\.Join\(([^()]*)\)", re.S)


def tracked_unit_tests():
    """Every tracked _test.go outside vendor/ that the cached unit tier builds."""
    out = subprocess.run(
        ["git", "ls-files", "*_test.go"], capture_output=True, text=True, check=True
    ).stdout.split()
    return [f for f in out if "/vendor/" not in f and not f.startswith("vendor/")]


def module_root(path):
    """The nearest ancestor directory holding a go.mod — go's cache boundary."""
    d = os.path.dirname(path)
    while True:
        if os.path.exists(os.path.join(d, "go.mod")):
            return d
        parent = os.path.dirname(d)
        if parent == d:
            return ""
        d = parent


def escaping_reads(path, src):
    """Relative path literals in src that resolve outside path's module root.

    Covers both spellings: a plain "../x" literal, and filepath.Join("..", "x"),
    whose leading segments are separate arguments and so match no single literal.
    """
    root, pkgdir = module_root(path), os.path.dirname(path)
    found = []

    def check(rel, shown):
        if not rel.startswith(".."):
            return
        target = os.path.normpath(os.path.join(pkgdir, rel))
        if not (target == root or target.startswith(root + os.sep)):
            found.append((shown, target))

    for call in JOIN_CALL.finditer(src):
        parts = STRING_LIT.findall(call.group(1))
        if parts and parts[0] == "..":
            check("/".join(parts), "/".join(parts))

    for lit in STRING_LIT.findall(src):
        if lit.startswith("../"):
            check(lit, lit)

    return found


def main():
    listing = "--list" in sys.argv[1:]
    findings, listed = [], []

    for path in tracked_unit_tests():
        src = open(path, encoding="utf-8").read()
        if TAGGED_OUT.search(src):
            continue
        for shown, target in escaping_reads(path, src):
            listed.append((path, shown, target))
            if (path, shown) not in ALLOW:
                findings.append((path, shown, target))

    if listing:
        for path, shown, target in listed:
            note = ALLOW.get((path, shown), "")
            print(f"{path}\t{shown}\t-> {target or '<repo root>'}\t{note}")
        return 0

    if not findings:
        print(f"ok: no unit test reads outside its module root ({len(ALLOW)} allowlisted)")
        return 0

    print("Unit tests read files outside their module root, which go's", file=sys.stderr)
    print("test cache drops from the key — these assertions replay stale:", file=sys.stderr)
    print("", file=sys.stderr)
    for path, shown, target in findings:
        print(f"  {path}", file=sys.stderr)
        print(f"      reads {shown}  ->  {target or '<repo root>'}", file=sys.stderr)
    print("", file=sys.stderr)
    print("Fix: commit a symlink under the package's own testdata/ pointing at the", file=sys.stderr)
    print("file, and read through it, so the read resolves inside the module root:", file=sys.stderr)
    print("", file=sys.stderr)
    print("    ln -s ../../../../Dockerfile cmd/agc/names/testdata/Dockerfile", file=sys.stderr)
    print('    const dockerfilePath = "testdata/Dockerfile"', file=sys.stderr)
    print("", file=sys.stderr)
    print("If the literal is data rather than a file read, add it to ALLOW in", file=sys.stderr)
    print("scripts/go/check-test-cache-inputs.py with the reason.", file=sys.stderr)
    print("Background: docs/development/testing.md#the-out-of-module-test-read-gate", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
