#!/usr/bin/env bash
# Spin up the on-demand e2e tenant and route e2e CI jobs to its GAG self-hosted
# runners (privileged-DinD, on GKE). The system pool must already be running
# (run dogfood/start.sh first) and the one-time e2e setup must have run once
# (dogfood/e2e-setup.sh — node pool + GitHub App Secret).
# See docs/plan/gke-dogfood.md Part F.
#
# On-demand (Q231): the e2e tenant's ~500m-CPU AGC pod is NOT kept always-on —
# it competes with the CI AGC + GMC + Athens on the single e2-standard-2 system
# node. This script applies the v2beta1 dind overlay to spin the AGC up per e2e
# run; e2e-stop.sh deletes the ActionsGateway to tear it back down.
#
# Required env vars (export before running):
#   PROJECT   GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-east1-b)
#   REPO      GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

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

	# Pin the target cluster and fail closed if it is not the active context, so
	# the on-demand tenant apply never lands on another cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	# Spin up the on-demand e2e tenant. Idempotent server-side apply of the
	# privileged-DinD overlay (namespace + quota + ClusterRunnerTemplate +
	# ActionsGateway + RunnerSet + open-egress NetworkPolicy). The GitHub App
	# Secret is created out-of-band by e2e-setup.sh and is NOT re-applied here.
	# Applying the ActionsGateway is what brings the per-tenant AGC pod up.
	echo "Spinning up the on-demand e2e tenant (v2beta1 dind overlay)..."
	kubectl apply -k "${REPO_ROOT}/deploy/dogfood-e2e/overlays/dind"

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

	echo "E2e jobs will now route to GAG (privileged-DinD runners on GKE)."
	echo "e2e pool nodes will autoscale 0→2 as jobs arrive."
}

main "$@"
