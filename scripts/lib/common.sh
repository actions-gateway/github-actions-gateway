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

# confirm_or_exit MESSAGE — print MESSAGE then require an interactive y/yes
# before continuing; exit non-zero on anything else. ASSUME_YES=1 skips the
# prompt (automation). Gate billable or destructive operations with this so a
# fat-finger is a no-op rather than a cloud spend.
confirm_or_exit() {
	local message="$1" reply
	printf '%s\n' "$message"
	if [[ "${ASSUME_YES:-}" == "1" ]]; then
		echo "ASSUME_YES=1 set — skipping confirmation."
		return 0
	fi
	read -r -p "Proceed? [y/N] " reply
	if [[ "$reply" != "y" && "$reply" != "Y" && "$reply" != "yes" ]]; then
		echo "Aborted — no changes made." >&2
		exit 1
	fi
}

# gke_get_credentials_and_verify PROJECT ZONE CLUSTER — fetch kubeconfig for the
# named GKE cluster, then fail closed unless it became the active kubectl
# context. Every later kubectl/helm call runs against the current context, so
# this one assertion guards them all from landing on the wrong cluster (e.g. a
# production context that happened to be selected). Callers must require_cmd
# gcloud, kubectl, and gke-gcloud-auth-plugin (GKE kubeconfigs authenticate
# through that external plugin).
gke_get_credentials_and_verify() {
	# Names are gke_-prefixed (not project/zone/cluster) so shellcheck's SC2153
	# does not flag callers' ${PROJECT}/${ZONE}/${CLUSTER} as case-typos of them.
	local gke_project="$1" gke_zone="$2" gke_cluster="$3"
	echo "Fetching cluster credentials for ${gke_cluster}..."
	gcloud container clusters get-credentials "$gke_cluster" \
		--project="$gke_project" --zone="$gke_zone"
	local expected current
	expected="gke_${gke_project}_${gke_zone}_${gke_cluster}"
	current="$(kubectl config current-context)"
	if [[ "$current" != "$expected" ]]; then
		echo "Refusing to continue: kubectl context is '${current}'," >&2
		echo "expected '${expected}'. Aborting before any cluster writes." >&2
		exit 1
	fi
	echo "Active kubectl context: ${current}"
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

# serialize_heavy_build — serialize the calling script's heavy work across
# concurrent worktrees/sessions on one dev machine, so two `make check` runs
# don't collectively saturate a small core count and blow the linter/test
# timeouts. The parallelism cap (init_throttle) bounds ONE run's fan-out but is
# blind to siblings; this holds a machine-wide lock so sibling runs queue and
# each runs at full throttle in turn. Idle servers and CI are NOT serialized
# (local-throttle.sh reports no lock there) — those SHOULD run fully parallel.
#
# It re-execs the calling script once under an exclusive advisory lock held for
# the script's whole lifetime, then proceeds normally in the re-exec'd child.
# Call it AFTER `cd "$REPO_ROOT"` and sourcing this file, passing the script's
# own "$@":
#
#   serialize_heavy_build "$@"
#
# No-op when there is no lock file (CI/headless/SSH/non-GUI), when perl is
# absent, or when the lock is already held (the sentinel env var), so a locked
# script may invoke other locked scripts without deadlocking on itself.
#
# Implemented with perl's flock: a true blocking advisory lock available on both
# macOS (which ships no flock(1)) and Linux, and — crucially — released
# automatically when the holding process dies, so a Ctrl-C'd or killed build
# never strands a stale lock that wedges every later run.
serialize_heavy_build() {
	[[ -n "${GAG_HEAVY_BUILD_LOCK_HELD:-}" ]] && return 0
	local lock
	lock="$("$REPO_ROOT/scripts/local-throttle.sh" lockfile)"
	[[ -z "$lock" ]] && return 0
	command -v perl >/dev/null 2>&1 || return 0
	export GAG_HEAVY_BUILD_LOCK_HELD=1
	# perl takes the lock, runs the script as a child, and exits with its status;
	# the lock fd lives in perl and releases when perl exits. A lock that cannot
	# be opened degrades to running unserialized rather than failing the build.
	exec perl -MFcntl=:flock -e '
		my $path = shift @ARGV;
		open(my $fh, ">", $path) or exec @ARGV;
		flock($fh, LOCK_EX);
		my $rc = system @ARGV;
		exit 255 if $rc == -1;
		exit($rc & 127 ? 128 + ($rc & 127) : $rc >> 8);
	' "$lock" bash "$0" "$@"
}

# release_identity_regexp — print the cosign --certificate-identity-regexp that
# a legitimate release signature must match. Releases are cut by pushing a v*
# tag, so the keyless Fulcio cert records publish.yml running from
# `refs/tags/vX.Y.Z`. The pattern is anchored and TAGS-ONLY: it deliberately
# rejects a signature minted from a branch ref (`refs/heads/...`) — e.g. a
# workflow_dispatch run from a scratch branch that overwrote a released GHCR tag
# (Q124). Shared by verify-release.sh (the verifier) and verify-release-test.sh
# (the assertion that the tags-only property holds). Single arg: the
# `owner/repo` slug, default actions-gateway/github-actions-gateway.
release_identity_regexp() {
	local slug="${1:-actions-gateway/github-actions-gateway}"
	printf '^https://github.com/%s/\\.github/workflows/publish\\.yml@refs/tags/v.*$' "$slug"
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
