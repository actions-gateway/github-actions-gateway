#!/usr/bin/env bash
# Print the newest published cluster-autoscaler patch release WITHIN the minor a
# given pin already names, as "vX.Y.Z", with no trailing newline.
#
# Used by updatecli (updatecli.d/cluster-autoscaler.yaml) as a shell source so
# CA_VERSION in scripts/e2e/autoscaler-cluster.sh — the cluster-autoscaler the
# live-autoscaler drift gate installs — picks up patch releases weekly instead of
# waiting for the next kind minor bump (Q483). The gate exists to catch an
# upstream event-vocabulary reword, and it only ever runs against the version
# that is pinned, so a patch release nobody pins is a patch release nobody tests.
#
# Why the minor is a floor and a ceiling, not a starting point: cluster-autoscaler
# ships one release series per Kubernetes minor, and its scheduler-framework
# predicates are that release's. The harness pins no KIND_NODE_IMAGE, so its
# cluster runs kind's default node image, and moving CA across a minor is
# therefore a decision about the kind version — made by hand in the kind bump PR,
# never here. This script resolves patches and nothing else.
#
# Why the registry rather than the GitHub release tags: the harness pulls
# registry.k8s.io/autoscaling/cluster-autoscaler:<tag>, so the registry's own tag
# list is the only source that answers the question actually being asked — which
# patch can this harness pull today. A git tag exists before its image is
# published, and auto-bumping into that window would leave the gate failing on an
# ImagePullBackOff that has nothing to do with drift. It also sidesteps the
# GitHub release tags being unparseable as semver ("cluster-autoscaler-1.36.1").
#
# Usage: latest-cluster-autoscaler-patch.sh <current-pin>   # e.g. v1.36.1
set -euo pipefail
shopt -s inherit_errexit

# The OCI tag-list endpoint for the image scripts/e2e/autoscaler-cluster.sh installs.
CA_TAGS_URL='https://registry.k8s.io/v2/autoscaling/cluster-autoscaler/tags/list'

main() {
	local pin="${1:?usage: latest-cluster-autoscaler-patch.sh <current-pin>   # e.g. v1.36.1}"

	if [[ ! "${pin}" =~ ^v([0-9]+\.[0-9]+)\.[0-9]+$ ]]; then
		echo "not a cluster-autoscaler pin of the form vX.Y.Z: ${pin}" >&2
		return 1
	fi
	local minor="${BASH_REMATCH[1]}"

	# Assigned on its own line, not on the `local`, so a curl or jq failure keeps
	# its exit status and trips `set -e` instead of being masked by `local`'s.
	# --retry tolerates a transient registry.k8s.io redirector error.
	local tags
	tags="$(curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 "${CA_TAGS_URL}" | jq -r '.tags[]')"

	# grep -x anchors to a bare vX.Y.Z, which drops both the other minors and any
	# pre-release or suffixed tag (v1.36.3-beta.0). An empty result is a real
	# answer here — reported below — so it must not abort the pipeline.
	local newest
	newest="$(printf '%s\n' "${tags}" | { grep -xE "v${minor}\.[0-9]+" || true; } | sort -V | tail -1)"
	if [[ -z "${newest}" ]]; then
		echo "no published cluster-autoscaler ${minor}.x image tag at ${CA_TAGS_URL}" >&2
		return 1
	fi

	# Never move backwards. A yanked release or a listing that lost a tag would
	# otherwise "bump" the harness onto an older cluster-autoscaler than the one
	# already vetted. Emitting the pin unchanged makes updatecli see no diff and
	# open no PR, which is the correct no-op.
	printf '%s\n%s\n' "${pin}" "${newest}" | sort -V | tail -1 | tr -d '\n'
}

main "$@"
