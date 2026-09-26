#!/usr/bin/env bash
#
# Unit tests for hooks/offline_links.py — the MkDocs hook that points raw-HTML
# directory links at a page's .html file when the offline export turns directory
# URLs off. A missed link opens a folder listing from disk; a link rewritten when
# it names no page hides the break from the export's own link check.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# python3 is an extended-tier prerequisite (scripts/ci/check-tools.sh), not a
# required one, so this skips rather than fails when it is absent. CI runners
# always have it.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
readonly REPO_ROOT
readonly HOOK="$REPO_ROOT/hooks/offline_links.py"

if ! command -v python3 >/dev/null 2>&1; then
	printf 'skip offline-links-hook-test: python3 not found (extended tier, scripts/ci/check-tools.sh)\n'
	exit 0
fi

fails=0

# The published pages, as src_uri -> output URL with directory URLs off.
readonly PAGES='index.md=index.html why-gag.md=why-gag.html operations/index.md=operations/index.html operations/foo.md=operations/foo.html design/README.md=design/index.html'

# rewrite PAGE_SRC HTML — run the hook's pure rewrite over HTML as page PAGE_SRC.
rewrite() {
	python3 - "$HOOK" "$PAGES" "$1" "$2" <<-'PY'
		import importlib.util, sys

		hook, pages, page_src, html = sys.argv[1:5]
		spec = importlib.util.spec_from_file_location("offline_links", hook)
		mod = importlib.util.module_from_spec(spec)
		spec.loader.exec_module(mod)
		urls = dict(p.split("=") for p in pages.split())
		by_dir = {mod.directory_url(src): url for src, url in urls.items()}
		sys.stdout.write(mod.rewrite(html, page_src, urls[page_src], by_dir))
	PY
}

# expect NAME WANT PAGE_SRC HTML
expect() {
	local name="$1" want="$2" got
	got="$(rewrite "$3" "$4")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s:\n  want=%q\n   got=%q\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# --- rewritten: a relative directory link naming a published page ------------

expect 'sibling section, fragment kept' \
	'<a href="operations/foo.html#bar">' why-gag.md '<a href="../operations/foo/#bar">'
expect 'section index page' \
	'<a href="operations/index.html">' index.md '<a href="operations/">'
expect 'from a section index, one level up' \
	'<a href="../why-gag.html">' operations/index.md '<a href="../why-gag/">'
expect 'from a section index, same section' \
	'<a href="foo.html">' operations/index.md '<a href="foo/">'
expect 'README-backed section index' \
	'<a href="design/index.html">' why-gag.md '<a href="../design/">'
expect 'site root' \
	'<a href="index.html">' why-gag.md '<a href="../">'

# --- left alone ---------------------------------------------------------------

# The export's link check reports these; rewriting would hide the break.
expect 'directory naming no page' \
	'<a href="../nope/">' why-gag.md '<a href="../nope/">'
expect 'already a file' \
	'<a href="operations/foo.html">' why-gag.md '<a href="operations/foo.html">'
expect 'absolute URL' \
	'<a href="https://example.test/operations/foo/">' why-gag.md '<a href="https://example.test/operations/foo/">'
expect 'root-absolute path' \
	'<a href="/operations/foo/">' why-gag.md '<a href="/operations/foo/">'
expect 'bare fragment' \
	'<a href="#operations/">' why-gag.md '<a href="#operations/">'

if ((fails > 0)); then
	printf '\n%d offline-links-hook assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\noffline-links-hook-test: all assertions passed\n'
