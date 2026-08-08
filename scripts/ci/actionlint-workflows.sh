#!/usr/bin/env bash
#
# actionlint every workflow under .github/workflows/. actionlint finds them
# itself from the git root, so there is no file list here to drift out of sync
# with the `workflows` path filter that gates the CI job.
#
# Two classes are what this gate is for: the workflow schema and `uses:`/
# expression correctness that a YAML linter cannot see, and the inline `run:`
# scripts, which actionlint hands to shellcheck. Self-hosted `runs-on:` labels
# are declared in .github/actionlint.yaml — a typo'd label still fails.
#
# The shellcheck check below is load-bearing, not a convenience: with shellcheck
# absent from PATH actionlint silently DISABLES the run: integration and still
# exits 0, so half this gate would report green having linted nothing (the Q404 /
# Q432 false-green class). shellcheck is a `required`-tier tool in
# scripts/ci/check-tools.sh, so `make doctor` already covers it.
#
# Backs `make actionlint` and the `actionlint` job in unit-test.yml.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

main() {
	cd "$REPO_ROOT"
	require_cmd shellcheck "https://github.com/koalaman/shellcheck#installing"

	local actionlint="${ACTIONLINT:-$REPO_ROOT/.build/actionlint}"
	if [[ ! -x "$actionlint" ]]; then
		echo "actionlint not built at $actionlint — run: make actionlint" >&2
		exit 1
	fi

	echo "==> actionlint .github/workflows/"
	"$actionlint" -no-color
}

main "$@"
