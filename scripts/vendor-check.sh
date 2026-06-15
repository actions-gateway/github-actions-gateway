#!/usr/bin/env bash
#
# vendor-check.sh - Fail if the committed vendor trees drift from go.sum.
#
# `go build -mod=vendor` only checks vendor/modules.txt consistency — it never
# verifies that the vendored *source* matches the hashes in go.sum. So a
# malicious or accidental edit under vendor/ (or tools/vendor/) compiles into
# the signed release images undetected. This gate closes that gap (Q126): it
# re-runs the workspace-vendor flow — which re-fetches every module verified
# against go.sum — and fails on any diff against the committed trees. Sibling of
# the license-notices drift gate (THIRD-PARTY-NOTICES) and of Q94/Q111.
#
# Dependabot interaction (Q111): a bot go.mod/go.sum bump cannot run
# `go work vendor`, so it lands a desynced vendor tree and this gate fails on
# that PR — which is the desired signal. The remedy is a follow-up vendor sync
# (see docs/development/go-workspaces.md § Changing dependencies), not an
# exemption: the gate asserts the COMMITTED tree is self-consistent.
#
# Single source of truth for `make vendor-check` and the CI vendor-check job.
#
# Usage: scripts/vendor-check.sh
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# Regenerate both vendor trees from go.mod/go.sum. `go work vendor` re-fetches
# every workspace module (hash-verified against each module's go.sum) into the
# shared vendor/. The tools module has its own vendor/ outside the workspace,
# regenerated the same way the build consumes it (GOWORK=off).
echo "re-vendoring workspace (go work vendor)..."
go work vendor
echo "re-vendoring tools module (cd tools && go mod vendor)..."
(cd tools && GOWORK=off go mod vendor)

# Any drift means the committed tree does not match what go.mod+go.sum produce.
if ! git diff --exit-code -- vendor/ tools/vendor/; then
	echo "" >&2
	echo "ERROR: vendor/ or tools/vendor/ is out of sync with go.sum (drift shown above)." >&2
	echo "A committed vendor tree that doesn't match its go.sum hashes is a supply-chain risk." >&2
	echo "Re-sync and commit: 'go work vendor && (cd tools && GOWORK=off go mod vendor)'" >&2
	echo "(see docs/development/go-workspaces.md § Changing dependencies)." >&2
	exit 1
fi

echo "vendor/ and tools/vendor/ are consistent with go.sum."
