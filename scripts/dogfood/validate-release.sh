#!/usr/bin/env bash
# One-command pre-GA dogfood validation gate for a release candidate. Runs the
# whole gate end-to-end and self-cleans on exit:
#
#   deploy (setup.sh) -> route CI (start.sh) -> on-demand e2e (e2e-start.sh)
#   -> re-run the e2e matrix on GAG runners (gh run rerun) -> signed v2 CRD
#   artifact smoke -> teardown (e2e-stop.sh + stop.sh)
#
# It bakes in the env and ordering that the manual runbook
# (docs/operations/release.md § "Validate the release candidate on dogfood")
# spells out, so the gate is a walk-away command instead of an hour of footguns:
#   * APP_ID / INSTALLATION_ID resolved, ASSUME_YES=1 for the child scripts (the
#     App PEM is read from the macOS keychain by setup.sh — never handled here).
#   * DOGFOOD_RUNNER_IMAGE left untouched so setup.sh (Q295) preserves the live
#     RunnerTemplate runner image and a re-run can't regress the toolchain.
#   * The cluster is at 0 nodes at rest — the system pool is scaled up before
#     setup so setup's GMC-rollout wait has something to schedule on, then a
#     temporary +1 node is added for the on-demand e2e AGC's contention window.
#   * The e2e leg is triggered by `gh run rerun` (+ GAG_E2E_RUNNER, set by
#     e2e-start.sh), not by e2e-start.sh itself.
#
# Idempotent: every underlying script is idempotent (guarded creates,
# apply/upsert, --ignore-not-found), so a re-run after a partial failure is
# safe. Self-cleaning: an EXIT trap tears the environment back down to 0 nodes
# on success AND failure.
#
# PROD NOTE: gag-dogfood is hard-classified prod (.claude/prod-guard.json). This
# is a lifecycle script run as `bash validate-release.sh …`, which the prod-guard
# hook does not parse; every cluster write goes through gke_get_credentials_and_verify
# (fail-closed context pinning) in the child scripts, and the wrapper confirms the
# resolved target once before it touches anything. Run it ONLY against dogfood.
#
# Usage:
#   scripts/dogfood/validate-release.sh <rc-tag>
#
# Required env vars (export before running):
#   PROJECT          GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER          GKE cluster name (e.g. gag-dogfood)
#   ZONE             GCP zone (e.g. us-east1-b)
#   REPO             GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#
# Optional env vars:
#   APP_ID           GitHub App numeric ID (default 3752347).
#   INSTALLATION_ID  GitHub App installation ID (auto-resolved from the REPO org
#                    via `gh api` when unset).
#   DOGFOOD_RUNNER_IMAGE  Build-capable RunnerTemplate image. Deliberately NOT
#                    reset here — leave unset to let setup.sh preserve the live
#                    image (Q295); export it only to intentionally re-pin.
#   E2E_WORKFLOW     e2e workflow to re-run for the matrix (default e2e-test.yml).
#   E2E_RUN_ID       Specific run id to re-run (default: the latest E2E_WORKFLOW run).
#   COSIGN           Path to the cosign binary (default .build/cosign; `make cosign`).
#   ASSUME_YES=1     Skip the wrapper's one interactive confirmation (automation).
#
# One-time prerequisite (NOT run here): scripts/dogfood/e2e-setup.sh must have
# provisioned the e2e node pool + GitHub App Secret once. See release.md.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

SCRIPT_DIR="${REPO_ROOT}/scripts/dogfood"
APP_ID_DEFAULT="3752347"

# The five v2 CRDs the signed manifest registers (Q276) — asserted by the smoke.
V2_CRDS=(
	actionsgateways.actions-gateway.com
	clusterrunnertemplates.actions-gateway.com
	egressproxies.actions-gateway.com
	runnersets.actions-gateway.com
	runnertemplates.actions-gateway.com
)

# WORKDIR holds the downloaded CRD artifacts; the EXIT trap removes it.
WORKDIR=""

usage() {
	cat >&2 <<'USAGE'
Usage: scripts/dogfood/validate-release.sh <rc-tag>

Runs the full pre-GA dogfood validation gate for <rc-tag> (e.g. v1.1.0-rc.7)
and tears the environment back down on exit. Requires PROJECT, CLUSTER, ZONE,
and REPO to be exported. See the script header for the optional knobs.
USAGE
}

# resolve_installation_id — print the GitHub App installation ID for APP_ID on
# the REPO's org (Part C1). The org is the slug's first path segment.
resolve_installation_id() {
	local org="${REPO%%/*}"
	gh api "/orgs/${org}/installations" \
		--jq ".installations[] | select(.app_id == ${APP_ID}) | .id"
}

# scale_system_pool N — resize the always-on system pool to N nodes. Mirrors
# start.sh/stop.sh's resize so the wrapper controls the node count around the
# 0-nodes-at-rest deploy and the e2e AGC contention window.
scale_system_pool() {
	local nodes="$1"
	echo "Scaling system pool (default-pool) to ${nodes} node(s)..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool=default-pool --num-nodes="${nodes}" --zone="${ZONE}" --quiet
}

# deploy_leg — bring one system node up, deploy the RC + tenant, route CI to GAG.
deploy_leg() {
	# Pre-scale to 1 BEFORE setup: the cluster is at 0 nodes at rest and setup.sh's
	# GMC-rollout wait (kubectl rollout status, no `|| true`) hard-fails with nothing
	# schedulable — aborting setup before it provisions the tenant (apply_cr). One
	# node up first lets the whole of setup's Part B complete in a single idempotent
	# pass. start.sh reasserts the same 1-node size, so this does not fight it.
	scale_system_pool 1

	echo "Deploying RC ${GAG_IMAGE_TAG} to dogfood (setup.sh)..."
	bash "${SCRIPT_DIR}/setup.sh"

	echo "Routing CI to GAG + completing GMC/AGC rollout (start.sh)..."
	bash "${SCRIPT_DIR}/start.sh"
}

# e2e_leg — add the temporary +1 node, spin the on-demand e2e AGC + routing, then
# re-run the e2e matrix on the RC's GAG runners and require it green.
e2e_leg() {
	local workflow="${E2E_WORKFLOW:-e2e-test.yml}"

	# The on-demand e2e AGC (~500m CPU) does not fit on the single e2-standard-2
	# system node beside the always-on CI AGCs. Add one node for the e2e window;
	# teardown's stop.sh scales the pool back to 0.
	scale_system_pool 2

	echo "Spinning up the on-demand e2e tenant + routing (e2e-start.sh)..."
	bash "${SCRIPT_DIR}/e2e-start.sh"

	local run_id="${E2E_RUN_ID:-}"
	if [[ -z "${run_id}" ]]; then
		echo "Resolving the latest ${workflow} run to re-run..."
		run_id="$(gh run list --workflow="${workflow}" --repo "${REPO}" \
			-L1 --json databaseId --jq '.[0].databaseId')"
	fi
	[[ -n "${run_id}" ]] || {
		echo "no ${workflow} run found to re-run" >&2
		return 1
	}

	echo "Re-running ${workflow} run ${run_id} on GAG runners..."
	gh run rerun "${run_id}" --repo "${REPO}"

	# `gh run rerun` is async: the run reads 'completed' for a moment before it
	# flips to 'queued'. Wait for that transition so `gh run watch` does not read
	# the stale completed status and return before the rerun has even started.
	local status i
	for ((i = 0; i < 12; i++)); do
		status="$(gh run view "${run_id}" --repo "${REPO}" --json status --jq '.status')"
		[[ "${status}" != "completed" ]] && break
		sleep 5
	done

	echo "Watching run ${run_id} to completion (runners autoscale in; may be slow)..."
	gh run watch "${run_id}" --repo "${REPO}" --exit-status
	echo "  e2e matrix GREEN"
}

# crd_smoke — download the RC's signed v2 CRD manifest, verify its blob signature
# against the publish identity, apply it server-side, and assert all five v2 CRDs
# register — the helm-free install path operators actually use.
crd_smoke() {
	local cosign="${COSIGN:-${REPO_ROOT}/.build/cosign}"
	[[ -x "${cosign}" ]] || {
		echo "cosign not found at ${cosign} — download it with: make cosign" >&2
		return 1
	}

	# Re-pin the cluster context (fail-closed) before the apply, independent of
	# which child last fetched credentials.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	WORKDIR="$(mktemp -d "${REPO_ROOT}/tmp/validate-release.XXXXXX")"
	echo "Downloading the signed v2 CRD manifest for ${GAG_IMAGE_TAG}..."
	gh release download "${GAG_IMAGE_TAG}" --repo "${REPO}" \
		--pattern 'actions-gateway-crds-v2.yaml' \
		--pattern 'actions-gateway-crds-v2.yaml.cosign.bundle' \
		--dir "${WORKDIR}" --clobber

	local manifest="${WORKDIR}/actions-gateway-crds-v2.yaml"
	local bundle="${manifest}.cosign.bundle"

	echo "Verifying the manifest's blob signature against the publish identity..."
	"${cosign}" verify-blob --bundle "${bundle}" \
		--certificate-identity-regexp "$(release_identity_regexp)" \
		--certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
		"${manifest}" >/dev/null
	echo "  signature OK"

	echo "Applying the signed manifest (server-side) and asserting registration..."
	kubectl apply --server-side -f "${manifest}"
	local crd
	for crd in "${V2_CRDS[@]}"; do
		kubectl get crd "${crd}" >/dev/null
		echo "  ${crd} OK"
	done
}

# teardown — EXIT trap: route e2e + CI off GAG and scale the cluster back to 0 on
# both success and failure, so a failed gate never strands billable nodes. Each
# step is guarded so one failure does not skip the rest.
teardown() {
	local rc="$?"
	echo ""
	echo "=== Teardown (self-cleaning; runs on success and failure) ==="
	bash "${SCRIPT_DIR}/e2e-stop.sh" || echo "e2e-stop failed — continuing teardown" >&2
	bash "${SCRIPT_DIR}/stop.sh" || echo "stop failed — continuing teardown" >&2
	[[ -n "${WORKDIR}" ]] && rm -rf "${WORKDIR}"
	echo "=== Teardown complete (exit ${rc}) ==="
}

# confirm_target — show the resolved target and require one confirmation before
# any billable/cluster write. Honors ASSUME_YES for the operator (set BEFORE the
# child ASSUME_YES=1 export, so the human still gates the wrapper itself).
confirm_target() {
	confirm_or_exit "$(printf 'About to run the full pre-GA dogfood validation gate:\n  RC tag:  %s\n  Project: %s\n  Cluster: %s  (zone %s)\n  Repo:    %s\nThis scales up billable GKE nodes, deploys the RC, re-runs the e2e matrix on GAG,\nsmoke-tests the signed CRD artifact, then tears the environment back down to 0 nodes.' \
		"${GAG_IMAGE_TAG}" "${PROJECT}" "${CLUSTER}" "${ZONE}" "${REPO}")"
}

main() {
	if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
		usage
		exit 0
	fi
	local rc_tag="${1:-}"
	if [[ -z "${rc_tag}" ]]; then
		echo "error: <rc-tag> is required" >&2
		usage
		exit 2
	fi

	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"
	require_cmd helm "https://helm.sh/docs/intro/install/"
	require_cmd gh "https://cli.github.com/"

	# The positional RC tag is the GAG image/CRD ref for the child scripts.
	GAG_IMAGE_TAG="${rc_tag}"
	APP_ID="${APP_ID:-${APP_ID_DEFAULT}}"
	if [[ -z "${INSTALLATION_ID:-}" ]]; then
		echo "Resolving INSTALLATION_ID for App ${APP_ID} on ${REPO%%/*}..."
		INSTALLATION_ID="$(resolve_installation_id)"
		[[ -n "${INSTALLATION_ID}" ]] || {
			echo "could not resolve an installation for App ${APP_ID} on ${REPO%%/*}" >&2
			exit 1
		}
	fi

	confirm_target

	# Everything below mutates the cluster — arm the self-cleaning teardown first.
	trap teardown EXIT

	# Child scripts run unattended past this point. DOGFOOD_RUNNER_IMAGE is
	# intentionally left as-is (unset => setup.sh preserves the live image, Q295).
	export GAG_IMAGE_TAG APP_ID INSTALLATION_ID
	export ASSUME_YES=1

	deploy_leg
	e2e_leg
	crd_smoke

	echo ""
	echo "Validation gate PASSED for ${GAG_IMAGE_TAG}."
	echo "Teardown runs next (scales dogfood back to 0 nodes at rest)."
}

main "$@"
