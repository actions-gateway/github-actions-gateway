#!/usr/bin/env bash
#
# Shellcheck every tracked shell script under scripts/. The git pathspec
# 'scripts/*.sh' matches recursively (git's default '*' spans '/'), so it
# covers all tracked .sh files — including scripts/lib/*.sh and any future
# scripts/<subdir>/*.sh — while skipping untracked scratch scripts. Backs
# `make shellcheck`, mirrored by the `shellcheck` job in unit-test.yml — CI
# pins shellcheck v0.11.0; install that version locally so verdicts match
# (shellcheck's heuristics drift between releases).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

require_cmd shellcheck "https://github.com/koalaman/shellcheck#installing"

files="$(git ls-files 'scripts/*.sh')"
if [[ -z "$files" ]]; then
	echo "no scripts to shellcheck"
	exit 0
fi
echo "==> shellcheck $(wc -l <<<"$files" | tr -d ' ') script(s) under scripts/"
# shellcheck disable=SC2086  # the file list word-splits intentionally (one path per line, no spaces)
shellcheck $files
