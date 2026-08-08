#!/usr/bin/env bash
#
# Print where the release-validation gate is, as one JSON object (Q616).
#
# The gate keeps this rendered at tmp/release-validation-status.json, so an
# operator or an agent usually just reads that file. This is the same renderer
# as a command, for reading a stream that is not the default one, or for
# re-rendering after the gate's own process is gone.
#
# Usage:
#   scripts/dogfood/release-status.sh [progress-file]
#
# Fields: gate (preflight|running|passed|failed), rc, phase, state, detail,
# startedAt, updatedAt, elapsed, phaseElapsed, idle, heartbeat, heartbeatAge,
# failure. Nulls mean "not yet known", not "zero".
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/dogfood/lib/progress.sh
source "${REPO_ROOT}/scripts/dogfood/lib/progress.sh"

progress_status_json "${1:-}"
