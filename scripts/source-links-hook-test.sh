#!/usr/bin/env bash
#
# Unit tests for hooks/source_links.py — the MkDocs hook that absolutizes
# relative links escaping docs/ against repo_url (Q558). Publishing the
# repo-internal docs on the `dev` site made those links load-bearing: 724 of
# them pointed at source files MkDocs cannot serve. The rewrite has to be exact
# in both directions — a missed link is a 404, and a link rewritten when it
# should not be turns a typo MkDocs would flag into a plausible-looking 404.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# python3 is an extended-tier prerequisite (scripts/check-tools.sh), not a
# required one, so this skips rather than fails when it is absent. CI runners
# always have it.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
readonly REPO_ROOT
readonly HOOK="$REPO_ROOT/hooks/source_links.py"
readonly BASE="https://example.test/o/r"

if ! command -v python3 >/dev/null 2>&1; then
	printf 'skip source-links-hook-test: python3 not found (extended tier, scripts/check-tools.sh)\n'
	exit 0
fi

WORKDIR="$(mktemp -d)"
readonly WORKDIR
trap 'rm -rf "$WORKDIR"' EXIT

# A throwaway tree shaped like the repo: a docs/ dir plus the source paths the
# cases link at. Existence is what decides a rewrite, so the fixture is the
# fixture — nothing about the real checkout leaks in.
mkdir -p "$WORKDIR/docs/plan" "$WORKDIR/cmd/agc" "$WORKDIR/cmd/gmc/internal" "$WORKDIR/.github/workflows"
touch "$WORKDIR/cmd/agc/main.go" "$WORKDIR/Makefile" "$WORKDIR/.github/workflows/ci.yml"
touch "$WORKDIR/docs/plan/sibling.md"

fails=0

# rewrite PAGE_DIR MARKDOWN [REF] — run the hook's pure rewrite over MARKDOWN as
# if it were the page docs/PAGE_DIR/page.md.
rewrite() {
	local page_dir="$1" markdown="$2" ref="${3:-main}"
	python3 - "$HOOK" "$WORKDIR" "$page_dir" "$markdown" "$BASE" "$ref" <<-'PY'
		import importlib.util, sys

		hook, repo_root, page_dir, markdown, base, ref = sys.argv[1:7]
		spec = importlib.util.spec_from_file_location("source_links", hook)
		mod = importlib.util.module_from_spec(spec)
		spec.loader.exec_module(mod)
		sys.stdout.write(mod.rewrite(markdown, page_dir, repo_root=repo_root,
		                             docs_prefix="docs", base=base, ref=ref))
	PY
}

# expect NAME WANT PAGE_DIR MARKDOWN [REF]
expect() {
	local name="$1" want="$2" got
	shift 2
	got="$(rewrite "$@")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s:\n  want=%q\n   got=%q\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# --- rewritten: relative targets that escape docs/ and exist in the tree ------

expect 'source file -> blob URL' \
	"[m]($BASE/blob/main/cmd/agc/main.go)" plan '[m](../../cmd/agc/main.go)'
expect 'file:line -> #L anchor' \
	"[m]($BASE/blob/main/cmd/agc/main.go#L91)" plan '[m](../../cmd/agc/main.go:91)'
expect 'directory -> tree URL' \
	"[c]($BASE/tree/main/cmd/gmc/internal)" plan '[c](../../cmd/gmc/internal)'
expect 'existing fragment is preserved' \
	"[w]($BASE/blob/main/.github/workflows/ci.yml#jobs)" plan '[w](../../.github/workflows/ci.yml#jobs)'
expect 'repo-root file from a top-level page' \
	"[mk]($BASE/blob/main/Makefile)" '' '[mk](../Makefile)'
expect 'ref override pins the version' \
	"[m]($BASE/blob/v1.2.0/cmd/agc/main.go)" plan '[m](../../cmd/agc/main.go)' v1.2.0
expect 'every target in a line is rewritten' \
	"[a]($BASE/blob/main/Makefile) and [b]($BASE/blob/main/cmd/agc/main.go)" \
	plan '[a](../../Makefile) and [b](../../cmd/agc/main.go)'

# --- left alone: MkDocs resolves it, or nothing in the repo backs it ----------

expect 'sibling doc stays relative' \
	'[s](sibling.md)' plan '[s](sibling.md)'
expect 'doc outside this dir but inside docs/ stays relative' \
	'[i](../index.md)' plan '[i](../index.md)'
expect 'absolute URL untouched' \
	'[u](https://example.com/x.go)' plan '[u](https://example.com/x.go)'
expect 'mailto untouched' \
	'[m](mailto:a@b.c)' plan '[m](mailto:a@b.c)'
expect 'site-absolute path untouched' \
	'[a](/stable/index.html)' plan '[a](/stable/index.html)'
expect 'bare anchor untouched' \
	'[a](#section)' plan '[a](#section)'
# A typo must keep failing MkDocs' link check rather than become a 404 URL.
expect 'nonexistent source path untouched' \
	'[x](../../cmd/agc/gone.go)' plan '[x](../../cmd/agc/gone.go)'
expect 'nonexistent path with a line suffix untouched' \
	'[x](../../cmd/agc/gone.go:12)' plan '[x](../../cmd/agc/gone.go:12)'
# normpath escapes the repo entirely — there is no URL to build.
expect 'target above the repo root untouched' \
	'[o](../../../elsewhere.md)' plan '[o](../../../elsewhere.md)'
# --- the scheme test itself --------------------------------------------------
#
# `path/to/file.go:91` is a relative target, not a URL in the `path/to/file.go`
# scheme. Every markdown form of it reaches _is_relative with a `../` prefix, so
# assert the classifier directly rather than through a contrived page layout.
scheme_cases="$(
	python3 - "$HOOK" <<-'PY'
		import importlib.util, sys

		spec = importlib.util.spec_from_file_location("source_links", sys.argv[1])
		mod = importlib.util.module_from_spec(spec)
		spec.loader.exec_module(mod)
		for target, want in [
		    ("Makefile:91", True),
		    ("cmd/agc/main.go:91", True),
		    ("../Makefile", True),
		    ("https://example.com", False),
		    ("mailto:a@b.c", False),
		    ("//cdn.example.com/x", False),
		    ("/stable/", False),
		    ("#anchor", False),
		]:
		    got = mod._is_relative(target)
		    if got is not want:
		        print(f"{target!r}: want {want} got {got}")
	PY
)"
if [[ -z "$scheme_cases" ]]; then
	printf 'ok   %s\n' 'file:line is classified relative, real schemes are not'
else
	printf 'FAIL %s:\n%s\n' 'relative/absolute classification' "$scheme_cases" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\n%d source-links-hook assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nsource-links-hook-test: all assertions passed\n'
