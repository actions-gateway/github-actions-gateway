#!/usr/bin/env python3
"""Fail a branch that adds a comment line carrying provenance instead of meaning.

docs/development/code-comments.md sends four things out of a comment and into the
commit message, the pull request, or docs/: a backlog ID, a pull request number, a
date, and wording that narrates a change. This gate reads the comment lines a branch
ADDED and fails on those four. Existing comments stay legal until a branch touches
them, so there is no baseline file.

Comments are found by lexing each file, not by searching for the comment marker: a
`//` inside a Go string and a `#` inside a shell string, a heredoc, or a quoted YAML
scalar are text, and a workflow `run:` block is lexed as the shell it is. Text inside
a string is never checked, which is also the gate's blind spot: an awk or Python
program embedded in a shell string is not read.

Allowed forms:
    TODO(Q123):                   open work, removed by the PR that closes it
    owner/repo#123, issue URLs    an upstream reference, which resolves
    a date beside `measured`      the upstream-behavior citation that
    or a version                  documentation-standards.md requires
    comment-provenance: <reason>  silences that line, visibly in the diff

Usage:
    check-comment-provenance.py [--base REV] [--report] [--all]

--report prints every finding as `rule<TAB>path:line<TAB>text` and exits 0. --all reads
every comment in the tree instead of the diff, which with --report sizes a burn-down.

Exit status: 0 clean, 1 on a finding, 2 when COMMENT_PROVENANCE_REQUIRE_BASE=1 and no
merge-base with origin/main resolves.
"""

from __future__ import annotations

import argparse
import ast
import io
import os
import re
import subprocess
import sys
import tokenize

EXCLUDED_DIRS = ("vendor/", "testdata/", "third_party/", "node_modules/")
GENERATED_RE = re.compile(r"(^|/)zz_generated[^/]*\.go$|\.pb\.go$")
GENERATED_HEADER_RE = re.compile(
    r"^// Code generated .* DO NOT EDIT\.$|controller-gen\.kubebuilder\.io/version", re.M
)

EXEMPT_RE = re.compile(r"comment-provenance:\s*\S")
TODO_RE = re.compile(r"\bTODO\(Q\d+\)")
VERSION_RE = re.compile(r"\bv?\d+\.\d+\.\d+\b|@[0-9a-f]{7,40}\b")
MEASURED_RE = re.compile(r"\bmeasured\b", re.I)

BACKLOG_ID_RE = re.compile(r"\bQ\d{2,4}\b")
# The lookbehind keeps `owner/repo#123` and a URL fragment legal.
PR_NUMBER_RE = re.compile(r"(?<![\w/&])#\d{2,5}\b|\bPR\s+#?\d{2,5}\b")
# The lookarounds drop a date that is part of a path or a timestamp.
DATE_RE = re.compile(
    r"(?<![\w/-])20\d{2}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])(?![\w-])"
)
# The one narration phrase precise enough to gate. The lookbehinds and the
# lowercase match drop its purpose sense ("is used to", "Used to key the cache").
NARRATION_RE = re.compile(
    r"(?<!\bis )(?<!\bare )(?<!\bwas )(?<!\bwere )(?<!\bbe )(?<!\bbeen )(?<!\bbeing )"
    r"\bused to\b"
)


def git(*args: str) -> tuple[int, str]:
    p = subprocess.run(["git", *args], capture_output=True, text=True, check=False)
    return p.returncode, p.stdout


def language(path: str) -> str | None:
    """Return the lexer a path is read with, or None when it is out of scope."""
    if any(path.startswith(d) or f"/{d}" in path for d in EXCLUDED_DIRS):
        return None
    if GENERATED_RE.search(path):
        return None
    base = os.path.basename(path)
    if base.endswith(".go"):
        return "go"
    if base.endswith(".sh"):
        return "shell"
    if base.endswith(".py"):
        return "python"
    if base.endswith((".yml", ".yaml")):
        return "yaml"
    if base in ("Makefile", "GNUmakefile") or base.endswith(".mk"):
        return "make"
    return None


def go_comments(src: str) -> list[tuple[int, str]]:
    out: list[tuple[int, str]] = []
    i, n, line = 0, len(src), 1
    while i < n:
        c = src[i]
        two = src[i : i + 2]
        if c == "\n":
            line += 1
            i += 1
        elif two == "//":
            j = src.find("\n", i)
            j = n if j < 0 else j
            out.append((line, src[i + 2 : j]))
            i = j
        elif two == "/*":
            j = src.find("*/", i + 2)
            j = n if j < 0 else j
            for text in src[i + 2 : j].split("\n"):
                out.append((line, text))
                line += 1
            line -= 1
            i = j + 2
        elif c == "`":
            j = src.find("`", i + 1)
            j = n if j < 0 else j
            line += src.count("\n", i, j)
            i = j + 1
        elif c in "\"'":
            i += 1
            while i < n and src[i] != c and src[i] != "\n":
                i += 2 if src[i] == "\\" else 1
            i += 1 if i < n and src[i] == c else 0
        else:
            i += 1
    return out


HEREDOC_RE = re.compile(r"<<(-?)\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\2")


def shell_comments(src: str, first_line: int = 1) -> list[tuple[int, str]]:
    """Lex shell source. A `#` opens a comment only at the start of a word.

    The context stack is what keeps `"$(echo "it's")"` from reading the apostrophe as
    an opening quote: each entry is a quote kind, or the paren depth of a command
    context that a `$(` opened.
    """
    out: list[tuple[int, str]] = []
    lines = src.split("\n")
    stack: list[str | int] = [0]
    idx = 0
    while idx < len(lines):
        text = lines[idx]
        lineno = first_line + idx
        idx += 1
        pending: list[tuple[str, bool]] = []
        arith = 0
        i, n = 0, len(text)
        while i < n:
            c = text[i]
            ctx = stack[-1]
            if ctx == "'":
                if c == "'":
                    stack.pop()
                i += 1
            elif ctx == "$'":
                if c == "'":
                    stack.pop()
                i += 2 if c == "\\" else 1
            elif c == "\\":
                i += 2
            elif ctx == '"':
                if c == '"':
                    stack.pop()
                elif text.startswith("$(", i) and not text.startswith("$((", i):
                    stack.append(0)
                    i += 1
                i += 1
            elif text.startswith("$'", i):
                stack.append("$'")
                i += 2
            elif c in "'\"":
                stack.append(c)
                i += 1
            elif text.startswith("((", i):
                arith += 1
                i += 2
            elif arith and text.startswith("))", i):
                arith -= 1
                i += 2
            elif arith:
                i += 1
            elif text.startswith("$(", i):
                stack.append(0)
                i += 2
            elif c == "(":
                stack[-1] = int(ctx) + 1
                i += 1
            elif c == ")":
                if int(ctx) > 0:
                    stack[-1] = int(ctx) - 1
                elif len(stack) > 1:
                    stack.pop()
                i += 1
            elif c == "#" and (i == 0 or text[i - 1] in " \t;|&()"):
                out.append((lineno, text[i + 1 :]))
                break
            elif text.startswith("<<<", i):
                i += 3
            elif text.startswith("<<", i):
                m = HEREDOC_RE.match(text, i)
                if m:
                    pending.append((m.group(3), m.group(1) == "-"))
                    i = m.end()
                else:
                    i += 2
            else:
                i += 1
        for word, strip_tabs in pending:
            while idx < len(lines):
                body = lines[idx]
                idx += 1
                if (body.lstrip("\t") if strip_tabs else body) == word:
                    break
    return out


def python_comments(src: str) -> list[tuple[int, str]]:
    out: list[tuple[int, str]] = []
    try:
        for tok in tokenize.generate_tokens(io.StringIO(src).readline):
            if tok.type == tokenize.COMMENT:
                out.append((tok.start[0], tok.string[1:]))
        tree = ast.parse(src)
    except (tokenize.TokenError, SyntaxError, IndentationError):
        return out
    for node in ast.walk(tree):
        if not isinstance(
            node, (ast.Module, ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)
        ):
            continue
        body = node.body
        if not (body and isinstance(body[0], ast.Expr)):
            continue
        value = body[0].value
        if isinstance(value, ast.Constant) and isinstance(value.value, str):
            end = value.end_lineno or value.lineno
            for offset, text in enumerate(src.split("\n")[value.lineno - 1 : end]):
                out.append((value.lineno + offset, text))
    return out


def line_comment(text: str) -> str | None:
    """Return the `#` comment on one line, reading quotes on that line alone."""
    quote = ""
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if quote:
            if c == "\\" and quote == '"':
                i += 2
                continue
            if c == quote:
                quote = ""
        elif c == "\\":
            i += 1
        elif c in "'\"":
            quote = c
        elif c == "#" and (i == 0 or text[i - 1] in " \t"):
            return text[i + 1 :]
        i += 1
    return None


RUN_BLOCK_RE = re.compile(r"^(\s*)(?:- +)?run\s*:\s*[|>][+-]?\d*\s*$")


def yaml_comments(src: str) -> list[tuple[int, str]]:
    """Lex YAML line by line, except a `run:` block, which is lexed as shell.

    Other block scalars get the line rule too. Measured over this tree, every
    `#`-leading line inside one is a comment in an embedded language that also
    uses `#`: a paths-filter list, a kustomize patch, a Rego policy.
    """
    out: list[tuple[int, str]] = []
    lines = src.split("\n")
    idx = 0
    while idx < len(lines):
        text = lines[idx]
        comment = line_comment(text)
        if comment is not None:
            out.append((idx + 1, comment))
        m = RUN_BLOCK_RE.match(text)
        idx += 1
        if not m:
            continue
        indent = len(m.group(1))
        start = idx
        while idx < len(lines) and (
            not lines[idx].strip() or len(lines[idx]) - len(lines[idx].lstrip(" ")) > indent
        ):
            idx += 1
        out.extend(shell_comments("\n".join(lines[start:idx]), first_line=start + 1))
    return out


def make_comments(src: str) -> list[tuple[int, str]]:
    out: list[tuple[int, str]] = []
    for lineno, text in enumerate(src.split("\n"), 1):
        comment = line_comment(text)
        if comment is not None:
            out.append((lineno, comment))
    return out


LEXERS = {
    "go": go_comments,
    "shell": shell_comments,
    "python": python_comments,
    "yaml": yaml_comments,
    "make": make_comments,
}


def check_line(lineno: int, by_line: dict[int, str]) -> list[str]:
    """Return the rules the comment on one line breaks."""
    text = by_line[lineno]
    if EXEMPT_RE.search(text):
        return []
    scrubbed = TODO_RE.sub(" ", text)
    rules = []
    if BACKLOG_ID_RE.search(scrubbed):
        rules.append("backlog-id")
    if PR_NUMBER_RE.search(scrubbed):
        rules.append("pr-number")
    if DATE_RE.search(scrubbed) and not VERSION_RE.search(scrubbed):
        # A measurement citation wraps, so the word may sit on a neighbouring line.
        nearby = " ".join(by_line.get(n, "") for n in (lineno - 1, lineno, lineno + 1))
        if not MEASURED_RE.search(nearby):
            rules.append("date")
    if NARRATION_RE.search(scrubbed):
        rules.append("narration")
    return rules


def added_line_numbers(base: str, path: str) -> set[int]:
    rc, out = git("diff", "--unified=0", base, "--", path)
    if rc != 0:
        return set()
    added: set[int] = set()
    for line in out.splitlines():
        m = re.match(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@", line)
        if m:
            start, count = int(m.group(1)), int(m.group(2) or "1")
            added.update(range(start, start + count))
    return added


def resolve_base(explicit: str | None) -> str | None:
    if explicit:
        return explicit
    rc, out = git("merge-base", "HEAD", "origin/main")
    return out.strip() if rc == 0 and out.strip() else None


HELP = {
    "backlog-id": "a backlog ID outside TODO(QNNN): resolves to nothing once the row is deleted",
    "pr-number": "a pull request number belongs in the commit message, where git blame leads; an upstream issue is `owner/repo#123`",
    "date": "a date belongs in the pull request or docs/, unless it cites a measurement: say `measured`, or give the version on the same line",
    "narration": "what the code used to do belongs in the commit message; for the purpose sense write `that scopes`, not `used to scope`",
}


def main() -> int:
    ap = argparse.ArgumentParser(description=(__doc__ or "").split("\n")[0])
    ap.add_argument("--base", help="revision to diff against, in place of the merge-base")
    ap.add_argument("--report", action="store_true", help="print findings as TSV and exit 0")
    ap.add_argument("--all", action="store_true", help="read every comment in the tree, not the diff")
    args = ap.parse_args()

    rc, root = git("rev-parse", "--show-toplevel")
    if rc == 0 and root.strip():
        os.chdir(root.strip())

    new_files: set[str] = set()
    base = ""
    if args.all:
        _, out = git("-c", "core.quotePath=false", "ls-files")
        changed = sorted(f for f in out.splitlines() if f)
    else:
        resolved = resolve_base(args.base)
        if resolved is None:
            msg = "check-comment-provenance: no merge-base with origin/main resolved"
            if os.environ.get("COMMENT_PROVENANCE_REQUIRE_BASE") == "1":
                print(f"{msg}, and a base was required", file=sys.stderr)
                return 2
            print(f"{msg}; skipping (shallow clone?)", file=sys.stderr)
            return 0
        base = resolved
        rc, out = git(
            "-c", "core.quotePath=false", "diff", "--name-only", "--diff-filter=d", base
        )
        if rc != 0:
            print(f"check-comment-provenance: git diff against {base} failed", file=sys.stderr)
            return 2
        _, untracked = git(
            "-c", "core.quotePath=false", "ls-files", "--others", "--exclude-standard"
        )
        new_files = {f for f in untracked.splitlines() if f}
        changed = sorted({f for f in out.splitlines() if f} | new_files)

    def whole_file(path: str) -> bool:
        return args.all or path in new_files

    findings: list[tuple[str, str, int, str]] = []
    scanned = added_comments = 0
    for path in changed:
        lang = language(path)
        if lang is None or os.path.islink(path) or not os.path.isfile(path):
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                src = fh.read()
        except (OSError, UnicodeDecodeError):
            continue
        if GENERATED_HEADER_RE.search(src[:2000]):
            continue
        scanned += 1
        by_line: dict[int, str] = {}
        for lineno, text in LEXERS[lang](src):
            by_line[lineno] = f"{by_line[lineno]} {text}" if lineno in by_line else text
        added = None if whole_file(path) else added_line_numbers(base, path)
        for lineno in sorted(by_line):
            if added is not None and lineno not in added:
                continue
            added_comments += 1
            for rule in check_line(lineno, by_line):
                findings.append((rule, path, lineno, by_line[lineno].strip()))

    if args.report:
        for rule, path, lineno, text in findings:
            print(f"{rule}\t{path}:{lineno}\t{text}")
        print(
            f"check-comment-provenance: {scanned} file(s), {added_comments} added "
            f"comment line(s), {len(findings)} finding(s)",
            file=sys.stderr,
        )
        return 0

    if not findings:
        print(
            f"check-comment-provenance: ok ({scanned} changed source file(s), "
            f"{added_comments} added comment line(s), none carrying provenance)"
        )
        return 0

    print(
        f"check-comment-provenance: this branch adds {len(findings)} comment line(s) "
        "carrying provenance a later reader of the file cannot use:",
        file=sys.stderr,
    )
    for rule, path, lineno, text in findings:
        print(f"  {path}:{lineno}: [{rule}] {text[:120]}", file=sys.stderr)
    print(file=sys.stderr)
    for rule in sorted({f[0] for f in findings}):
        print(f"  {rule}: {HELP[rule]}", file=sys.stderr)
    print(
        "\nMove it to the commit message, the pull request description, or docs/ "
        "(docs/development/code-comments.md#what-goes-somewhere-else). Open work is "
        "`TODO(QNNN):`; a line that must keep the text carries "
        "`comment-provenance: <reason>`.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
