#!/usr/bin/env bash
# Route e2e CI jobs back to GitHub-hosted runners and tear down the on-demand
# e2e tenant's AGC. The e2e node pool then autoscales to 0 on the cluster
# autoscaler's own schedule, which this script neither drives nor waits for.
# See the GKE dogfood plan (indexed in docs/plan/README.md), Part F.
#
# On-demand (Q231): deleting the ActionsGateway makes the GMC tear down the
# per-tenant AGC pod, freeing its standing ~500m CPU on the system node. The
# namespace, App Secret, ResourceQuota, ClusterRunnerTemplate, RunnerSet, and
# NetworkPolicy are left in place — inert without the gateway and cheap to keep,
# so a later e2e-start.sh re-applies the gateway and the AGC comes back.
#
# Drain BEFORE delete (2026-07-31 incident): the AGC is the only thing that
# serves jobs already queued on the scale set and the only thing that reaps
# worker pods. Deleting it under either strands them — a queued job waits
# forever (wedging its workflow's concurrency group), and an orphaned worker pod
# carries do-not-disrupt annotations that pin its billable node indefinitely. So
# this script waits for both to clear first, and on timeout fails BEFORE the
# delete, leaving the AGC alive to finish the work.
#
# Capacity (Q335/Q357): e2e-start.sh scaled the system pool up for the e2e
# window; this script scales it back to the running size dogfood/start.sh
# leaves it in (derived from the deployed always-on tenants — lib/pool.sh).
# dogfood/stop.sh later takes the whole system pool to 0 for the
# zero-cost-at-rest state.
#
# Required env vars (export before running):
#   PROJECT   GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-east1-b)
#   REPO      GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#
# Optional:
#   E2E_DRAIN_TIMEOUT  Seconds to wait for queued scale-set jobs, then for
#                      in-flight e2e worker pods, before failing (default 1500 —
#                      the e2e job timeout dominates both).
#   SKIP_E2E_DRAIN=1   Skip both drains and delete the AGC regardless. Anything
#                      still queued or running is knowingly stranded.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/dogfood/lib/pool.sh
source "${REPO_ROOT}/scripts/dogfood/lib/pool.sh"
# shellcheck source=scripts/dogfood/lib/workers.sh
source "${REPO_ROOT}/scripts/dogfood/lib/workers.sh"

# The scale-set runnerLabel the on-demand e2e RunnerSet registers — the runs-on
# target of routed jobs. Must stay in step with runnerLabel in
# deploy/dogfood-e2e/base/resources.yaml.
E2E_SCALE_SET_LABEL="gag-ci-e2e"
E2E_DRAIN_TIMEOUT="${E2E_DRAIN_TIMEOUT:-1500}"

# System pool sizing (Q335/Q357). After the e2e window this script restores the
# running size dogfood/start.sh computes — derived from the deployed always-on
# ActionsGateways (lib/pool.sh), so both scripts agree by construction and a
# third tenant is not re-evicted here. SYSTEM_POOL_AT_REST_NODES pins the
# restore size explicitly instead.
SYSTEM_POOL="${SYSTEM_POOL:-default-pool}"
SYSTEM_POOL_AT_REST_NODES="${SYSTEM_POOL_AT_REST_NODES:-}"

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gh "https://cli.github.com/"
	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"

	# Route e2e off GAG FIRST, so no new e2e job lands on the tenant while its
	# AGC is being torn down.
	gh variable set GAG_E2E_RUNNER \
		--body '"ubuntu-latest"' \
		--repo "${REPO}"

	# Pin the target cluster and fail closed if it is not the active context,
	# so the teardown delete never lands on another cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	# Drain before delete (see header). Queued jobs first — they have no pod yet,
	# so the pod probe cannot see them, and the still-alive AGC is what serves
	# them; then the e2e tenant's worker pods. Scoped to the e2e namespace so the
	# always-on CI tenant's ordinary traffic never holds this teardown up.
	if [[ "${SKIP_E2E_DRAIN:-0}" == "1" ]]; then
		echo "SKIP_E2E_DRAIN=1 — skipping the queued-job and worker-pod drains." >&2
	else
		echo "Waiting for jobs queued on '${E2E_SCALE_SET_LABEL}' to drain (up to ${E2E_DRAIN_TIMEOUT}s)..."
		if ! wait_for_scale_set_idle "${E2E_SCALE_SET_LABEL}" "${E2E_DRAIN_TIMEOUT}"; then
			echo "ERROR: jobs still queued on '${E2E_SCALE_SET_LABEL}' after ${E2E_DRAIN_TIMEOUT}s." >&2
			echo "NOT deleting the AGC — it is the only thing that can serve them; deleting" >&2
			echo "it now wedges each job's workflow concurrency group indefinitely." >&2
			echo "Next steps:" >&2
			echo "  - Let the AGC work the queue down, then re-run this script." >&2
			echo "  - Or cancel the queued runs (gh run list --status queued), then re-run." >&2
			echo "  - You accept stranding them: re-run with SKIP_E2E_DRAIN=1." >&2
			exit 1
		fi
		echo "Waiting for in-flight e2e worker pods to drain (up to ${E2E_DRAIN_TIMEOUT}s)..."
		WORKER_POD_NAMESPACE="${E2E_TENANT_NAMESPACE}"
		if ! wait_for_worker_drain "${E2E_DRAIN_TIMEOUT}"; then
			echo "ERROR: e2e worker pods were still in flight after ${E2E_DRAIN_TIMEOUT}s:" >&2
			describe_inflight_workers >&2
			echo "Why they are still in flight:" >&2
			explain_inflight_workers >&2
			drain_progress_summary >&2
			echo "NOT deleting the AGC — it is the only thing that reaps these pods; an" >&2
			echo "orphan's do-not-disrupt annotations pin its billable node indefinitely." >&2
			echo "Wait or bounce the AGC, then re-run; or re-run with SKIP_E2E_DRAIN=1." >&2
			exit 1
		fi
		WORKER_POD_NAMESPACE=""
	fi

	# Tear down only the ActionsGateway — the GMC deletes the AGC pod, releasing
	# the standing ~500m CPU. Everything else in the tenant is left in place.
	echo "Tearing down the on-demand e2e AGC (deleting the ActionsGateway)..."
	kubectl delete actionsgateway dogfood-e2e \
		--namespace "${E2E_TENANT_NAMESPACE}" --ignore-not-found

	# Same argument for the registry pull-through cache (Q408): five idle pods
	# would sit on the system pool that Q231 keeps the e2e AGC off. Scale rather
	# than delete, so the namespace, the NetworkPolicies and any PVC-backed layer
	# cache survive the window — e2e-start.sh's apply restores replicas to 1.
	# `kubectl scale` has no --ignore-not-found, and a cluster set up before Q408
	# has no such namespace, so the presence read is what keeps this teardown from
	# aborting there; skipping it only leaves pods running.
	local mirrors
	mirrors="$(kubectl get deployment --namespace gag-registry-mirror \
		-l app=registry-mirror -o name 2>/dev/null || true)"
	if [[ -n "${mirrors}" ]]; then
		# The pods go, and their access logs go with them. That log is the only
		# reading that says whether the job's pulls rode the mirror (Q408
		# Phase 3), so say so before it is gone rather than after.
		echo "Scaling the registry pull-through cache back to zero..."
		echo "  (their access logs go with the pods; scripts/dogfood/e2e-mirror-hits.sh reads them)"
		kubectl scale deployment --namespace gag-registry-mirror \
			-l app=registry-mirror --replicas=0
	else
		echo "No registry pull-through cache deployed — nothing to scale down."
	fi

	# Restore the system pool to the running size now the e2e window is over
	# (Q335/Q357) — derived from the deployed always-on ActionsGateways unless
	# pinned. The e2e gateway was deleted above (and its namespace is excluded
	# from the count anyway), so the derivation sees only the always-on tenants.
	# --project/--zone are pinned per call so the resize never relies on the
	# active gcloud config; a resize to the current node count is a no-op, so
	# this is safe to re-run. dogfood/stop.sh later takes it to 0.
	local restore="${SYSTEM_POOL_AT_REST_NODES}"
	[[ -n "${restore}" ]] || restore="$(required_system_nodes)"
	echo "Scaling system pool (${SYSTEM_POOL}) back to ${restore} node(s)..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool="${SYSTEM_POOL}" --num-nodes="${restore}" --zone="${ZONE}" --quiet

	echo "E2e jobs will now route to GitHub-hosted runners."
	# No duration here. Scale-down belongs to the cluster autoscaler, which this
	# script neither drives nor waits for, and a number invites reading a pool
	# that is merely slow as a pool that is stuck: measured 2026-08-28, a node
	# took ~35 minutes against the "~10 min" this line used to print, and the
	# gap nearly bought a hand-run `kubectl delete` and a pool resize against
	# the prod-classified dogfood cluster to fix nothing.
	echo "e2e pool nodes scale to 0 once in-flight jobs finish, on the cluster"
	echo "autoscaler's own schedule. Check with:"
	echo "  gcloud compute instance-groups managed list --project ${PROJECT} --filter='name~e2e'"
}

[[ -n "${E2E_STOP_LIB_ONLY:-}" ]] || main "$@"
