# Shared skeleton for the sync-chart-{crds,rbac,webhook}.sh chart generators
# (Q370 / F10). Each of those scripts regenerates a Helm chart artifact FROM its
# controller-gen source and offers a `--check` drift mode. This file centralises
# the three things they had triplicated verbatim: a temp-file registry with a
# single EXIT-trap cleanup (--check renders a candidate to a temp file and diffs
# it, never mutating the working tree), and the `--check` / write / usage
# argument dispatch. The per-artifact render/sync/check logic stays in each
# script because it genuinely differs.
#
# Source AFTER `cd "$REPO_ROOT"` and having `set -euo pipefail` active (every
# script in this repo does, per the bash conventions):
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   cd "$REPO_ROOT"
#   # shellcheck source=scripts/lib/chart-sync.sh
#   source "$REPO_ROOT/scripts/lib/chart-sync.sh"
#
# Each sourcing script must define sync() and check(), then end with:
#
#   chart_sync_main "$@"
#
# shellcheck shell=bash

# Temp files registered by chart_sync_mktemp; removed on EXIT. Declared empty so
# the cleanup is a no-op when nothing was created (and never trips `set -u` on
# macOS bash 3.2's empty-array expansion).
CHART_SYNC_TMP_FILES=()
_chart_sync_cleanup() {
	if [[ ${#CHART_SYNC_TMP_FILES[@]} -gt 0 ]]; then
		rm -f "${CHART_SYNC_TMP_FILES[@]}"
	fi
}
trap _chart_sync_cleanup EXIT

# chart_sync_mktemp — print a fresh temp-file path and register it for cleanup on
# EXIT. Each script's check() uses this to render a candidate artifact off to the
# side, so the drift comparison never touches the committed chart.
chart_sync_mktemp() {
	local tmp
	tmp="$(mktemp)"
	CHART_SYNC_TMP_FILES+=("$tmp")
	printf '%s\n' "$tmp"
}

# chart_sync_main — the shared argument contract for the three generators:
#   (no args)  regenerate the chart artifact in place (the `make chart-*` target)
#   --check    fail if the committed chart artifact is stale (`make chart-*-check`)
# Requires the sourcing script to define sync() and check().
chart_sync_main() {
	case "${1:-}" in
	--check)
		check
		;;
	"")
		sync
		;;
	*)
		echo "usage: $0 [--check]" >&2
		exit 2
		;;
	esac
}
