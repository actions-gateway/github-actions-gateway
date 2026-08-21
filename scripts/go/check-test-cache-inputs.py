#!/usr/bin/env python3
"""check-test-cache-inputs.py — cached tests may not read outside their module root.

Go's test-result cache keys a run on the files the test opened, but it drops
every `open`/`stat` whose path resolves outside the package's *module* root
(cmd/go/internal/test: "Do not recheck files outside the module, GOPATH, or
GOROOT root"). A unit test asserting against a repo file one level up is
therefore invisible to the cache key: change the file alone and `go test`
replays a cached pass.

Measured 2026-08-17 (Q895): `make check` reported `pipedgate (cached)` and
exited 0 while the package run directly failed 5 assertions, as did CI. That
package has since been retired, so the live instances are the ones below. The
same shape silently disarmed the root-Dockerfile runner-version lockstep gate —
bumping the pinned tag left `cmd/agc/names` cached and green, which is the
drift #197 introduced arriving through the gate written to catch it.

The fix is a committed symlink under the package's own testdata/, so the read
lands inside the module root and the file becomes a real cache input. This gate
fails when a cached test reaches out directly instead.

Scope is every tier `go test` caches — the unit tier and the `integration` tier
alike. Measured 2026-08-18 (Q902) on cmd/gmc/internal/controller/integration:
warm, `(cached)`, then a renamed spec.runnerGroup property in the out-of-module
api/config/crd still replayed `(cached)` and green, while -count=1 over the same
tree failed. Files behind //go:build e2e are skipped, because `ginkgo run`
compiles with `go test -c` and execs the binary itself, so that tier consults no
test-result cache at all.

The sweep above reads path *spellings*, so it is blind to a root a test derives
at runtime — `os.Getwd()` or `runtime.Caller(0)` walked up to a marker file.
Such a test carries no `".."` anywhere and escapes the module root regardless.
Measured 2026-08-21 (Q936) on cmd/probe/compat, which walks up to go.work and
then shells out to `go list` across every workspace module: warm, `(cached)`,
then an `_ "net/http/httptest"` import added to cmd/proxy's package main still
replayed `(cached)` and exited 0, while -count=1 over the same tree failed. A
subprocess's reads never reach the testlog at all, so no path rewrite can key
that run — which is why the second detector's fix is -count=1 rather than a
symlink.

Usage: check-test-cache-inputs.py [--list|--list-derivations|--uncached-packages]
  --list                Print every escaping read found, allowlisted or not.
  --list-derivations    Print every runtime root derivation found, exempt or not.
  --uncached-packages   Print the package dirs go-test.sh must run with -count=1.
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

# Packages whose result cannot be cache-keyed at all, so go-test.sh runs them
# with -count=1. This is an escape from the detector below only in the sense
# that the defect is fixed elsewhere — the entry is what makes the run honest,
# not what excuses it. Keep it small: -count=1 busts a whole package.
UNCACHED = {
    "cmd/probe/compat":
        "walks up to go.work and shells out to `go list` per workspace module; "
        "subprocess reads never reach the testlog, so nothing can key the run",
}

# Runtime root derivations that are not the defect, each with its judgement.
# Use this when the derived path provably stays inside the module root, so the
# reads are keyed normally and -count=1 would buy nothing.
DERIV_ALLOW = {}

E2E_OUT = re.compile(r"^//go:build\b.*\be2e\b", re.M)
# A base directory computed at runtime rather than written as a literal.
ROOT_DERIV = re.compile(r"\bos\.Getwd\(|\bruntime\.Caller\(")
# ...fed into path construction. Both together are the shape; runtime.Caller on
# its own is the ordinary line-number idiom of a test helper and reads nothing.
PATH_BUILD = re.compile(r"\bfilepath\.(?:Join|Dir)\(")
STRING_LIT = re.compile(r'"((?:[^"\\]|\\.)*)"')
JOIN_CALL = re.compile(r"filepath\.Join\(([^()]*)\)", re.S)


def tracked_tests():
    """Every tracked _test.go outside vendor/, whatever tier builds it."""
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


def root_derivation(src):
    """Whether src computes a base directory at runtime and builds paths from it.

    Returns the matched idiom, or "". Such a path carries no ".." for the sweep
    above to find, and where it walks out of the module root the reads it drives
    are dropped from the cache key exactly as a literal's would be.
    """
    m = ROOT_DERIV.search(src)
    if not m or not PATH_BUILD.search(src):
        return ""
    return m.group(0).rstrip("(")


def main():
    args = sys.argv[1:]
    findings, listed = [], []
    derivations, deriv_findings = [], []

    if "--uncached-packages" in args:
        for pkg in sorted(UNCACHED):
            print(pkg)
        return 0

    for path in tracked_tests():
        src = open(path, encoding="utf-8").read()
        if E2E_OUT.search(src):
            continue
        for shown, target in escaping_reads(path, src):
            listed.append((path, shown, target))
            if (path, shown) not in ALLOW:
                findings.append((path, shown, target))
        idiom = root_derivation(src)
        if idiom:
            pkgdir = os.path.dirname(path)
            derivations.append((path, idiom))
            if pkgdir not in UNCACHED and path not in DERIV_ALLOW:
                deriv_findings.append((path, idiom))

    if "--list-derivations" in args:
        for path, idiom in derivations:
            pkgdir = os.path.dirname(path)
            note = UNCACHED.get(pkgdir) or DERIV_ALLOW.get(path, "")
            print(f"{path}\t{idiom}\t{note}")
        return 0

    if "--list" in args:
        for path, shown, target in listed:
            note = ALLOW.get((path, shown), "")
            print(f"{path}\t{shown}\t-> {target or '<repo root>'}\t{note}")
        return 0

    if deriv_findings:
        print("Tests derive a base directory at runtime and build paths from it.", file=sys.stderr)
        print("No \"..\" literal marks these, and a walk that leaves the module root", file=sys.stderr)
        print("is dropped from the cache key just the same — they replay stale:", file=sys.stderr)
        print("", file=sys.stderr)
        for path, idiom in deriv_findings:
            print(f"  {path}", file=sys.stderr)
            print(f"      derives its root from {idiom}()", file=sys.stderr)
        print("", file=sys.stderr)
        print("Fix: if the derived path stays inside the module root, say so in", file=sys.stderr)
        print("DERIV_ALLOW. If it cannot be keyed at all — a walk to a repo-root", file=sys.stderr)
        print("marker, or reads done by a subprocess — add the package dir to", file=sys.stderr)
        print("UNCACHED, which is what makes go-test.sh run it with -count=1.", file=sys.stderr)
        print("Background: docs/development/testing.md#the-out-of-module-test-read-gate", file=sys.stderr)

    if not findings:
        if deriv_findings:
            return 1
        print(
            f"ok: no cached test reads outside its module root "
            f"({len(ALLOW)} allowlisted, {len(UNCACHED)} package(s) forced uncached)"
        )
        return 0

    if deriv_findings:
        print("", file=sys.stderr)
    print("Tests read files outside their module root, which go's", file=sys.stderr)
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
