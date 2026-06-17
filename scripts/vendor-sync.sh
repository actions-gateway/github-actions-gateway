#!/usr/bin/env bash
#
# vendor-sync.sh - Re-sync the workspace module files, vendor trees, and notices.
#
# Runs the full "Changing dependencies" remedy flow (docs/development/
# go-workspaces.md § Changing dependencies) in one shot, in dependency order:
#   1. scripts/go-work-tidy.sh    - tidy every workspace module leaf-first
#   2. go work sync               - push the resolved build list into each module
#   3. go work vendor             - rebuild the shared repo-root vendor/ from go.sum
#   4. (cd tools; go mod vendor)  - rebuild the tools module's own vendor/ (GOWORK=off)
#   5. gen-third-party-notices.sh - regenerate THIRD-PARTY-NOTICES from vendor/
#
# This is the single command that clears all three drift gates at once —
# tidy-check (Q94), vendor-check (Q126), and license-notices. It is the remedy a
# human runs after changing a dependency, and the action the dependabot-go-sync
# workflow (Q111) runs to auto-repair a Dependabot Go bump, which updates a
# module's go.mod/go.sum but cannot run `go work vendor` itself.
#
# It mutates the working tree (unlike the read-only *-check siblings); pair it
# with `git diff` / `git status` to review or commit the result. It no-ops
# cleanly (no diff) when the committed tree is already in sync.
#
# Usage: scripts/vendor-sync.sh
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

echo "tidying workspace modules (scripts/go-work-tidy.sh)..."
scripts/go-work-tidy.sh
echo "syncing workspace build list (go work sync)..."
go work sync
echo "re-vendoring workspace (go work vendor)..."
go work vendor
echo "re-vendoring tools module (cd tools && go mod vendor)..."
(cd tools && GOWORK=off go mod vendor)
echo "regenerating THIRD-PARTY-NOTICES (scripts/gen-third-party-notices.sh)..."
scripts/gen-third-party-notices.sh
echo "vendor sync complete — review 'git status' for any updated files."
