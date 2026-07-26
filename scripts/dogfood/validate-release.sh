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
#     setup so setup's GMC-rollout wait has something to schedule on; start.sh
#     and e2e-start.sh then derive the pool size from the deployed tenant AGCs.
#   * The e2e leg is triggered by `gh run rerun` (+ GAG_E2E_RUNNER, set by
#     e2e-start.sh), not by e2e-start.sh itself.
#
# Idempotent: every underlying script is idempotent (guarded creates,
# apply/upsert, --ignore-not-found), so a re-run after a partial failure is
# safe. Self-cleaning: an EXIT trap tears the environment back down to 0 nodes
# on success AND failure. On failure the trap first dumps a cluster snapshot
# (nodes, pods, events) — teardown's scale-to-0 evicts everything, destroying
# the evidence (e.g. the FailedScheduling reason) a diagnosis needs (Q355).
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
#   E2E_RUN_ID       Specific run id to re-run (default: the latest E2E_WORKFLOW
#                    run). Status-checked like the default selection.
#   E2E_WAIT_TIMEOUT Seconds to wait for the selected run to finish if it is still
#                    in flight, before any billable work (default 1800; 0 = fail
#                    immediately instead of waiting).
#   COSIGN           Path to the cosign binary (default .build/cosign; `make cosign`).
#                    Checked up front, like every other local tool — the CRD smoke
#                    that consumes it runs last, after the billable legs.
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

# E2E_RESOLVED_RUN_ID is the run id the e2e leg re-runs. resolve_e2e_run_id sets
# it BEFORE any billable work; e2e_leg only consumes it.
E2E_RESOLVED_RUN_ID=""

# COSIGN_BIN is the cosign binary the CRD smoke verifies with. preflight_cosign
# sets it BEFORE any billable work; crd_smoke only consumes it.
COSIGN_BIN=""

# Poll interval for the in-flight waits (run settle + rerun transition).
E2E_POLL_INTERVAL=15

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
# start.sh/stop.sh's resize so the wrapper can bootstrap the 0-nodes-at-rest
# cluster before setup.sh (the e2e window sizing lives in e2e-start.sh).
scale_system_pool() {
	local nodes="$1"
	echo "Scaling system pool (default-pool) to ${nodes} node(s)..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool=default-pool --num-nodes="${nodes}" --zone="${ZONE}" --quiet
}

# deploy_leg — bring the system pool up, deploy the RC + tenant, route CI to GAG.
deploy_leg() {
	# Pre-scale BEFORE setup: the cluster is at 0 nodes at rest and setup.sh's
	# GMC-rollout wait (kubectl rollout status, no `|| true`) hard-fails with nothing
	# schedulable — aborting setup before it provisions the tenant (apply_cr). Nodes
	# up first let the whole of setup's Part B complete in a single idempotent pass.
	# Size to 2 — the pool floor (scripts/dogfood/lib/pool.sh), enough for setup's
	# GMC wait. start.sh then derives the running size from the deployed tenant
	# AGCs (Q357) and resizes again only if a third tenant needs more.
	scale_system_pool 2

	echo "Deploying RC ${GAG_IMAGE_TAG} to dogfood (setup.sh)..."
	bash "${SCRIPT_DIR}/setup.sh"

	echo "Routing CI to GAG + completing GMC/AGC rollout (start.sh)..."
	bash "${SCRIPT_DIR}/start.sh"
}

# run_status <run-id> — print a run's status field (queued|in_progress|completed…).
run_status() {
	gh run view "$1" --repo "${REPO}" --json status --jq '.status'
}

# resolve_e2e_run_id — pick the e2e run the gate will re-run and set
# E2E_RESOLVED_RUN_ID. Called BEFORE any billable work (no nodes, no deploy, no
# e2e AGC), because the two ways this fails are both cheap to hit and expensive
# to discover late.
#
# `gh run rerun` refuses a run that is still in flight ("This workflow is already
# running"). The latest run very often *is* in flight: the gate is typically run
# minutes after a merge, whose push-run of e2e-test.yml is still going. Selecting
# it inside the e2e leg aborted the gate after the scale-up + deploy + e2e AGC —
# a wasted cluster cycle. So: settle the run here. Waiting (rather than falling
# back to an older completed run) keeps the semantics — the matrix that gets
# re-run is the one for the commit under validation, not a stale one.
resolve_e2e_run_id() {
	local workflow="${E2E_WORKFLOW:-e2e-test.yml}"
	local timeout="${E2E_WAIT_TIMEOUT:-1800}"
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

	local status waited=0
	while :; do
		status="$(run_status "${run_id}")"
		[[ "${status}" == "completed" ]] && break
		if ((waited >= timeout)); then
			echo "error: ${workflow} run ${run_id} is still '${status}' after ${waited}s" >&2
			echo "  gh run rerun cannot re-run an in-flight run. Let it finish, raise" >&2
			echo "  E2E_WAIT_TIMEOUT (currently ${timeout}s), or pin a completed run with" >&2
			echo "  E2E_RUN_ID=<id>." >&2
			return 1
		fi
		echo "  run ${run_id} is '${status}' — waiting for it to complete (${waited}s/${timeout}s)..."
		sleep "${E2E_POLL_INTERVAL}"
		waited=$((waited + E2E_POLL_INTERVAL))
	done

	E2E_RESOLVED_RUN_ID="${run_id}"
	echo "Will re-run ${workflow} run ${run_id} once the e2e tenant is routed."
}

# e2e_leg — spin the on-demand e2e AGC + routing, then re-run the pre-resolved
# e2e matrix on the RC's GAG runners and require it green. The e2e-window pool
# sizing belongs to e2e-start.sh (it resizes to at least the derived running
# size, Q357) — a fixed pre-resize here could briefly shrink a larger pool and
# evict a tenant AGC.
e2e_leg() {
	local run_id="${E2E_RESOLVED_RUN_ID}"

	echo "Spinning up the on-demand e2e tenant + routing (e2e-start.sh)..."
	bash "${SCRIPT_DIR}/e2e-start.sh"

	echo "Re-running run ${run_id} on GAG runners..."
	gh run rerun "${run_id}" --repo "${REPO}"

	# `gh run rerun` is async: the run reads 'completed' for a moment before it
	# flips to 'queued'. Wait for that transition so `gh run watch` does not read
	# the stale completed status and return before the rerun has even started.
	local status i
	for ((i = 0; i < 12; i++)); do
		status="$(run_status "${run_id}")"
		[[ "${status}" != "completed" ]] && break
		sleep 5
	done

	echo "Watching run ${run_id} to completion (runners autoscale in; may be slow)..."
	gh run watch "${run_id}" --repo "${REPO}" --exit-status
	echo "  e2e matrix GREEN"
}

# preflight_cosign — resolve the cosign binary the CRD smoke needs and set
# COSIGN_BIN. Called BEFORE any billable work: the smoke is the LAST leg, so a
# missing binary discovered there aborts the gate ~25 minutes in, after a full
# node scale-up + deploy + e2e cycle. Failing here is free.
preflight_cosign() {
	local cosign="${COSIGN:-${REPO_ROOT}/.build/cosign}"
	[[ -x "${cosign}" ]] || {
		echo "cosign not found at ${cosign} — download it with: make cosign" >&2
		echo "  (checked up front: the CRD smoke that needs it runs last, after" >&2
		echo "  the billable legs)" >&2
		return 1
	}
	COSIGN_BIN="${cosign}"
}

# crd_smoke — download the RC's signed v2 CRD manifest, verify its blob signature
# against the publish identity, apply it server-side, and assert all five v2 CRDs
# register — the helm-free install path operators actually use. Consumes the
# COSIGN_BIN that preflight_cosign resolved before anything billable ran.
crd_smoke() {
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
	"${COSIGN_BIN}" verify-blob --bundle "${bundle}" \
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

# dump_diagnostics — snapshot the cluster's scheduling state to stdout before
# teardown destroys it. Scaling the pool to 0 cordons the nodes and evicts
# every pod, which erases the FailedScheduling (or crash-loop) evidence that
# explains a failed gate — so diagnosing a failure used to need a second
# billable run just to watch it happen again (Q355). Runs only on failure,
# from a subshell in teardown(): the context pin re-runs fail-closed here (the
# failure may predate any credential fetch, and its `exit` on a wrong context
# only kills the subshell), and every step is best-effort so a broken or
# unreachable cluster can never block the teardown that follows.
dump_diagnostics() {
	echo "=== Failure diagnostics (cluster snapshot before teardown evicts it) ==="
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}" || {
		echo "could not pin the cluster context — skipping the snapshot" >&2
		return 0
	}
	echo "--- Nodes ---"
	kubectl get nodes -o wide || true
	echo "--- Pods (all namespaces) ---"
	kubectl get pods -A -o wide || true
	echo "--- Unhealthy pod detail (scheduling/image/crash reasons) ---"
	local ns name _rest
	kubectl get pods -A --no-headers \
		--field-selector 'status.phase!=Running,status.phase!=Succeeded' 2>/dev/null |
		while read -r ns name _rest; do
			kubectl describe pod -n "${ns}" "${name}" || true
		done || true
	echo "--- Events (all namespaces, oldest first) ---"
	kubectl get events -A --sort-by=.lastTimestamp || true
	echo "=== End failure diagnostics ==="
}

# teardown — EXIT trap: route e2e + CI off GAG and scale the cluster back to 0 on
# both success and failure, so a failed gate never strands billable nodes. Each
# step is guarded so one failure does not skip the rest. On failure, diagnostics
# are captured FIRST — the scale-down below destroys the evidence.
#
# stop.sh waits for in-flight workers before scaling down and fails rather than
# stranding them (Q434), so the guard below can leave the system pool up. That
# is the cheaper failure: two e2-standard-2 nodes still billing beats worker
# nodes pinned by pods no AGC is left alive to reap. Its error names the
# remedy — re-run stop.sh once the drain finishes.
teardown() {
	local rc="$?"
	echo ""
	if ((rc != 0)); then
		(dump_diagnostics) || echo "diagnostics dump failed — continuing teardown" >&2
	fi
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
	# Not require_cmd: cosign is a repo-local pinned download, not a PATH tool.
	preflight_cosign

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

	# Resolve (and settle) the e2e run BEFORE the trap arms and before anything
	# billable: a collision with an in-flight run must not cost a cluster cycle.
	resolve_e2e_run_id

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

# scripts/dogfood/validate-release-test.sh sources this file to exercise
# resolve_e2e_run_id in isolation (it gates an hour-long billable run, so its
# failure modes are asserted rather than discovered live). Only a direct
# invocation runs the gate.
[[ -n "${VALIDATE_RELEASE_LIB_ONLY:-}" ]] || main "$@"
