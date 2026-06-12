# Shared helpers for the scripts/ tree. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/lib/common.sh
#   source "$REPO_ROOT/scripts/lib/common.sh"
#
# Callers must set REPO_ROOT before sourcing and have `set -euo pipefail`
# active (every script in this repo does, per the bash conventions).
# shellcheck shell=bash

# require_cmd NAME INSTALL_URL — fail fast with an install hint when a tool a
# gate needs is missing from PATH.
require_cmd() {
	local name="$1" url="$2"
	command -v "$name" >/dev/null 2>&1 || {
		echo "$name not found on PATH — install: $url" >&2
		exit 1
	}
}

# workspace_modules — print the disk path of every module in go.work, one per
# line. The repo is a Go workspace, so go tooling runs per module (a repo-root
# `go test ./...` does not work — see docs/development/go-workspaces.md).
workspace_modules() {
	go work edit -json | jq -r '.Use[].DiskPath'
}

# init_throttle — populate THROTTLE_JOBS / THROTTLE_PREFIX from
# scripts/local-throttle.sh: a parallelism cap (physical cores − 2) and a
# low-priority QoS command prefix on an interactive GUI dev shell, both empty
# on CI/headless/SSH so heavy phases run at full speed there. See that
# script's header for the detection rules and rationale (an unthrottled run
# can trip the macOS WindowServer watchdog and freeze the GUI).
#
# THROTTLE_PREFIX is a command prefix ("taskpolicy -c utility", "nice -n 19")
# that callers expand UNQUOTED so it word-splits into command + args; when
# empty it disappears entirely.
init_throttle() {
	# shellcheck disable=SC2034  # consumed by the sourcing scripts
	THROTTLE_JOBS="$("$REPO_ROOT/scripts/local-throttle.sh" jobs)"
	# shellcheck disable=SC2034  # consumed by the sourcing scripts
	THROTTLE_PREFIX="$("$REPO_ROOT/scripts/local-throttle.sh" prefix)"
}

# Placeholder sha256 digest used only to render the Helm chart for scanning
# and validation: production installs pin gmc.image.digest, so auditing the
# digest-pinned form reflects the SHIPPED posture. A digest is also REQUIRED
# to render at all — the chart fails closed when gmc.image.digest is empty
# (Q96), and scripts/manifest-validate.sh asserts that rejection. The value
# must satisfy values.schema.json's sha256:[a-f0-9]{64} pattern. Shared by
# polaris-scan.sh and manifest-validate.sh.
# shellcheck disable=SC2034  # consumed by the sourcing scripts
POLARIS_RENDER_DIGEST="${POLARIS_RENDER_DIGEST:-sha256:1111111111111111111111111111111111111111111111111111111111111111}"
