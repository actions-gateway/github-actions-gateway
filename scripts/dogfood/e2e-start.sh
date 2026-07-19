#!/usr/bin/env bash
# Spin up the on-demand e2e tenant and route e2e CI jobs to its GAG self-hosted
# runners on GKE. The system pool must already be running (run dogfood/start.sh
# first) and the one-time e2e setup must have run once (dogfood/e2e-setup.sh —
# node pool + Kata runtime + GitHub App Secret).
# See docs/plan/gke-dogfood.md Part F.
#
# On-demand (Q231): the e2e tenant's ~500m-CPU AGC pod is NOT kept always-on —
# it competes with the CI AGC + GMC + Athens on the system pool. This script
# applies the selected worker-isolation overlay to spin the AGC up per e2e run;
# e2e-stop.sh deletes the ActionsGateway to tear it back down.
#
# Capacity (Q335): a single e2-standard-2 system node no longer has ~500m free
# for the on-demand AGC (DaemonSet/kube-dns growth), so it stays Pending and the
# Ready wait below times out. This script scales the system pool up for the e2e
# window; e2e-stop.sh scales it back to the at-rest size (1 node, what
# dogfood/start.sh leaves it in — dogfood/stop.sh later takes it to 0).
#
# Required env vars (export before running):
#   PROJECT      GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER      GKE cluster name (e.g. gag-dogfood)
#   ZONE         GCP zone (e.g. us-east1-b)
#   REPO         GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#
# Optional:
#   E2E_VARIANT  Worker-isolation overlay: "kata" (unprivileged kind in a Kata
#                micro-VM — the default, live-validated green under Q286) or
#                "dind" (privileged DinD — explicit opt-in fallback for
#                environments without nested virtualization, per the
#                secure-by-default rule). Selects
#                deploy/dogfood-e2e/overlays/<variant>; the two share one base,
#                so switching variants is re-running this script.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

E2E_VARIANT="${E2E_VARIANT:-kata}"

# System pool sizing for the e2e window (Q335). The at-rest size (1 node) does
# not fit the on-demand AGC; 2 nodes was live-validated green (see
# docs/plan/archive/kata-on-gke.md#what-the-live-session-found-2026-07-16).
SYSTEM_POOL="${SYSTEM_POOL:-default-pool}"
E2E_SYSTEM_NODES="${E2E_SYSTEM_NODES:-2}"

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	case "${E2E_VARIANT}" in
		dind|kata) ;;
		*)
			echo "error: E2E_VARIANT must be 'dind' or 'kata' (got '${E2E_VARIANT}')" >&2
			exit 1
			;;
	esac

	require_cmd gh "https://cli.github.com/"
	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"

	# Scale the system pool up for the e2e window so the on-demand AGC has room
	# to schedule (Q335). --project/--zone are pinned per call so the resize
	# never relies on the active gcloud config; a resize to the current node
	# count is a no-op, so this is safe to re-run.
	echo "Scaling system pool (${SYSTEM_POOL}) to ${E2E_SYSTEM_NODES} nodes for the e2e window..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool="${SYSTEM_POOL}" --num-nodes="${E2E_SYSTEM_NODES}" --zone="${ZONE}" --quiet

	# Pin the target cluster and fail closed if it is not the active context, so
	# the on-demand tenant apply never lands on another cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	# Spin up the on-demand e2e tenant. Idempotent server-side apply of the
	# selected isolation overlay (namespace + quota + ClusterRunnerTemplate +
	# ActionsGateway + RunnerSet + open-egress NetworkPolicy). The GitHub App
	# Secret is created out-of-band by e2e-setup.sh and is NOT re-applied here.
	# Applying the ActionsGateway is what brings the per-tenant AGC pod up.
	echo "Spinning up the on-demand e2e tenant (v2beta1 ${E2E_VARIANT} overlay)..."
	kubectl apply -k "${REPO_ROOT}/deploy/dogfood-e2e/overlays/${E2E_VARIANT}"

	echo "Waiting for the e2e gateway's AGC to become Ready..."
	kubectl wait --namespace gag-dogfood-e2e \
		--for=condition=Ready actionsgateway/dogfood-e2e --timeout=3m

	# ScaleSet routing (Q231): the v2beta1 RunnerSet declares exactly one
	# runnerLabel (gag-ci-e2e), which is both the runs-on target and the
	# scale-set name registered at GitHub. e2e-reusable.yml resolves
	# fromJSON(vars.GAG_E2E_RUNNER), so the value is a single JSON string, not
	# the old Classic multi-label array.
	gh variable set GAG_E2E_RUNNER \
		--body '"gag-ci-e2e"' \
		--repo "${REPO}"

	echo "E2e jobs will now route to GAG (${E2E_VARIANT} runners on GKE)."
	echo "e2e pool nodes will autoscale 0→2 as jobs arrive."
}

main "$@"
