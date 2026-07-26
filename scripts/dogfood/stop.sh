#!/usr/bin/env bash
# Take the dogfood GKE cluster offline and reset the GAG runner label. Opt-in
# dispatches are one-shot, so there is no standing push/PR CI route to revert;
# resetting the label just makes a later dispatch a safe no-op (and undoes any
# end-state global routing). See docs/plan/gke-dogfood.md Part D.
#
# Order matters (Q434). The system pool carries the tenant AGCs, and an AGC is
# the only thing that reaps its worker pods. Scaling the pool to 0 with a job in
# flight evicts the AGC with nowhere to reschedule, so its workers keep running
# on the `workers` pool, those nodes stay pinned by pods no controller will ever
# delete, and the cluster bills spot nodes until a human notices — one incident
# stranded 82 spot node-hours. So this script: routes CI off GAG, waits for the
# in-flight workers to drain, and only then takes the pool to 0. A drain that
# does not finish inside DRAIN_TIMEOUT fails the stop rather than scaling down,
# because leaving the (small, fixed-size) system pool up costs far less than
# stranding worker nodes, and it keeps the AGC alive to finish reaping.
#
# Required env vars (export before running):
#   PROJECT   GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-east1-b)
#   REPO      GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#
# Optional env vars:
#   DRAIN_TIMEOUT   Seconds to wait for in-flight workers (default 1500 — the
#                   longest dispatched job's timeout-minutes plus reap time).
#   DRAIN_INTERVAL  Seconds between drain polls (default 15).
#   SKIP_DRAIN=1    Scale down without waiting. Only for a cluster already known
#                   idle, or one already broken (an AGC that is down cannot reap,
#                   so its orphaned workers never drain — see Q435).
#   SYSTEM_POOL     Node pool to take to 0 (default default-pool), matching the
#                   knob start.sh and e2e-stop.sh size.
#
# Idempotent: a resize to the current node count is a no-op and a cluster that is
# already at rest drains instantly, so this is safe to re-run.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/dogfood/lib/workers.sh
source "${REPO_ROOT}/scripts/dogfood/lib/workers.sh"

SYSTEM_POOL="${SYSTEM_POOL:-default-pool}"
DRAIN_TIMEOUT="${DRAIN_TIMEOUT:-${WORKER_DRAIN_TIMEOUT_DEFAULT}}"
DRAIN_INTERVAL="${DRAIN_INTERVAL:-${WORKER_DRAIN_INTERVAL_DEFAULT}}"

# drain_workers — block until no worker pod is in flight, or abort the stop.
# Skipped entirely under SKIP_DRAIN=1.
drain_workers() {
	if [[ "${SKIP_DRAIN:-}" == "1" ]]; then
		echo "SKIP_DRAIN=1 set — scaling down without waiting for in-flight workers."
		echo "  Any worker running now will be stranded on its node until you delete it."
		return 0
	fi

	echo "Waiting for in-flight worker pods to drain (up to ${DRAIN_TIMEOUT}s)..."
	if wait_for_worker_drain "${DRAIN_TIMEOUT}" "${DRAIN_INTERVAL}"; then
		echo "No worker pods in flight — safe to scale the system pool down."
		return 0
	fi

	echo >&2
	echo "ERROR: worker pods were still in flight after ${DRAIN_TIMEOUT}s:" >&2
	describe_inflight_workers >&2
	echo >&2
	echo "NOT scaling the system pool down. Doing so would evict the tenant AGCs," >&2
	echo "and an AGC is the only thing that reaps these pods — they would keep" >&2
	echo "their (billable) worker nodes up indefinitely." >&2
	echo >&2
	echo "Next steps:" >&2
	echo "  - Jobs genuinely still running: wait, then re-run this script." >&2
	echo "  - Jobs finished but the pods remain: their AGC is wedged. Bounce it" >&2
	echo "    (scripts/dogfood/ops.sh agc-bounce ci) and re-run this script." >&2
	echo "  - Pods are orphans with no live job: delete them by hand, then re-run." >&2
	echo "  - You accept stranding them: re-run with SKIP_DRAIN=1." >&2
	exit 1
}

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"
	require_cmd gh "https://cli.github.com/"

	# Route CI off GAG FIRST, so the drain below is not chasing a queue that
	# keeps growing. Already-queued dispatches still land on the scale set —
	# that is what the drain wait is for.
	echo "Resetting GAG runner label to ubuntu-latest..."
	gh variable set GAG_RUNNER \
		--body '"ubuntu-latest"' \
		--repo "${REPO}"

	# Pin the target cluster and fail closed if it is not the active context, so
	# the drain reads (and the scale-down decision they gate) can only ever be
	# about this cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	drain_workers

	# --project/--zone are pinned per call so the resize never relies on the
	# active gcloud config.
	echo "Scaling system pool (${SYSTEM_POOL}) to 0 nodes..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool="${SYSTEM_POOL}" --num-nodes=0 --zone="${ZONE}" --quiet

	echo "Done. Worker nodes drain and autoscale to 0 automatically (~10 min)."
}

main "$@"
