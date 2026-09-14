#!/usr/bin/env bash
#
# check-render-overflow.sh — fail when a published page is wider than the
# viewport it is read in.
#
# website.md § Measure the render has said since 2026-08 that the source cannot
# answer layout questions, and nothing checked any of them: `make check` never
# builds the site, so every docs gate here is a proxy over the Markdown. This is
# the first that holds the render itself.
#
# It gates ONE assertion, deliberately. `scrollWidth > clientWidth` is absolute:
# a page wider than the viewport gives the reader a horizontal scrollbar, which
# is a defect at any width with no threshold to pick. The neighbouring density
# rules (card bullets, stat-tile labels) stay character budgets over the
# Markdown, because "too tall against its siblings" has no such zero point and a
# number invented for it is what gets a gate deleted for crying wolf.
#
# A real browser is the only instrument that can see this class. The persona
# pills that motivated the gate are built by docs/javascripts/extra.js at page
# load from a `> **Audience:**` blockquote, so they are in no built HTML file and
# no Markdown-AST gate can reach them. Measured 2026-09-14 before the fix: two
# published pages over at 320px, operations/migration-from-arc.md by 324px,
# rendering a 320px viewport 644px wide.
#
# Both publication scopes are checked. They differ by 345 pages — the backlog,
# the design docs and the plan tree publish under `dev` only — and a reader uses
# both.
#
# Costs, measured 2026-09-14. Provisioning is ~13s and ~350MB, once, on a
# workstation and on a GitHub runner alike, and is then cached. The sweep is not
# portable: 14s for the 63 public pages and 94s for the 411 `dev` ones on an
# 18-core M5 Max, against 39s and 3m30s on the 2-core ubuntu-latest runner, for a
# CI job of about 5 minutes end to end including both site builds. The `dev`
# sweep is 70% of that, so a future run that needs to be cheaper should narrow
# THAT scope rather than drop a width. It is NOT in `make check`, for the reason
# check-release-links.sh gives: the fast local gate has no business provisioning
# a Python venv, let alone a browser.
#
# Usage:
#   scripts/docs/check-render-overflow.sh [--widths 320,1440] [--scope site|site-dev]
#
# Env:
#   GAG_RENDER_WIDTHS   viewport widths to measure (default 320,1440)
#
# Backs `make render-overflow-check` and the doc-links.yml CI workflow.
#
# It proves its own instrument first, running the planted-fixture half of
# scripts/docs/check-render-overflow-test.sh before it measures the site. That
# half lives here rather than under `make scripts-test` because it needs the
# pinned browser, and a suite that asserts one thing on a machine with a venv
# and another on a machine without one cannot be read. The browser-free half of
# that suite runs under `make scripts-test` as usual.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

readonly venv_dir="${repo_root}/.venv-render"
readonly requirements="${repo_root}/requirements-docs-check.txt"
readonly stamp="${venv_dir}/.requirements.sha256"
# Kept inside the venv so `rm -rf .venv-render` takes the browser with it, and so
# nothing lands in the host-wide ~/.cache that a workspace guard would flag.
readonly browsers_dir="${venv_dir}/browsers"

widths="${GAG_RENDER_WIDTHS:-320,1440}"
scopes=()

while (($# > 0)); do
	case "$1" in
	--widths)
		widths="${2:?--widths needs a value}"
		shift
		;;
	--scope)
		scopes+=("${2:?--scope needs a value}")
		shift
		;;
	*)
		printf 'check-render-overflow: unknown argument %s\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done
((${#scopes[@]} > 0)) || scopes=(site site-dev)

requirements_hash() {
	local file="$1"
	shasum -a 256 "$file" | awk '{print $1}'
}

# Provision .venv-render/ from the pinned requirements, skipping the work when it
# already matches. Mirrors docs-preview.sh's stamp so a pin bump reprovisions and
# an unchanged one costs nothing.
ensure_venv() {
	local have="" want
	want="$(requirements_hash "$requirements")"
	[[ -f "$stamp" ]] && have="$(cat "$stamp")"
	if [[ -x "${venv_dir}/bin/playwright" && "$have" == "$want" ]]; then
		return
	fi
	printf 'check-render-overflow: provisioning venv from %s…\n' "${requirements##*/}"
	python3 -m venv "$venv_dir" \
		|| die "python3 -m venv failed — on Debian/Ubuntu install the python3-venv package"
	"${venv_dir}/bin/pip" install --quiet --upgrade pip
	"${venv_dir}/bin/pip" install --quiet -r "$requirements"
	# headless-shell rather than full chromium: it is the variant Playwright's
	# headless mode drives, and it is the smaller download.
	PLAYWRIGHT_BROWSERS_PATH="$browsers_dir" "${venv_dir}/bin/playwright" install chromium-headless-shell
	printf '%s' "$want" >"$stamp"
}

# A missing site build is built, not skipped past: the render IS the gate, so a
# no-op when it is absent would make a green verdict meaningless.
ensure_site() {
	local scope="$1"
	if [[ ! -f "${scope}/index.html" ]]; then
		printf 'check-render-overflow: %s/ is not built; building both scopes…\n' "$scope"
		scripts/docs/docs-preview.sh build
	fi
	[[ -f "${scope}/index.html" ]] \
		|| die "check-render-overflow: ${scope}/ holds no index.html after a build, so nothing was checked"
}

require_cmd python3 "https://www.python.org/downloads/"
[[ -f "$requirements" ]] || die "check-render-overflow: missing ${requirements}"
ensure_venv

# Prove the instrument on planted fixtures before believing it about the site.
# A browser that cannot see a deliberately over-wide element reports a clean
# sweep exactly like a site that has none, and the browser is the one input here
# that differs between machines. Running this from the gate rather than leaving
# it to `make scripts-test` is what keeps a local green and a CI green the same
# claim: the assertions run wherever the gate runs, instead of wherever a venv
# happens to already exist.
printf 'check-render-overflow: self-test\n'
"$SCRIPT_DIR/check-render-overflow-test.sh" --render-only \
	|| die "check-render-overflow: the self-test failed, so this run's verdict about the site means nothing"

rc=0
for scope in "${scopes[@]}"; do
	ensure_site "$scope"
	printf 'check-render-overflow: %s at widths %s\n' "$scope" "$widths"
	PLAYWRIGHT_BROWSERS_PATH="$browsers_dir" "${venv_dir}/bin/python" \
		"${SCRIPT_DIR}/check-render-overflow.py" --root "$scope" --widths "$widths" \
		|| rc=$?
done
exit "$rc"
