#!/usr/bin/env python3
"""Fail when a make target named in prose exists in no Makefile (Q1012).

`make next-task` was taught by CLAUDE.md and had no rule behind it, so a session
following the entrypoint the repo names ran `make -n next-task` and got
`No rule to make target`. The repo already gates the neighbouring classes:
`doc-links` for hrefs, `check-script-docs.sh` for scripts missing a README row,
`gate-lists-check` for the gate registry. A named-but-absent target is the same
family and was the only one unguarded.

Three decisions, each measured while the gate was written:

**The target set is the union of every repo Makefile**, not the root one.
`deploy`, `undeploy` and `deepcopy` are real targets in `api/Makefile` and the
`cmd/*/Makefile`s, and docs name them bare; checking only the root Makefile
flags 16 of 23 such mentions wrongly, which is a gate nobody would keep.
Targets come from `make -qp`, which resolves includes and pattern rules where
grepping recipe lines does not. `-qp` exits non-zero by design when a target is
out of date, so its status is not a signal and is discarded.

**A mention counts only at command position**, meaning the start of a code span
or of a line inside a fenced block, or straight after `&&`, `||`, `;` or `|`.
Prose is full of "make sure", "make room" and "would make the worker and infra
allowlists intersect"; scanning prose for `make <word>` returns 73 hits on
"the" alone. Anchoring to command position takes the 1,181 real invocations in
the tree and leaves the English behind.

**`docs/plan/` is out of scope.** Plan docs are point-in-time records of what
was run, not instructions, so a target that has since been renamed is not a
defect there. Scoping them in would mean archiving a plan is what clears a
finding, which is not a repair.

A deliberate negative mention ("there is no `make next-task`") is a real shape
and the gate cannot tell it from a defect, so a line carrying the marker
`<!-- make-targets-check: ignore -->` is skipped and the reason sits beside it.

Usage: check-make-targets.py [--root DIR] [file.md ...]
"""

import os
import re
import subprocess
import sys

# The repo's own Makefiles. Vendored trees carry dozens more whose targets are
# nobody's to name here.
MAKEFILE_DIRS = (".", "api", "cmd/agc", "cmd/gmc")

EXCLUDED_PREFIXES = ("vendor/", "tools/vendor/", "devtools/vendor/", "docs/plan/")

IGNORE_MARKER = "<!-- make-targets-check: ignore -->"

# Flags that consume the following word, so it is not the target.
FLAG_WITH_ARG = frozenset(("-C", "-f", "-j", "-o", "-W", "-I", "--directory",
                           "--file", "--makefile", "--jobs", "--assume-old",
                           "--assume-new", "--include-dir"))

# The repo's target naming: lowercase words joined by hyphens. This is what
# separates a target from a file target (`.build/mdreflow`), a prose shorthand
# for two of them (`third-party-notices(-check)`) and an ellipsis placeholder.
TARGET_RE = re.compile(r"^[a-z][a-z0-9-]*$")

# A line of `make -qp` output that declares a target. `:=` and `::=` are
# variable assignments and must not be read as one.
DB_TARGET_RE = re.compile(r"^([A-Za-z0-9.][A-Za-z0-9._/-]*):(?!=)")

CODE_SPAN_RE = re.compile(r"`([^`\n]+)`")
COMMAND_SPLIT_RE = re.compile(r"(?:&&|\|\||[;|])")
FENCE_RE = re.compile(r"^\s*(?:```|~~~)")
PROMPT_RE = re.compile(r"^\s*(?:\$\s*|sudo\s+)")


def make_targets(root):
    """Every target declared by any repo Makefile, as one set."""
    targets = set()
    for rel in MAKEFILE_DIRS:
        directory = os.path.join(root, rel)
        if not os.path.isfile(os.path.join(directory, "Makefile")):
            continue
        # -qp exits 1 when a target is out of date, which is not a failure here.
        proc = subprocess.run(["make", "-qp"], cwd=directory, check=False,
                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        for line in proc.stdout.decode("utf-8", "replace").splitlines():
            match = DB_TARGET_RE.match(line)
            if match:
                targets.add(match.group(1))
    return targets


def invoked_target(command):
    """The target a `make ...` command names, or None if it names none."""
    tokens = command.split()
    if not tokens or tokens[0] != "make":
        return None
    i = 1
    while i < len(tokens):
        token = tokens[i]
        if token in FLAG_WITH_ARG:
            i += 2
            continue
        if token.startswith("-") or "=" in token:
            i += 1
            continue
        return token
    return None


def invocations(path):
    """Yield (line number, target) for every make invocation at command position."""
    in_fence = False
    with open(path, encoding="utf-8", errors="replace") as handle:
        for lineno, line in enumerate(handle, 1):
            if FENCE_RE.match(line):
                in_fence = not in_fence
                continue
            if IGNORE_MARKER in line:
                continue
            fragments = []
            if in_fence:
                if line.lstrip().startswith("#"):
                    continue
                fragments.append(PROMPT_RE.sub("", line).split("#")[0])
            else:
                fragments.extend(CODE_SPAN_RE.findall(line))
            for fragment in fragments:
                for piece in COMMAND_SPLIT_RE.split(fragment):
                    target = invoked_target(piece.strip())
                    if target is not None:
                        yield lineno, target


def tracked_markdown(root):
    out = subprocess.run(["git", "ls-files", "*.md"], cwd=root, check=True,
                         stdout=subprocess.PIPE).stdout.decode()
    return [f for f in out.split() if not f.startswith(EXCLUDED_PREFIXES)]


def main(argv):
    root = "."
    files = []
    args = list(argv)
    while args:
        arg = args.pop(0)
        if arg == "--root":
            root = args.pop(0)
        elif arg == "--":
            files.extend(args)
            break
        elif arg.startswith("-"):
            sys.stderr.write("check-make-targets.py: unknown argument: %s\n" % arg)
            return 2
        else:
            files.append(arg)

    targets = make_targets(root)
    if not targets:
        sys.stderr.write("check-make-targets: no Makefile targets found under %s; "
                         "the gate would pass by checking nothing\n" % root)
        return 2

    if not files:
        files = tracked_markdown(root)

    findings = []
    scanned = 0
    for rel in files:
        path = os.path.join(root, rel)
        if not os.path.isfile(path):
            continue
        for lineno, target in invocations(path):
            if not TARGET_RE.match(target):
                continue
            scanned += 1
            if target not in targets:
                findings.append((rel, lineno, target))

    for rel, lineno, target in findings:
        print("%s:%d: `make %s` names no target in any Makefile" % (rel, lineno, target))
    if findings:
        print("check-make-targets: %d finding(s) over %d invocation(s) in %d file(s)"
              % (len(findings), scanned, len(files)))
        return 1
    print("check-make-targets: ok (%d invocation(s) in %d file(s), %d target(s) known)"
          % (scanned, len(files), len(targets)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
