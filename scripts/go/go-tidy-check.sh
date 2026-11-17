#!/usr/bin/env bash
#
# go-tidy-check.sh - Fail if any module's go.mod/go.sum drifts from the tidy flow.
#
# `go mod tidy` is the canonical normaliser for a module's go.mod/go.sum: it adds
# missing entries (including the `/go.mod` hash rows for every module in the build
# graph) and drops unused ones. When the committed go.sum is not in that canonical
# shape, the documented dependency-change flow (scripts/go/go-work-tidy.sh +
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
# Usage: scripts/go/go-tidy-check.sh
#   GO_TIDY_CHECK_ATTEMPTS (default 3) and GO_TIDY_CHECK_RETRY_DELAY seconds
#   (default 5) bound the retry on a module-proxy transport error.
set -euo pipefail
shopt -s inherit_errexit

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# A dropped HTTP/2 stream while fetching a module from the proxy, the text both
# CI sightings carried (Q1314):
#   gonum.org/v1/gonum@v0.17.0: read "https://proxy.golang.org/...zip": stream error: stream ID 241; INTERNAL_ERROR; received from peer
# A failed tidy rewrites nothing and a re-run is idempotent, so retrying is safe;
# any other failure is deterministic and is not retried.
transient_re=': read "https?://[^"]+": stream error: '
attempts="${GO_TIDY_CHECK_ATTEMPTS:-3}"
retry_delay="${GO_TIDY_CHECK_RETRY_DELAY:-5}"
err_log="$(mktemp)"
trap 'rm -f "$err_log"' EXIT

# run_retrying_transient CMD... — run CMD, re-running it up to $attempts times
# while its stderr matches $transient_re. Exhausted retries exit non-zero naming
# a download failure, so it never reads as drift.
run_retrying_transient() {
	local attempt=1 rc
	while true; do
		rc=0
		"$@" 2>"$err_log" || rc=$?
		cat "$err_log" >&2
		(( rc == 0 )) && return 0
		if ! grep -Eq "$transient_re" "$err_log"; then
			return "$rc"
		fi
		if (( attempt >= attempts )); then
			echo "" >&2
			echo "ERROR: '$*' failed downloading a module from the proxy $attempts times." >&2
			echo "This is a network failure, not untidy module files; re-run the job." >&2
			return "$rc"
		fi
		echo "module proxy transport error (attempt $attempt/$attempts); retrying in ${retry_delay}s..." >&2
		sleep "$retry_delay"
		attempt=$((attempt + 1))
	done
}

# Re-run the documented tidy flow (go-work-tidy.sh tidies every module, the
# workspace members in go.work order; `go work sync` pushes the resolved build
# list back into each module).
# Both touch only go.mod/go.sum/go.work.sum — never the vendored source.
echo "tidying workspace modules (scripts/go/go-work-tidy.sh)..."
run_retrying_transient scripts/go/go-work-tidy.sh
echo "syncing workspace build list (go work sync)..."
run_retrying_transient go work sync

# Any drift means the committed module files are not in tidy-canonical form.
if ! git diff --exit-code -- '**/go.mod' '**/go.sum' go.work go.work.sum; then
	echo "" >&2
	echo "ERROR: a go.mod/go.sum/go.work.sum is not tidy (drift shown above)." >&2
	echo "The committed module files differ from what the tidy flow produces." >&2
	echo "Re-sync and commit: 'scripts/go/go-work-tidy.sh && go work sync'" >&2
	echo "(see docs/development/go-workspaces.md § Changing dependencies)." >&2
	exit 1
fi

echo "all go.mod/go.sum/go.work.sum files are tidy."
