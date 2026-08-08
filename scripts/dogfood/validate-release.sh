#!/usr/bin/env bash
# One-command pre-GA dogfood validation gate for a release candidate. Runs the
# whole gate end-to-end and self-cleans on exit:
#
#   deploy (setup.sh) -> route CI (start.sh) -> on-demand e2e (e2e-start.sh)
#   -> dispatch the e2e matrix on GAG runners (gh workflow run, run-scoped
#   routing) -> sizing-profile assertion -> signed v2 CRD artifact smoke ->
#   teardown (e2e-stop.sh + stop.sh)
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
#   * The e2e leg is a workflow_dispatch of E2E_WORKFLOW with the `runner`
#     input pinned to the e2e scale set — routing scoped to that ONE run.
#     vars.GAG_E2E_RUNNER is never touched: the repo-wide flip used to catch
#     other sessions' PRs and merges mid-window, and a caught job wedged main
#     CI when teardown deleted the AGC under it (2026-07-31 incident).
#
# Idempotent: every underlying script is idempotent (guarded creates,
# apply/upsert, --ignore-not-found), so a re-run after a partial failure is
# safe. Self-cleaning: an EXIT trap tears the environment back down to 0 nodes
# on success AND failure — bash runs it on TERM, INT and HUP too, so Ctrl-C and
# an ordinary `kill` self-clean. On failure the trap first dumps a cluster
# snapshot (nodes, pods, events) — teardown's scale-to-0 evicts everything,
# destroying the evidence (e.g. the FailedScheduling reason) a diagnosis needs
# (Q355).
#
# Self-cleaning cannot cover every ending, though. SIGKILL is untrappable, a
# killed parent takes the process group with it, and a teardown killed part-way
# through stops between the two stop scripts — each leaving billable nodes up
# with no process left to release them, found twice only by hunting for a live
# teardown process by hand (Q640). So the gate takes a lease
# (lib/lease.sh) for the window in which it owns cluster state, and reclaims an
# orphaned one — a lease for THIS target whose owning process is gone — before
# it spends anything. Cluster state is never the trigger: a cluster that merely
# has nodes up is what an operator debugging by hand leaves behind, so reclaim
# acts on the lease and nothing else. `--reclaim` runs that step alone.
#
# PROD NOTE: gag-dogfood is hard-classified prod (.claude/prod-guard.json). This
# is a lifecycle script run as `bash validate-release.sh …`, which the prod-guard
# hook does not parse; every cluster write goes through gke_get_credentials_and_verify
# (fail-closed context pinning) in the child scripts, and the wrapper confirms the
# resolved target once before it touches anything. Run it ONLY against dogfood.
#
# Usage:
#   scripts/dogfood/validate-release.sh <rc-tag>
#   scripts/dogfood/validate-release.sh --reclaim
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
#   E2E_WORKFLOW     e2e workflow to dispatch for the matrix (default
#                    e2e-test.yml). Must have the `runner` workflow_dispatch
#                    input on E2E_DISPATCH_REF.
#   E2E_DISPATCH_REF Ref to dispatch the workflow on (default main).
#   E2E_WAIT_TIMEOUT Seconds to wait for an in-flight run of E2E_WORKFLOW to
#                    finish before dispatching, before any billable work
#                    (default 1800; 0 = fail immediately instead of waiting).
#                    The dispatched run enters the workflow's per-ref
#                    concurrency group; entering a busy group parks it in the
#                    single pending slot, where the next push to main cancels it.
#   COSIGN           Path to the cosign binary (default .build/cosign; `make cosign`).
#                    Checked up front, like every other local tool — the CRD smoke
#                    that consumes it runs last, after the billable legs.
#   ASSUME_YES=1     Skip the wrapper's one interactive confirmation (automation).
#
# One-time prerequisite (NOT run here): scripts/dogfood/e2e-setup.sh must have
# provisioned the e2e node pool + GitHub App Secret once. See release.md.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/dogfood/lib/progress.sh
source "${REPO_ROOT}/scripts/dogfood/lib/progress.sh"
# shellcheck source=scripts/dogfood/lib/lease.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease.sh"

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

# WORKDIR holds every artifact the legs write — the sampled worker sizing, the
# downloaded JUnit report, the CRD manifest. main() creates it before the first
# leg runs, because the e2e leg writes into it long before the CRD leg does; the
# EXIT trap removes it.
WORKDIR=""

# The on-demand e2e tenant's namespace and the label the provisioner stamps on
# worker pods (provisioner.LabelRunnerSet), used by the sizing leg to find a
# live worker.
E2E_NAMESPACE="gag-dogfood-e2e"
RUNNER_SET_LABEL="actions-gateway.com/runner-set"

# The runner cpu request NodeShare must derive on an e2e worker: the nodeShare
# envelope declared in deploy/dogfood-e2e/base/resources.yaml divided by its
# workersPerNode (1500m / 1). Kept here rather than recomputed so a drift
# between the manifest and this gate is a loud failure, not a silent pass.
EXPECTED_NODESHARE_CPU="1500m"

# usage.MinSamplesForDrift — the per-template-container sample count Throughput
# needs before it actuates. Mirrored here only for the remediation text; the
# gate never asserts on it (the Throughput leg reports, never fails).
MIN_SAMPLES_FOR_DRIFT="20"

# GAG_E2E_RUNNER_INPUT — the value passed to the workflows' `runner` dispatch
# input: the e2e scale set's runnerLabel, JSON-encoded because the reusable
# workflow resolves the input through fromJSON (same convention as
# vars.GAG_E2E_RUNNER). Must stay in step with runnerLabel in
# deploy/dogfood-e2e/base/resources.yaml.
GAG_E2E_RUNNER_INPUT='"gag-ci-e2e"'

# E2E_RESOLVED_RUN_ID is the run id of the dispatched e2e run. dispatch_e2e_run
# sets it; e2e_leg then watches it. The pre-billable guarantee lives in
# settle_e2e_lane instead — the dispatch itself needs the e2e AGC up first.
E2E_RESOLVED_RUN_ID=""

# COSIGN_BIN is the cosign binary the CRD smoke verifies with. preflight_cosign
# sets it BEFORE any billable work; crd_smoke only consumes it.
COSIGN_BIN=""

# Poll interval for the in-flight waits (run settle + rerun transition).
E2E_POLL_INTERVAL=15

# The gate's phase event stream and its rendered status object default in
# lib/progress.sh (empty either to opt out). Exported so the e2e relay, which
# runs as a child process, folds its heartbeat into the same stream (Q616).
export RELEASE_PROGRESS_FILE RELEASE_STATUS_FILE

usage() {
	cat >&2 <<'USAGE'
Usage: scripts/dogfood/validate-release.sh <rc-tag>
       scripts/dogfood/validate-release.sh --reclaim

Runs the full pre-GA dogfood validation gate for <rc-tag> (e.g. v1.1.0-rc.7)
and tears the environment back down on exit. Requires PROJECT, CLUSTER, ZONE,
and REPO to be exported. See the script header for the optional knobs.

--reclaim runs only the orphaned-run check: if a previous gate against this
target was killed before its teardown finished, its cluster is torn back down
to 0 nodes. Spends nothing otherwise, and never acts on a cluster no lease
claims.
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

# settle_e2e_lane — wait until the latest run of E2E_WORKFLOW is completed.
# Called BEFORE any billable work (no nodes, no deploy, no e2e AGC), because a
# collision is cheap to hit and expensive to discover late: the workflow
# serializes per-ref through a concurrency group with ONE pending slot, so a
# run dispatched into a busy group parks there — where the next push to main
# cancels it, aborting the gate after the scale-up + deploy + e2e AGC. The
# latest run very often *is* in flight: the gate is typically run minutes after
# a merge, whose push-run of e2e-test.yml is still going.
settle_e2e_lane() {
	local workflow="${E2E_WORKFLOW:-e2e-test.yml}"
	local timeout="${E2E_WAIT_TIMEOUT:-1800}"

	echo "Checking the ${workflow} lane is settled before dispatching into it..."
	local run_id
	run_id="$(gh run list --workflow="${workflow}" --repo "${REPO}" \
		-L1 --json databaseId --jq '.[0].databaseId')"
	if [[ -z "${run_id}" ]]; then
		echo "  no prior ${workflow} run — the lane is free."
		return 0
	fi

	local status waited=0
	while :; do
		status="$(run_status "${run_id}")"
		[[ "${status}" == "completed" ]] && break
		if ((waited >= timeout)); then
			echo "error: ${workflow} run ${run_id} is still '${status}' after ${waited}s" >&2
			echo "  Dispatching now would park the gate's run in the concurrency group's" >&2
			echo "  single pending slot, where the next push to main cancels it. Let the" >&2
			echo "  run finish, or raise E2E_WAIT_TIMEOUT (currently ${timeout}s)." >&2
			return 1
		fi
		echo "  run ${run_id} is '${status}' — waiting for it to complete (${waited}s/${timeout}s)..."
		sleep "${E2E_POLL_INTERVAL}"
		waited=$((waited + E2E_POLL_INTERVAL))
	done
	echo "  lane settled — latest ${workflow} run ${run_id} is completed."
}

# latest_dispatch_run_id WORKFLOW — print the newest workflow_dispatch run id of
# WORKFLOW, or nothing when it has never been dispatched.
latest_dispatch_run_id() {
	gh run list --workflow="$1" --repo "${REPO}" \
		--event workflow_dispatch -L1 --json databaseId --jq '.[0].databaseId'
}

# dispatch_e2e_run — trigger E2E_WORKFLOW with its runs-on pinned to the e2e
# scale set for that single run, and set E2E_RESOLVED_RUN_ID to the new run.
# `gh workflow run` prints no run id, so the id is resolved by watching the
# newest workflow_dispatch run change from a pre-dispatch baseline.
dispatch_e2e_run() {
	local workflow="${E2E_WORKFLOW:-e2e-test.yml}"
	local ref="${E2E_DISPATCH_REF:-main}"

	local before
	before="$(latest_dispatch_run_id "${workflow}")"
	echo "Dispatching ${workflow} @ ${ref} routed to ${GAG_E2E_RUNNER_INPUT} (this run only)..."
	gh workflow run "${workflow}" --repo "${REPO}" --ref "${ref}" \
		-f runner="${GAG_E2E_RUNNER_INPUT}"

	local i id
	for ((i = 0; i < 24; i++)); do
		id="$(latest_dispatch_run_id "${workflow}")"
		if [[ -n "${id}" && "${id}" != "${before}" ]]; then
			E2E_RESOLVED_RUN_ID="${id}"
			echo "  dispatched run is ${id}."
			return 0
		fi
		sleep "${E2E_POLL_INTERVAL}"
	done
	echo "error: the dispatched ${workflow} run did not appear within $((24 * E2E_POLL_INTERVAL))s" >&2
	return 1
}

# e2e_leg — spin the on-demand e2e AGC, then dispatch the e2e matrix onto the
# RC's GAG runners (run-scoped routing) and require it green. The e2e-window
# pool sizing belongs to e2e-start.sh (it resizes to at least the derived
# running size, Q357) — a fixed pre-resize here could briefly shrink a larger
# pool and evict a tenant AGC. The AGC comes up before the dispatch: a job
# queued against a scale set that never registers waits forever.
e2e_leg() {
	echo "Spinning up the on-demand e2e tenant (e2e-start.sh)..."
	bash "${SCRIPT_DIR}/e2e-start.sh"

	dispatch_e2e_run
	local run_id="${E2E_RESOLVED_RUN_ID}"

	# Sample a live worker's derived sizing while the matrix runs. Worker pods are
	# ephemeral — released the moment their job finishes — so this is the only
	# window in which the value NodeShare derived can be read at all.
	capture_worker_sizing &
	local capture_pid=$!

	# e2e-run-watch.sh rather than `gh run watch` (Q615): the dispatched run
	# already prints a spec heartbeat every 30s, and gh's watch blocks without
	# relaying it, leaving the operator on job-level status for ~25 minutes.
	# Same exit-status contract — a non-success conclusion fails the gate.
	local watch_rc=0
	echo "Watching run ${run_id} to completion (runners autoscale in; may be slow)..."
	REPO="${REPO}" bash "${SCRIPT_DIR}/e2e-run-watch.sh" "${run_id}" || watch_rc=$?

	# Render the report before acting on the status: a red matrix is exactly
	# when the failing spec names are worth having, and this is the last moment
	# they are cheap to get.
	report_e2e_run "${run_id}"

	if ((watch_rc != 0)); then
		progress_event e2e fail "run ${run_id} did not conclude success"
		wait "${capture_pid}" || true
		return "${watch_rc}"
	fi

	echo "  e2e matrix GREEN"
	wait "${capture_pid}" || true
}

# report_e2e_run <run-id> — render the run's JUnit report into this terminal.
# The dispatched run already writes it to its own job summary; this is the copy
# for an operator who is watching from here and should not have to open a
# browser to find out which spec failed.
#
# Wholly best-effort: an artifact that is missing (a run that died before the
# suite wrote one) must not turn into a second, misleading failure on top of the
# real one.
report_e2e_run() {
	local run_id="$1" dir="${WORKDIR}/e2e-report"
	mkdir -p "${dir}" 2>/dev/null || return 0

	# --pattern: the reusable workflow names the artifact per CNI lane
	# (e2e-junit-report-kindnet / -calico), and the dispatched matrix may
	# produce either.
	if ! gh run download "${run_id}" --repo "${REPO}" \
		--pattern 'e2e-junit-report-*' --dir "${dir}" >/dev/null 2>&1; then
		echo "  (no JUnit artifact on run ${run_id} — skipping the report)"
		return 0
	fi

	local report
	while IFS= read -r report; do
		echo ""
		echo "=== e2e report: $(basename "$(dirname "${report}")") ==="
		bash "${REPO_ROOT}/scripts/e2e/e2e-report-summary.sh" "${report}" || true
	done < <(find "${dir}" -name '*.xml' -type f 2>/dev/null)
}

# capture_worker_sizing records the runner container's cpu request from the
# first e2e worker pod it catches, into a file the sizing leg reads (a
# background function cannot set a parent variable). Best-effort by design: a
# miss is reported, never fatal, because pod churn is not a release defect. The
# hard assertion in sizing_leg is on status.sizingProfileState, which persists
# after every worker is gone.
capture_worker_sizing() {
	local i pod cpu
	for ((i = 0; i < 30; i++)); do
		pod="$(kubectl get pods -n "${E2E_NAMESPACE}" \
			-l "${RUNNER_SET_LABEL}=ci-e2e" \
			-o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
		if [[ -n "${pod}" ]]; then
			cpu="$(kubectl get pod "${pod}" -n "${E2E_NAMESPACE}" \
				-o jsonpath='{.spec.containers[?(@.name=="runner")].resources.requests.cpu}' 2>/dev/null || true)"
			if [[ -n "${cpu}" ]]; then
				printf '%s' "${cpu}" >"${WORKDIR}/e2e-runner-cpu"
				return 0
			fi
		fi
		sleep 10
	done
	return 0
}

# sizing_leg — assert the v1.3 headline sizing profiles actually actuated on
# real workers. Without this the gate can pass having exercised NONE of them:
# a profile that falls back to Static still provisions a healthy pod and still
# runs the matrix green, so every other leg here would report success while the
# release's headline feature sat inert. Same failure shape as Q400/Q404 — a
# gate that cannot observe the thing it gates.
#
# The two profiles are asserted differently ON PURPOSE:
#   NodeShare  — hard failure. It needs no sample history, so it MUST be Active
#                whenever the overlay is applied. Anything else is a defect.
#   Throughput — reported, never fatal. It needs >=20 samples per template
#                container (usage.MinSamplesForDrift), which the CI tenant's
#                ordinary traffic supplies, not this gate's ~7-job matrix. A
#                release must not be blocked on history maturing — but shipping
#                the profile unvalidated must not be SILENT either.
# Binpack is already live-validated (2026-07-25) and is not re-asserted here.
#
# Sampling is UNCONDITIONAL: the sampler tracks every worker pod carrying
# provisioner.LabelRunnerSet in the namespace and the reconciler persists
# status.sizingRecommendation whether or not spec.sizing is set, and the
# aggregate re-seeds from that status — so history accrues without the profile
# and survives stop/start rather than being re-earned. That makes the two
# non-Active states different diagnoses, which is why the report below splits
# them: an EMPTY state means the CR never reached the cluster, and only
# AwaitingSamples means history is genuinely short (Q488).
sizing_leg() {
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"
	echo "Asserting the v1.3 sizing profiles actuated on real workers..."

	local state
	state="$(kubectl get runnerset ci-e2e -n "${E2E_NAMESPACE}" \
		-o jsonpath='{.status.sizingProfileState}' 2>/dev/null || true)"
	if [[ "${state}" != "Active" ]]; then
		echo "error: RunnerSet ci-e2e sizingProfileState is '${state:-<empty>}', want Active." >&2
		echo "  NodeShare needs no sample history, so anything else means the profile is not" >&2
		echo "  actuating and every e2e worker ran on the template's static values instead." >&2
		echo "  Check spec.sizing on deploy/dogfood-e2e/base/resources.yaml reached the cluster." >&2
		return 1
	fi
	echo "  NodeShare: ci-e2e sizingProfileState=Active"

	local observed=""
	[[ -r "${WORKDIR}/e2e-runner-cpu" ]] && observed="$(cat "${WORKDIR}/e2e-runner-cpu")"
	if [[ -z "${observed}" ]]; then
		echo "  NodeShare: derived value NOT checked — no live worker pod was caught during"
		echo "             the matrix. The state assertion above still holds; the pod-level"
		echo "             value is simply unsampled this run."
	elif [[ "${observed}" != "${EXPECTED_NODESHARE_CPU}" ]]; then
		echo "error: e2e worker runner cpu request is '${observed}', want '${EXPECTED_NODESHARE_CPU}'." >&2
		echo "  That is the nodeShare envelope in deploy/dogfood-e2e/base/resources.yaml" >&2
		echo "  divided by workersPerNode. A mismatch means the two drifted apart." >&2
		return 1
	else
		echo "  NodeShare: worker runner cpu request=${observed} (derived — the templates ask 2 and 3)"
	fi

	# Throughput on the always-on CI tenant: report, never fail.
	local ci_state samples
	ci_state="$(kubectl get runnerset ci -n gag-dogfood \
		-o jsonpath='{.status.sizingProfileState}' 2>/dev/null || true)"
	samples="$(kubectl get runnerset ci -n gag-dogfood \
		-o jsonpath='{.status.sizingRecommendation[*].sampleCount}' 2>/dev/null || true)"
	echo "  Throughput: ci sizingProfileState=${ci_state:-<empty>} sampleCounts=[${samples:-none}]"
	if [[ "${ci_state}" == "Active" ]]; then
		echo "             Throughput IS actuating — this RC ran CI on derived sizing."
	else
		echo "             NOT VALIDATED THIS RUN: the profile is not actuating."
		if [[ -z "${ci_state}" ]]; then
			echo "             sizingProfileState is EMPTY — spec.sizing is not on the live"
			echo "             RunnerSet at all, so this is a deploy gap, not a sample gap."
			echo "             A committed CR edit reaches the cluster ONLY via setup.sh's"
			echo "             apply_cr or a direct patch: start.sh resizes the pool and routes"
			echo "             CI but never applies CRs, so no start can deploy it."
			echo "             Fix: re-run scripts/dogfood/setup.sh, or patch the CR directly:"
			echo "               kubectl patch runnersets.v2alpha1.actions-gateway.com/ci \\"
			echo "                 -n gag-dogfood --type=merge -p '{\"spec\":{\"sizing\":{...}}}'"
		else
			echo "             sizingProfileState=${ci_state} — spec.sizing IS deployed, but a"
			echo "             template container is short of ${MIN_SAMPLES_FOR_DRIFT} samples (see the sampleCounts"
			echo "             above for which one). Sampling does not wait on spec.sizing and the"
			echo "             aggregate re-seeds from status.sizingRecommendation, so history is"
			echo "             durable across stop/start and is never re-earned — this wants ~${MIN_SAMPLES_FOR_DRIFT}"
			echo "             jobs of ordinary CI traffic per container, not a multi-day soak."
		fi
		echo "             The RC is not blocked on it, but the profile ships live-unvalidated."
	fi
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
		# Record the terminal state before the diagnostics dump: a renderer
		# watching the stream should learn the gate died now, not after a
		# minute of cluster snapshotting.
		progress_event gate fail "gate exited ${rc}"
		(dump_diagnostics) || echo "diagnostics dump failed — continuing teardown" >&2
	fi
	progress_phase teardown "Scaling dogfood back to 0 nodes at rest"
	echo "=== Teardown (self-cleaning; runs on success and failure) ==="
	bash "${SCRIPT_DIR}/e2e-stop.sh" || echo "e2e-stop failed — continuing teardown" >&2
	bash "${SCRIPT_DIR}/stop.sh" || echo "stop failed — continuing teardown" >&2
	[[ -n "${WORKDIR}" ]] && rm -rf "${WORKDIR}"
	# Release LAST, after the scripts that take the cluster down: the lease is
	# the claim that this run still owns billable state, so dropping it earlier
	# would make a teardown killed mid-drain look like a clean exit and leave
	# the nodes for nobody to reclaim.
	lease_release "${PROJECT}" "${ZONE}" "${CLUSTER}"
	progress_event teardown "done"
	echo "=== Teardown complete (exit ${rc}) ==="
}

# reclaim_orphaned_gate [CONFIRM] — tear down what a killed gate left running,
# before this run spends anything. CONFIRM=1 asks first (the --reclaim entry;
# the gate proper has already confirmed the same target).
#
# The lease is the only trigger. Nodes being up is not evidence of an orphan —
# an operator debugging by hand leaves exactly that state, and tearing theirs
# down would be worse than the leak this fixes — so `free` returns having
# touched nothing, and a `foreign` record (another host's pid, or another
# target) is reported rather than acted on.
#
# A live lease is the two-sessions-at-once case: refuse, and touch nothing. Both
# gates would otherwise fight over the pool size and each other's teardown.
reclaim_orphaned_gate() {
	local confirm="${1:-0}" state
	state="$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"
	case "${state}" in
	free)
		return 0
		;;
	held)
		echo "error: another release gate already owns ${PROJECT}/${ZONE}/${CLUSTER}." >&2
		echo "  $(lease_describe "${PROJECT}" "${ZONE}" "${CLUSTER}")" >&2
		echo "  Two gates on one cluster resize the pool under each other and tear down" >&2
		echo "  each other's tenants. Wait for it to finish, or stop it, then re-run." >&2
		return 1
		;;
	foreign)
		echo "error: ${PROJECT}/${ZONE}/${CLUSTER} has a lease this host cannot judge." >&2
		echo "  $(lease_describe "${PROJECT}" "${ZONE}" "${CLUSTER}")" >&2
		echo "  A pid means nothing off the host that minted it, so this is never" >&2
		echo "  reclaimed automatically. Confirm no gate is running, then delete it." >&2
		return 1
		;;
	esac

	echo "An earlier release gate against this target was killed before it finished tearing down:"
	echo "  $(lease_describe "${PROJECT}" "${ZONE}" "${CLUSTER}")"
	echo "  Its process is gone; the nodes it scaled up are not. Reclaiming them first."
	if ((confirm)); then
		confirm_or_exit "$(printf 'Tear down the cluster that orphaned gate left running?\n  Project: %s\n  Cluster: %s  (zone %s)\nThis routes e2e + CI off GAG and scales the cluster back to 0 nodes.' \
			"${PROJECT}" "${CLUSTER}" "${ZONE}")"
	fi

	# The stop scripts pin the cluster context fail-closed themselves and drain
	# before they delete, so a reclaim cannot scale down under live work.
	export ASSUME_YES=1
	local failed=0
	bash "${SCRIPT_DIR}/e2e-stop.sh" || failed=1
	bash "${SCRIPT_DIR}/stop.sh" || failed=1
	if ((failed)); then
		echo "error: the orphaned run's teardown did not complete." >&2
		echo "  The lease is kept so the next run retries it — the usual cause is a drain" >&2
		echo "  that will not converge, which means live work the stop scripts refuse to" >&2
		echo "  strand. Read their output above; re-run with --reclaim once it clears." >&2
		return 1
	fi
	lease_discard "${PROJECT}" "${ZONE}" "${CLUSTER}"
	echo "  Reclaim complete — the orphaned run's cluster is back at rest."
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
	local reclaim_only=0 rc_tag=""
	if [[ "${1:-}" == "--reclaim" ]]; then
		reclaim_only=1
	else
		rc_tag="${1:-}"
		if [[ -z "${rc_tag}" ]]; then
			echo "error: <rc-tag> is required" >&2
			usage
			exit 2
		fi
	fi

	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"
	require_cmd gh "https://cli.github.com/"

	# --reclaim stops here: it only ever tears down, so it needs neither the
	# deploy toolchain nor an RC tag, and reporting a clean target must stay
	# free enough that a session can run it on any suspicion.
	if ((reclaim_only)); then
		if [[ "$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")" == "free" ]]; then
			echo "No release gate holds ${PROJECT}/${ZONE}/${CLUSTER} — nothing to reclaim."
			echo "  (A cluster left up by something other than this gate is not visible here:"
			echo "  no lease, no reclaim. Check it by hand and use scripts/dogfood/stop.sh.)"
			exit 0
		fi
		reclaim_orphaned_gate 1
		exit 0
	fi

	require_cmd helm "https://helm.sh/docs/intro/install/"
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

	# Reclaim before anything else spends: a gate killed mid-run left this
	# cluster scaled up, and a run that deployed on top of it would inherit the
	# leak instead of ending it. Also the point where a second concurrent gate
	# is refused, cheaply and before the wait below.
	reclaim_orphaned_gate

	# Settle the e2e lane BEFORE the trap arms and before anything billable: a
	# collision with an in-flight run must not cost a cluster cycle.
	settle_e2e_lane

	# Claim the target, then arm the teardown that releases it. The lease is
	# taken here rather than at reclaim time so it spans exactly the window in
	# which this run owns billable state — a settle wait that times out spends
	# nothing and so leaves nothing to reclaim. Two gates that raced through the
	# check above both arrive here; ln decides, and the loser has spent nothing.
	if ! lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "${GAG_IMAGE_TAG}"; then
		echo "error: another release gate claimed ${PROJECT}/${ZONE}/${CLUSTER} while this one waited." >&2
		echo "  $(lease_describe "${PROJECT}" "${ZONE}" "${CLUSTER}")" >&2
		exit 1
	fi

	# Everything below mutates the cluster — arm the self-cleaning teardown first.
	trap teardown EXIT

	# Start the phase stream once the gate is committed to running, so a run
	# that aborts during preflight leaves no half-stream for a renderer to
	# mistake for an in-flight gate.
	progress_init
	# The gate-start detail is the RC tag by contract — it is what the status
	# renderer reports as .rc (lib/progress.sh).
	progress_event gate start "${GAG_IMAGE_TAG}"
	if [[ -n "${RELEASE_STATUS_FILE}" ]]; then
		echo ""
		echo "Progress: ${RELEASE_STATUS_FILE} holds where this gate is, one JSON object,"
		echo "  refreshed on every phase transition and e2e heartbeat. Watch it from an"
		echo "  agent session with: bash ${SCRIPT_DIR}/release-sentinel.sh"
	fi

	# Child scripts run unattended past this point. DOGFOOD_RUNNER_IMAGE is
	# intentionally left as-is (unset => setup.sh preserves the live image, Q295).
	export GAG_IMAGE_TAG APP_ID INSTALLATION_ID
	export ASSUME_YES=1

	WORKDIR="$(mktemp -d "${REPO_ROOT}/tmp/validate-release.XXXXXX")"

	progress_phase deploy "Deploying the RC and routing CI to GAG"
	deploy_leg
	progress_event deploy "done"

	progress_phase e2e "Running the e2e matrix on GAG runners"
	e2e_leg
	progress_event e2e "done"

	progress_phase sizing "Asserting the derived worker sizing profiles"
	sizing_leg
	progress_event sizing "done"

	progress_phase crd-smoke "Verifying the signed v2 CRD artifact"
	crd_smoke
	progress_event crd-smoke "done"

	progress_event gate "done" "validation PASSED for ${GAG_IMAGE_TAG}"
	echo ""
	echo "Validation gate PASSED for ${GAG_IMAGE_TAG}."
	echo "Teardown runs next (scales dogfood back to 0 nodes at rest)."
}

# scripts/dogfood/validate-release-test.sh sources this file to exercise
# settle_e2e_lane and dispatch_e2e_run in isolation (they gate an hour-long
# billable run, so their failure modes are asserted rather than discovered
# live). Only a direct invocation runs the gate.
[[ -n "${VALIDATE_RELEASE_LIB_ONLY:-}" ]] || main "$@"
