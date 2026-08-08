#!/usr/bin/env bash
# Delete the dogfood GKE cluster outright — the "not needed for a while" state,
# one level beyond `stop.sh`.
#
#   stop.sh    Scale to 0 nodes. Cluster object survives; GAG install, tenant
#              CRs, cert-manager PKI, and the GitHub App secret all persist.
#              Restart is `start.sh` (~5 min). Use between CI sessions — this
#              is the normal at-rest state.
#   delete.sh  Delete the cluster object. Everything inside it is destroyed.
#              Recreate is `setup.sh` (+ `e2e-setup.sh` for the e2e tenant),
#              which is a full bootstrap (~20 min), not a resume.
#
# Deleting is NOT a cost optimisation. The dogfood cluster is a zonal Standard
# cluster, one of which is free per billing account, so at 0 nodes it already
# bills $0.00/hr — `stop.sh` captures the entire saving. Delete for one of the
# real reasons instead:
#   - Done with the environment for the foreseeable future and you would rather
#     not leave a cluster lying around accruing config drift.
#   - The cluster HAS drifted (hand-made pools, hand-patched objects) and you
#     want it converged back onto the scripted shape — recreate is the cheapest
#     way to get a known-good environment.
#   - The free zonal-cluster slot is wanted for something else.
# If the goal is just "stop paying for it", run stop.sh and stop reading here.
#
# What survives a delete (so recreate is possible): the GCP project, billing
# link, enabled APIs, quota, the GitHub App and its installation, and the App
# private key in the local Keychain. This script deletes ONLY the cluster — it
# never touches the project or the App.
#
# What does NOT survive: the GAG control-plane install, all tenant namespaces
# and CRs, the cert-manager CA and every cert minted from it, the per-tenant
# metrics PKI, the in-cluster GitHub App secret, and every node pool including
# any created by hand. Recreate rebuilds these from scratch; it does not
# restore them.
#
# Required env vars (export before running):
#   PROJECT   GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-east1-b)
#   REPO      GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#
# Optional env vars:
#   ASSUME_YES=1  Skip the interactive confirmation (automation).
#
# Idempotent: a missing cluster is reported and exits 0, so it is safe to re-run
# after a partial failure or against an already-deleted environment.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/dogfood/lib/workers.sh
source "${REPO_ROOT}/scripts/dogfood/lib/workers.sh"

# ---------------------------------------------------------------------------
# Existence + occupancy probes. The confirmation below quotes these back, so an
# operator deleting a cluster that is actually busy sees it before answering.
# ---------------------------------------------------------------------------

cluster_exists() {
	gcloud container clusters describe "${CLUSTER}" \
		--project="${PROJECT}" --zone="${ZONE}" >/dev/null 2>&1
}

# current_node_count — total nodes across every pool, or "unknown" if the
# cluster cannot be described. A non-zero count means this is NOT the at-rest
# state and something may still be running.
current_node_count() {
	local count
	count="$(gcloud container clusters describe "${CLUSTER}" \
		--project="${PROJECT}" --zone="${ZONE}" \
		--format='value(currentNodeCount)' 2>/dev/null)" || { echo "unknown"; return; }
	echo "${count:-0}"
}

# inflight_worker_count — worker pods in a non-terminal phase, best-effort, via
# the shared label-selector probe (lib/workers.sh). The context is pinned rather
# than made active: this read only feeds the confirmation below, so it should
# not mutate kubeconfig state on a path that may abort. A failure is reported as
# "unknown" rather than assumed to be zero, so an unreachable cluster never
# reads as "safe to delete".
inflight_worker_count() {
	# Dynamically scoped for the callee: `local` here, read by the probe, gone
	# when this returns — no assignment leaks into the rest of the script.
	# shellcheck disable=SC2034  # read by count_inflight_workers via lib/workers.sh
	local WORKER_KUBECTL_CONTEXT="gke_${PROJECT}_${ZONE}_${CLUSTER}"
	count_inflight_workers
}

# ---------------------------------------------------------------------------
# Route CI away from the cluster BEFORE deleting it. Deleting first would leave
# a window where a dispatched job routes at a cluster that is mid-deletion; the
# job would hang until it timed out rather than falling back to GitHub-hosted.
# stop.sh resets the same variables for the same reason.
# ---------------------------------------------------------------------------

reset_runner_labels() {
	echo "Resetting GAG runner labels to ubuntu-latest (routing CI off the cluster)..."
	gh variable set GAG_RUNNER --body '"ubuntu-latest"' --repo "${REPO}"
	gh variable set GAG_E2E_RUNNER --body '"ubuntu-latest"' --repo "${REPO}"
}

delete_cluster() {
	echo "Deleting cluster ${CLUSTER} (this takes several minutes)..."
	gcloud container clusters delete "${CLUSTER}" \
		--project="${PROJECT}" --zone="${ZONE}" --quiet
}

# ---------------------------------------------------------------------------
# Post-delete hygiene.
# ---------------------------------------------------------------------------

# prune_kubeconfig — drop the deleted cluster's kubeconfig entries. Without
# this the context lingers and a later kubectl silently targets a cluster that
# no longer exists, which surfaces as a confusing auth/timeout error rather
# than "no such cluster". Entry names are the GKE convention that
# gke_get_credentials_and_verify asserts on.
prune_kubeconfig() {
	local entry="gke_${PROJECT}_${ZONE}_${CLUSTER}"
	echo "Pruning kubeconfig entries for ${entry}..."
	kubectl config delete-context "${entry}" >/dev/null 2>&1 || true
	kubectl config delete-cluster "${entry}" >/dev/null 2>&1 || true
	kubectl config unset "users.${entry}" >/dev/null 2>&1 || true
}

# report_orphans — GKE usually reclaims a cluster's disks and load-balancer
# addresses with the cluster, but not always: a PVC-backed disk whose
# reclaimPolicy is Retain, or an address reserved by hand, outlives it and
# bills silently. This reports what is left rather than deleting it — an
# unexpected survivor is a signal worth reading, not something to sweep away
# automatically.
report_orphans() {
	echo
	echo "Checking for billable resources that outlived the cluster..."
	local disks addresses
	disks="$(gcloud compute disks list --project="${PROJECT}" --format='value(name,zone,sizeGb)' 2>/dev/null)" || disks=""
	addresses="$(gcloud compute addresses list --project="${PROJECT}" --format='value(name,region,address)' 2>/dev/null)" || addresses=""
	if [[ -z "${disks}" && -z "${addresses}" ]]; then
		echo "  None — no leftover disks or reserved addresses."
		return
	fi
	[[ -n "${disks}" ]] && { echo "  Disks still present (these bill):"; echo "${disks}" | awk '{print "    " $0}'; }
	[[ -n "${addresses}" ]] && { echo "  Reserved addresses still present (these bill):"; echo "${addresses}" | awk '{print "    " $0}'; }
	echo "  Review these — delete them by hand if they are not intentional."
}

confirm_target() {
	local nodes workers
	nodes="$(current_node_count)"
	workers="$(inflight_worker_count)"
	confirm_or_exit "$(printf 'About to DELETE the dogfood cluster:\n  Project: %s\n  Cluster: %s  (zone %s)\n  Repo:    %s\n\n  Nodes currently up:      %s\n  Worker pods in flight:   %s\n\nThis destroys the GAG install, every tenant namespace and CR, the\ncert-manager CA, and the in-cluster GitHub App secret. Recreate is a full\nsetup.sh bootstrap, NOT a resume. To merely take the cluster offline and\nkeep all of it, run stop.sh instead.' \
		"${PROJECT}" "${CLUSTER}" "${ZONE}" "${REPO}" "${nodes}" "${workers}")"
}

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd gh "https://cli.github.com/"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"

	if ! cluster_exists; then
		echo "Cluster ${CLUSTER} does not exist in ${PROJECT}/${ZONE} — nothing to delete."
		prune_kubeconfig
		exit 0
	fi

	confirm_target
	reset_runner_labels
	delete_cluster
	prune_kubeconfig
	report_orphans

	echo
	echo "Done. The project, billing, APIs, quota, and the GitHub App are untouched."
	echo "Recreate with:  scripts/dogfood/setup.sh   (then e2e-setup.sh for the e2e tenant)"
}

[[ -n "${DELETE_LIB_ONLY:-}" ]] || main "$@"
