#!/usr/bin/env bash
# Spin up the on-demand e2e tenant on GKE. The system pool must already be
# running (run dogfood/start.sh first) and the one-time e2e setup must have run
# once (dogfood/e2e-setup.sh — node pool + Kata runtime + GitHub App Secret).
# See the GKE dogfood plan (indexed in docs/plan/README.md), Part F.
#
# Routing is NOT wired by default (2026-07-31 incident): flipping the repo-wide
# vars.GAG_E2E_RUNNER routes EVERY e2e job — other sessions' PRs and merges
# included — onto this tenant for as long as it stays set; a job caught mid-
# window wedged main CI when the teardown deleted the AGC under it. Route a
# single run instead via the workflows' `runner` dispatch input (what
# validate-release.sh does), or opt into the repo-wide window with
# E2E_ROUTE_VAR=1 for a standing dogfood soak.
#
# On-demand (Q231): the e2e tenant's ~500m-CPU AGC pod is NOT kept always-on —
# it competes with the CI AGC + GMC + Athens on the system pool. This script
# applies the selected worker-isolation overlay to spin the AGC up per e2e run;
# e2e-stop.sh deletes the ActionsGateway to tear it back down.
#
# Capacity (Q335): a single e2-standard-2 system node no longer has ~500m free
# for the on-demand AGC (DaemonSet/kube-dns growth), so it stays Pending and the
# Ready wait below times out. This script scales the system pool up for the e2e
# window; e2e-stop.sh scales it back to the running size dogfood/start.sh
# leaves it in (derived from the deployed always-on tenants — lib/pool.sh;
# dogfood/stop.sh later takes it to 0).
#
# Required env vars (export before running):
#   PROJECT      GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER      GKE cluster name (e.g. gag-dogfood)
#   ZONE         GCP zone (e.g. us-east1-b)
#   REPO         GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#
# Optional:
#   E2E_ROUTE_VAR=1  Set vars.GAG_E2E_RUNNER to the scale set (repo-wide routing
#                window; e2e-stop.sh resets it). Default: leave routing alone
#                and print the run-scoped dispatch command instead.
#   REGISTRY_MIRROR_PERSISTENT=1
#                Render the PVC-backed registry-mirror overlay, keeping the
#                pull-through layer caches warm across e2e windows at the cost of
#                five continuously-billed disks. Default: ephemeral emptyDir
#                caches. See deploy/registry-mirror/README.md.
#   GMC_ROLLOUT_TIMEOUT
#                How long to wait for the GMC rollout that serves the v2beta1
#                conversion webhook (default: 5m).
#   E2E_VARIANT  Worker-isolation overlay: "kata" (unprivileged kind in a Kata
#                micro-VM — the default, live-validated green under Q286) or
#                "dind" (privileged DinD — explicit opt-in fallback for
#                environments without nested virtualization, per the
#                secure-by-default rule). Selects
#                deploy/dogfood-e2e/overlays/<variant>; the two share one base,
#                so switching variants is re-running this script.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/dogfood/lib/pool.sh
source "${REPO_ROOT}/scripts/dogfood/lib/pool.sh"
# shellcheck source=scripts/dogfood/lib/gmc.sh
source "${REPO_ROOT}/scripts/dogfood/lib/gmc.sh"

E2E_VARIANT="${E2E_VARIANT:-kata}"

# System pool sizing for the e2e window (Q335). 2 nodes was live-validated
# green with both always-on AGCs plus the on-demand e2e AGC (see
# the kata-on-gke plan's 2026-07-16 live-session findings, indexed in
# docs/plan/README.md). The effective size never drops below the derived
# running size (Q357), so
# with a third always-on tenant this resize cannot evict a tenant AGC.
SYSTEM_POOL="${SYSTEM_POOL:-default-pool}"
E2E_SYSTEM_NODES="${E2E_SYSTEM_NODES:-2}"

# The in-cluster registry pull-through cache (Q408) — one Distribution instance
# per upstream registry, plus the additive NetworkPolicy that gives workload pods
# their only registry path. Applied here rather than in e2e-setup.sh so it shares
# the tenant's on-demand lifecycle: five standing pods on the contended system
# pool is exactly the cost Q231 keeps the e2e AGC off it to avoid. e2e-stop.sh
# scales them back to zero.
#
# Idempotent server-side apply. The Kata overlay wires dockerd, the docker-client
# refs, buildkit and helm to these instances (Q408 Phase 3,
# deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml), and the dind overlay does
# not, so both variants are safe to bring up either way: the mirror's additive
# policy is a no-op while the Kata overlay's allow-all e2e-open-egress is still
# in place, and an unwired client simply reaches its upstream.
apply_registry_mirror() {
	# Ephemeral caches by default (emptyDir — $0 at rest, cold on the first pull
	# of each e2e window). Set REGISTRY_MIRROR_PERSISTENT=1 to render the
	# PVC-backed overlay, which keeps the layer caches warm across windows at the
	# cost of five continuously-billed disks. See deploy/registry-mirror/README.md.
	local overlay="${REPO_ROOT}/deploy/registry-mirror"
	if [[ "${REGISTRY_MIRROR_PERSISTENT:-0}" == "1" || "${REGISTRY_MIRROR_PERSISTENT:-0}" == "true" ]]; then
		overlay="${REPO_ROOT}/deploy/registry-mirror/overlays/persistent"
		echo "Applying the in-cluster registry pull-through cache (persistent PVCs)..."
	else
		echo "Applying the in-cluster registry pull-through cache (ephemeral)..."
	fi
	kubectl apply -k "${overlay}"
	echo "  Waiting for the mirror instances to be ready..."
	kubectl wait --namespace gag-registry-mirror \
		--for=condition=Available deployment -l app=registry-mirror --timeout=180s
}

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

	# Pin the target cluster and fail closed if it is not the active context, so
	# the sizing read and the on-demand tenant apply never land on another
	# cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	# Scale the system pool up for the e2e window so the on-demand AGC has room
	# to schedule (Q335), but never below the derived running size — with 3+
	# always-on tenants a resize to the bare e2e default would evict a tenant
	# AGC (Q357). The e2e AGC itself packs into the non-first nodes' headroom
	# (live-validated), so the derived size needs no extra node on top.
	# --project/--zone are pinned per call so the resize never relies on the
	# active gcloud config; a resize to the current node count is a no-op, so
	# this is safe to re-run.
	local nodes needed
	nodes="${E2E_SYSTEM_NODES}"
	needed="$(required_system_nodes)"
	((nodes >= needed)) || nodes="${needed}"
	echo "Scaling system pool (${SYSTEM_POOL}) to ${nodes} nodes for the e2e window..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool="${SYSTEM_POOL}" --num-nodes="${nodes}" --zone="${ZONE}" --quiet

	# After the resize, which is what gives the GMC a node to schedule on, and
	# before the apply its conversion webhook serves.
	wait_for_gmc

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

	# After the AGC wait, which is the bring-up's verdict and should not queue
	# behind five image pulls, and before the routing block below, which is the
	# first point at which a job can reach this tenant.
	apply_registry_mirror

	# ScaleSet routing (Q231): the v2beta1 RunnerSet declares exactly one
	# runnerLabel (gag-ci-e2e), which is both the runs-on target and the
	# scale-set name registered at GitHub. Both routing paths resolve it through
	# fromJSON — the reusable workflow's `runner` input for a single dispatched
	# run, vars.GAG_E2E_RUNNER for the opt-in repo-wide window — so the value is
	# a single JSON string, not the old Classic multi-label array.
	if [[ "${E2E_ROUTE_VAR:-0}" == "1" ]]; then
		gh variable set GAG_E2E_RUNNER \
			--body '"gag-ci-e2e"' \
			--repo "${REPO}"
		echo "ALL e2e jobs now route to GAG (${E2E_VARIANT} runners on GKE) until"
		echo "e2e-stop.sh resets vars.GAG_E2E_RUNNER — including other sessions' PRs."
	else
		echo "Tenant is up (${E2E_VARIANT}). Routing left untouched — send a single run:"
		echo "  gh workflow run e2e-test.yml --repo ${REPO} --ref main -f runner='\"gag-ci-e2e\"'"
		echo "(repo-wide routing is an explicit opt-in: re-run with E2E_ROUTE_VAR=1)"
	fi
	echo "e2e pool nodes will autoscale 0→2 as jobs arrive."
}

[[ -n "${E2E_START_LIB_ONLY:-}" ]] || main "$@"
