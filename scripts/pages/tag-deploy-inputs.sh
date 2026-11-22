#!/usr/bin/env bash
#
# tag-deploy-inputs.sh — turn a pushed release tag into the `pages.yml`
# workflow_dispatch inputs that deploy its docs (Q1355).
#
# A Pages deployment whose ref is a tag and whose commit is the one the site is
# already serving is accepted and never activated (v1.9.0, v1.10.0, v2.0.0 and
# v2.0.1 each; see docs/postmortems/2026-08-26-the-tag-published-no-docs.md).
# The same content deployed on `main` activates. So the tag's deploy is
# dispatched on `main` with `docs_ref` naming the tag, and the version and alias
# the tag's own ref used to resolve are computed here instead.
#
# Usage:
#   tag-deploy-inputs.sh vX.Y.Z < tag-names
#
# stdin: every tag name in the repository, one per line (v-prefixed).
# stdout, one `key=value` per line:
#   deploy=false                              a prerelease: nothing to publish
#   deploy=true version=X.Y.Z docs_ref=vX.Y.Z alias=stable set_default=true
#                                             the highest stable release
#   ... alias= set_default=false              a backport to an older line, which
#                                             must not move `stable`
#
# "Stable" is publish.yml's test, shared with pages.yml: a SemVer core of 0.x,
# or any `-` suffix, is a prerelease.
#
# Assertions: tag-deploy-inputs-test.sh, under `make scripts-test`.
set -euo pipefail
shopt -s inherit_errexit

if (($# != 1)) || [[ "$1" != v* ]]; then
	echo "usage: $0 vX.Y.Z < tag-names" >&2
	exit 2
fi

tag="$1"
core="${tag#v}"

if [[ "$core" == 0.* || "$core" == *-* ]]; then
	echo "deploy=false"
	exit 0
fi

# Highest stable tag, the tag itself included so a not-yet-listed push still ranks.
highest="$(
	{
		cat
		echo "$tag"
	} | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ {sub(/^v/, ""); print}' | sort -V | tail -n 1
)"

echo "deploy=true"
echo "version=${core}"
echo "docs_ref=${tag}"
if [[ "$highest" == "$core" ]]; then
	echo "alias=stable"
	echo "set_default=true"
else
	echo "alias="
	echo "set_default=false"
fi
