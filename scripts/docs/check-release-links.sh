#!/usr/bin/env bash
#
# check-release-links.sh — resolve a release note's absolute site links against a
# local docs-site build (Q636).
#
# Release notes are the one doc whose links are all absolute. They point into the
# versioned site (https://actions-gateway.com/X.Y.Z/…) so an operator reading the
# notes for a release gets that release's instructions, and docs/releases/ is
# excluded from every site version, so a relative link would fail
# `mkdocs build --strict` instead. check-doc-links.sh skips external URLs by
# design, so nothing checked them.
#
# The oracle is a local `site/` build, never the network: a gate that fetches URLs
# fails when a third party sneezes. `mkdocs build` lays the release publication
# scope out exactly as the site serves it, so `…/1.3.0/operations/upgrade/#gmc-rollback`
# resolves to `site/operations/upgrade/index.html` carrying `id="gmc-rollback"`.
#
# Three scope decisions, each load-bearing:
#
#   1. Only the site host is resolved. A github.com or third-party URL has no
#      local oracle; those are counted and reported, never failed. That is also
#      why check-doc-links.sh keeps its external-URL exclusion — "external" there
#      means "unresolvable", and these are resolvable only because the site is
#      built from this same tree.
#
#   2. Only ONE version is resolvable: the newest note in docs/releases/. The
#      build comes from the working tree, which is the tip of development after
#      that tag — a faithful oracle for the notes being authored or amended, and
#      a wrong one for a frozen older release whose pages have since moved. Links
#      naming any other version are skipped and the skip is printed with its
#      count, because a gate that quietly checks nothing reads exactly like a
#      clean one.
#
#   3. A missing site/ is built, not skipped past. The build is the whole gate;
#      no-opping when it is absent would make a green verdict meaningless. An
#      explicit $GAG_SITE_DIR that does not exist is an error instead — the
#      caller named a tree, so building a different one would be a lie.
#
# Usage:
#   scripts/docs/check-release-links.sh
#
# Env:
#   GAG_SITE_DIR            built site to resolve against (default: site/, built
#                           on demand via scripts/docs/docs-preview.sh)
#   GAG_RELEASE_NOTES_DIR   notes to scan (default: docs/releases)
#   GAG_SITE_HOST           site host (default: mkdocs.yml's site_url)
#
# Runs under `make release-links-check` and the doc-links.yml CI workflow. It is
# not part of `make check`: the oracle is an mkdocs build, which the fast gate
# has no business provisioning. Assertions: scripts/docs/check-release-links-test.sh.

set -euo pipefail
shopt -s inherit_errexit

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

notes_dir="${GAG_RELEASE_NOTES_DIR:-docs/releases}"

site_dir="${GAG_SITE_DIR:-}"
site_dir_is_default=0
if [[ -z "$site_dir" ]]; then
    site_dir="site"
    site_dir_is_default=1
fi

# The host comes from mkdocs.yml's site_url so the two cannot drift apart.
site_host="${GAG_SITE_HOST:-}"
if [[ -z "$site_host" ]]; then
    site_host="$(awk '/^site_url:/ {
        url = $2
        sub(/^[a-zA-Z]+:\/\//, "", url)
        sub(/\/.*$/, "", url)
        print url
        exit
    }' mkdocs.yml)"
fi
if [[ -z "$site_host" ]]; then
    printf 'check-release-links: no site_url in mkdocs.yml, so there is no host to\n' >&2
    printf '                     resolve. Nothing was checked.\n' >&2
    exit 2
fi

shopt -s nullglob
notes=("$notes_dir"/v*.md)
shopt -u nullglob
if (( ${#notes[@]} == 0 )); then
    printf 'check-release-links: SKIP — no release notes in %s/, so there is nothing to\n' "$notes_dir"
    printf '                     resolve (expected on a fresh fork). Nothing was checked.\n'
    exit 0
fi

# Newest note by version order, not lexically: v1.10.0 outranks v1.9.0.
mapfile -t notes < <(printf '%s\n' "${notes[@]}" | sort -V)
resolvable_version="$(basename "${notes[-1]}" .md)"
resolvable_version="${resolvable_version#v}"

if [[ ! -d "$site_dir" ]]; then
    if (( site_dir_is_default )); then
        printf 'check-release-links: %s/ is not built — building it now (make docs-build)\n' "$site_dir"
        scripts/docs/docs-preview.sh build
    else
        printf 'check-release-links: GAG_SITE_DIR=%s does not exist, so the oracle is\n' "$site_dir" >&2
        printf '                     missing. Nothing was checked.\n' >&2
        exit 2
    fi
fi
if [[ ! -f "$site_dir/index.html" ]]; then
    printf 'check-release-links: %s/ holds no index.html, so it is not a docs-site build.\n' "$site_dir" >&2
    printf '                     Rebuild it with "make docs-build". Nothing was checked.\n' >&2
    exit 2
fi

# Emit one `<line>\t<url>` record per http(s) URL outside fenced code. Inline
# code spans are blanked first: a URL shown as sample text is not a live link.
urls() {
    awk '
        /^[ \t]*(```+|~~~+)/ { infence = !infence; next }
        infence { next }
        {
            line = $0
            gsub(/`[^`]*`/, "  ", line)
            while (match(line, /https?:\/\/[^][ \t()<>"'"'"']+/)) {
                u = substr(line, RSTART, RLENGTH)
                sub(/[.,;:]+$/, "", u)                 # sentence punctuation, not path
                printf "%d\t%s\n", FNR, u
                line = substr(line, RSTART + RLENGTH)
            }
        }
    ' "$1"
}

fail=0
resolved=0
skipped_other_version=0
skipped_unversioned=0
external=0
declare -A skipped_by_version=()

report() {
    local file="$1" lineno="$2" msg="$3"
    fail=1
    if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
        printf '::error file=%s,line=%d::%s\n' "$file" "$lineno" "$msg"
    else
        printf '%s:%d: %s\n' "$file" "$lineno" "$msg"
    fi
}

for note in "${notes[@]}"; do
    while IFS=$'\t' read -r lineno url; do
        [[ -n "$url" ]] || continue

        case "$url" in
            http://"$site_host" | http://"$site_host"/* | \
            https://"$site_host" | https://"$site_host"/*) ;;
            *)
                external=$((external + 1))
                continue
                ;;
        esac

        rest="${url#*://}"
        rest="${rest#"$site_host"}"
        rest="${rest#/}"

        anchor=""
        case "$rest" in
            *#*)
                anchor="${rest#*#}"
                rest="${rest%%#*}"
                ;;
        esac

        version="${rest%%/*}"
        if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            # An unversioned site link floats to whatever `stable` points at,
            # which is the thing these notes exist to pin. Not this gate's
            # failure to declare, but never silently dropped either.
            skipped_unversioned=$((skipped_unversioned + 1))
            printf 'check-release-links: %s:%s: no version prefix, not resolvable: %s\n' \
                "$note" "$lineno" "$url"
            continue
        fi
        if [[ "$version" != "$resolvable_version" ]]; then
            skipped_other_version=$((skipped_other_version + 1))
            skipped_by_version["$version"]=$(( ${skipped_by_version["$version"]:-0} + 1 ))
            continue
        fi

        path="${rest#"$version"}"
        path="${path#/}"
        if [[ -z "$path" ]]; then
            page="$site_dir/index.html"
        elif [[ "$path" == */ ]]; then
            page="$site_dir/${path}index.html"
        elif [[ -d "$site_dir/$path" ]]; then
            page="$site_dir/$path/index.html"
        else
            page="$site_dir/$path"
        fi

        resolved=$((resolved + 1))
        if [[ ! -f "$page" ]]; then
            report "$note" "$lineno" \
                "dead site link: $url — the $resolvable_version build has no $page"
            continue
        fi
        if [[ -n "$anchor" ]] && ! grep -qF -e "id=\"$anchor\"" "$page"; then
            report "$note" "$lineno" \
                "dead site anchor: $url — #$anchor is no id in $page"
        fi
    done < <(urls "$note")
done

total_site_links=$((resolved + skipped_other_version + skipped_unversioned))

# An empty result cannot tell "every link is good" from "the extractor stopped
# matching them". Every release note links the versioned site by convention, so
# finding none across the whole set means the scan broke.
if (( total_site_links == 0 )); then
    printf 'check-release-links: no %s link found in any of the %d release note(s).\n' \
        "$site_host" "${#notes[@]}" >&2
    printf '                     These notes link the versioned site by convention, so this\n' >&2
    printf '                     is the extractor matching nothing, not a clean tree.\n' >&2
    exit 1
fi

if (( skipped_other_version > 0 )); then
    for v in $(printf '%s\n' "${!skipped_by_version[@]}" | sort -V); do
        printf 'check-release-links: %d link(s) to version %s skipped — %s/ is built from this\n' \
            "${skipped_by_version["$v"]}" "$v" "$site_dir"
        printf '                     tree, which publishes %s, not %s.\n' "$resolvable_version" "$v"
    done
fi
if (( external > 0 )); then
    printf 'check-release-links: %d link(s) to other hosts not checked — no local oracle.\n' "$external"
fi

if (( fail )); then
    printf '\ncheck-release-links: a release note points at a page or anchor the site does not\n' >&2
    printf 'serve. Fix the link, or the heading it names. Rebuild the oracle with\n' >&2
    printf '"make docs-build" if the docs changed since it was built.\n' >&2
    exit 1
fi

if (( resolved == 0 )); then
    printf '\ncheck-release-links: WARNING — 0 of %d site link(s) were resolvable. Every one\n' \
        "$total_site_links" >&2
    printf 'names a version other than %s, the only version %s/ can stand in for.\n' \
        "$resolvable_version" "$site_dir" >&2
    exit 0
fi

printf 'check-release-links: ok (%d link(s) into %s resolved against %s/, %d skipped)\n' \
    "$resolved" "$resolvable_version" "$site_dir" "$((total_site_links - resolved))"
