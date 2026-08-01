#!/usr/bin/env bash
#
# Run govulncheck across every Go workspace module. govulncheck is
# symbol-precise (it only fails on vulnerabilities actually reachable from our
# code) and module-scoped, hence the per-module loop. Backs `make vulncheck`
# and the `govulncheck` job in .github/workflows/security-scan.yml.
#
# Env:
#   GOVULNCHECK  Path to the govulncheck binary (default .build/govulncheck at
#                the repo root — build it with `make govulncheck`).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

GOVULNCHECK="${GOVULNCHECK:-$REPO_ROOT/.build/govulncheck}"
if [[ ! -x "$GOVULNCHECK" ]]; then
	echo "govulncheck not found at $GOVULNCHECK — build it with: make govulncheck" >&2
	exit 1
fi

for dir in $(workspace_modules); do
	echo "==> govulncheck $dir"
	(cd "$dir" && "$GOVULNCHECK" ./...)
done
