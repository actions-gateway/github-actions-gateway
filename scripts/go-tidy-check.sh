#!/usr/bin/env bash
#
# go-tidy-check.sh - Fail if any module's go.mod/go.sum drifts from the tidy flow.
#
# `go mod tidy` is the canonical normaliser for a module's go.mod/go.sum: it adds
# missing entries (including the `/go.mod` hash rows for every module in the build
# graph) and drops unused ones. When the committed go.sum is not in that canonical
# shape, the documented dependency-change flow (scripts/go-work-tidy.sh +
# `go work sync`) re-adds those rows, producing a spurious diff that contributors
# keep reverting. This gate closes that gap (Q94): it re-runs the tidy flow and
# fails on any drift in the workspace module files, so the committed tree is
# already in tidy-canonical form.
#
# Scope: go.mod / go.sum / go.work.sum only. The committed vendor/ tree is the
# concern of the sibling vendor-check gate (Q126) — it re-runs `go work vendor`
# and verifies the vendored source against go.sum — so this gate deliberately
# stops at `go work sync` and does not re-vendor. Together: this gate makes the
# module files canonical; vendor-check makes vendor/ match them.
#
# Dependabot interaction (Q111): a bot go.mod/go.sum bump that does not re-run the
# tidy flow can land non-canonical go.sum rows and fail here — which is the desired
# signal. The remedy is the follow-up tidy sync (see
# docs/development/go-workspaces.md § Changing dependencies), not an exemption.
#
# Single source of truth for `make tidy-check` and the CI tidy-check job.
#
# Usage: scripts/go-tidy-check.sh
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# Re-run the documented tidy flow (go-work-tidy.sh tidies every workspace module
# leaf-first; `go work sync` pushes the resolved build list back into each module).
# Both touch only go.mod/go.sum/go.work.sum — never the vendored source.
echo "tidying workspace modules (scripts/go-work-tidy.sh)..."
scripts/go-work-tidy.sh
echo "syncing workspace build list (go work sync)..."
go work sync

# Any drift means the committed module files are not in tidy-canonical form.
if ! git diff --exit-code -- '**/go.mod' '**/go.sum' go.work go.work.sum; then
	echo "" >&2
	echo "ERROR: a go.mod/go.sum/go.work.sum is not tidy (drift shown above)." >&2
	echo "The committed module files differ from what the tidy flow produces." >&2
	echo "Re-sync and commit: 'scripts/go-work-tidy.sh && go work sync'" >&2
	echo "(see docs/development/go-workspaces.md § Changing dependencies)." >&2
	exit 1
fi

echo "all go.mod/go.sum/go.work.sum files are tidy."
