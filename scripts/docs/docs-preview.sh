#!/usr/bin/env bash
# scripts/docs/docs-preview.sh — run the MkDocs site from an isolated venv so the
# pinned docs toolchain never pollutes the host Python.
#
# Usage:
#   scripts/docs/docs-preview.sh serve   # live-reload preview at http://localhost:8000
#   scripts/docs/docs-preview.sh build   # strict build of both scopes: site/, site-dev/
#   scripts/docs/docs-preview.sh export  # offline release-scope site, zipped, in site-export/
#
# The venv lives in .venv-docs/ (gitignored) and is reused across runs; it is
# (re)provisioned only when requirements-docs.txt changes, so the toolchain
# stays exactly pinned (MkDocs 2.0 is incompatible with Material 9.x — see
# docs/development/website.md). python3 is the sole host prerequisite
# (scripts/ci/check-tools.sh, extended tier). On Debian/Ubuntu the stdlib venv
# module ships separately — install python3-venv if `python3 -m venv` fails.
set -euo pipefail
shopt -s inherit_errexit

repo_root="$(git rev-parse --show-toplevel)"
readonly repo_root
readonly venv_dir="${repo_root}/.venv-docs"
readonly requirements="${repo_root}/requirements-docs.txt"
readonly stamp="${venv_dir}/.requirements.sha256"
readonly export_dir="${repo_root}/site-export"
readonly export_name="actions-gateway-site"

die() {
  printf 'docs-preview: %s\n' "$*" >&2
  exit 1
}

# hash prints the sha256 of a file using the already-required python3, so the
# script needs no extra hashing binary (sha256sum vs. shasum differs by OS).
hash() {
  python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$1"
}

# ensure_venv creates or refreshes .venv-docs/ from the pinned requirements,
# skipping the work when the venv already matches the current requirements hash.
ensure_venv() {
  local have="" want
  [[ -f "${stamp}" ]] && have="$(cat "${stamp}")"
  want="$(hash "${requirements}")"

  if [[ -x "${venv_dir}/bin/mkdocs" && "${have}" == "${want}" ]]; then
    return 0
  fi

  printf 'docs-preview: provisioning venv from %s…\n' "${requirements##*/}"
  python3 -m venv "${venv_dir}" \
    || die "python3 -m venv failed — on Debian/Ubuntu install the python3-venv package"
  "${venv_dir}/bin/pip" install --quiet --upgrade pip
  "${venv_dir}/bin/pip" install --quiet -r "${requirements}"
  printf '%s' "${want}" >"${stamp}"
}

# A page opened from disk has no server to turn `foo/` into `foo/index.html`, so
# every local href and src must name a file that exists. mkdocs --strict cannot
# see this: raw-HTML links bypass its resolution (hooks/offline_links.py).
# 404.html is skipped because it is written for a server, with root-absolute paths.
check_export_links() {
  python3 - "$1" <<'PY'
import os, re, sys, urllib.parse

root = sys.argv[1]
broken = []
for d, _, names in os.walk(root):
    for name in names:
        if not name.endswith(".html") or name == "404.html":
            continue
        page = os.path.join(d, name)
        with open(page, encoding="utf-8") as f:
            html = f.read()
        for ref in re.findall(r'\b(?:href|src)="([^"]+)"', html):
            if re.match(r"(?:[A-Za-z][A-Za-z0-9+.-]*:|#)", ref):
                continue
            path = urllib.parse.unquote(ref.split("#")[0].split("?")[0])
            if path and not os.path.isfile(os.path.join(d, path)):
                broken.append(f"{os.path.relpath(page, root)}: {ref}")
if broken:
    print("docs-preview: the export has links that do not open from disk:", file=sys.stderr)
    print("\n".join(f"  {b}" for b in sorted(set(broken))), file=sys.stderr)
    sys.exit(1)
PY
}

main() {
  local cmd="${1:-build}"

  command -v python3 >/dev/null 2>&1 \
    || die "python3 not found — run scripts/ci/check-tools.sh (extended tier)"
  [[ -f "${requirements}" ]] || die "missing ${requirements}"

  case "${cmd}" in
    serve | build | export) ;;
    *) die "unknown command '${cmd}' (expected: serve | build | export)" ;;
  esac

  ensure_venv

  # `serve` honors $PORT (default 8000) so parallel previews don't collide and
  # tooling can inject an auto-assigned port; `build` takes no address. serve
  # stays non-strict: aborting the live-reload loop on a half-written link would
  # make editing unusable.
  if [[ "${cmd}" == "serve" ]]; then
    exec "${venv_dir}/bin/mkdocs" serve --dev-addr "127.0.0.1:${PORT:-8000}"
  fi

  # The zip's top-level entry is ${export_name}/, so unzipping never scatters
  # pages into the current directory.
  if [[ "${cmd}" == "export" ]]; then
    rm -rf "${export_dir}"
    "${venv_dir}/bin/mkdocs" build --strict -f "${repo_root}/mkdocs-offline.yml" \
      --site-dir "${export_dir}/${export_name}"
    check_export_links "${export_dir}/${export_name}"
    python3 -c 'import shutil,sys; shutil.make_archive(sys.argv[1], "zip", sys.argv[2], sys.argv[3])' \
      "${export_dir}/${export_name}" "${export_dir}" "${export_name}"
    printf 'docs-preview: open %s/%s/index.html, or share %s/%s.zip\n' \
      "${export_dir}" "${export_name}" "${export_dir}" "${export_name}"
    return 0
  fi

  # --strict fails on the link/anchor warnings mkdocs.yml's `validation` block
  # raises (Q560), and on a published page listed in neither `nav` nor
  # `not_in_nav` (Q563, `validation.nav.omitted_files`). Both scopes matter for
  # that one: a page's nav coverage is per build, so a dev-only page can only be
  # caught by the second build.
  # Both publication scopes are built, matching pages.yml's PR
  # gate: the release scope excludes docs/plan/ and docs/development/, so a
  # break in those pages only ever surfaces in the dev scope.
  "${venv_dir}/bin/mkdocs" build --strict
  # Must match the dev override in pages.yml: /releases/ holds GitHub Release
  # bodies, excluded from every version including dev (mkdocs.yml says why).
  MKDOCS_EXCLUDE_DOCS=$'/README.md\n/releases/\n' \
    "${venv_dir}/bin/mkdocs" build --strict --site-dir site-dev
}

main "$@"
