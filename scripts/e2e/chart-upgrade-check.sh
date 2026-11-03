#!/usr/bin/env bash
#
# Chart `helm upgrade` check (Q475).
#
# Every test tier in this repo installs the chart into a cluster that has never
# had it before — `make deploy` runs `helm upgrade --install`, but always with no
# prior release, so the `--install` half is the only half ever exercised. Day-2
# upgrade over a LIVE release is what this check covers.
#
# It upgrades a live release to a chart that differs by one deliberate change, a
# pod-template annotation on the GMC Deployment, which forces a real manager
# rollout — the thing an operator fears most about an upgrade is that the manager
# does not come back, or comes back with admission broken. It asserts the
# annotation arrives and the validating webhook enforces before, during and after,
# then upgrades BACK to the real chart, asserting the removal is delivered too —
# leaving the cluster as it was found.
#
# The chart owns no tenant CRD, so there is no CRD schema change for `helm upgrade`
# to deliver: the v2 kinds ship in the actions-gateway-crds-v2 chart, applied with
# `kubectl apply --server-side`, and the PriorityClassAllowlist CRD sits in the
# chart-root crds/ dir, which docs/operations/upgrade.md applies by hand.
#
# Run against a cluster that already has the chart installed (CI runs it after
# the e2e suite, which leaves the release up under E2E_SKIP_TEARDOWN). The
# release's current values are captured and replayed, so no image refs are
# needed here.
#
# Usage: scripts/e2e/chart-upgrade-check.sh
#   KIND_CLUSTER    kind cluster name (default actions-gateway-e2e); the kube
#                   context is kind-<name>. Set KUBE_CONTEXT to override directly.
#   HELM_RELEASE    release name (default actions-gateway)
#   RELEASE_NS      release namespace (default gmc-system)
set -euo pipefail
shopt -s inherit_errexit

KIND_CLUSTER="${KIND_CLUSTER:-actions-gateway-e2e}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER}}"
HELM_RELEASE="${HELM_RELEASE:-actions-gateway}"
RELEASE_NS="${RELEASE_NS:-gmc-system}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

readonly CHART_DIR="${REPO_ROOT}/charts/actions-gateway"
# The synthetic Deployment annotation injected into the upgrade candidate.
# Namespaced so it cannot collide with any annotation the chart, cert-manager, or
# kubectl sets.
readonly MARKER_ANNOTATION="actions-gateway.github.com/upgrade-probe"
readonly MARKER_VALUE="delivered-by-helm-upgrade"

require_cmd helm "https://helm.sh/docs/intro/install/"
require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
require_cmd jq "https://jqlang.github.io/jq/download/"

work_dir="$(mktemp -d)"
readonly work_dir
# Set once the mutated chart is live, cleared once the real chart is back. The
# trap reads it so a failed assertion mid-cycle still restores the release
# instead of stranding the cluster on a synthetic chart.
mutated_release_live=""
cleanup() {
	if [[ -n "${mutated_release_live}" ]]; then
		echo "restoring the real chart (the check exited before its own restore step)" >&2
		helm upgrade "${HELM_RELEASE}" "${CHART_DIR}" \
			--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
			-f "${work_dir}/values.yaml" --wait --timeout 5m >/dev/null 2>&1 || true
	fi
	rm -rf "${work_dir}"
}
trap cleanup EXIT

kc() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

# release_revision echoes the release's current Helm revision number.
release_revision() {
	helm get metadata "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" \
		--namespace "${RELEASE_NS}" -o json | jq -r '.revision'
}

# assert_webhook_enforces fails unless the GMC validating webhook denies an
# ActionsGateway in a reserved namespace. failurePolicy is Fail, so an unreachable
# webhook produces a connection error rather than this denial — which makes one
# probe distinguish "admission healthy" from both "webhook down" and "webhook up
# but no longer validating".
#
# Retried: the upgrade restarts the manager, and the Service endpoint takes a
# moment to point at the new pod. Only a persistent non-denial is a failure.
assert_webhook_enforces() {
	local phase="$1" answer="" i
	for ((i = 1; i <= 45; i++)); do
		answer="$(kc create -f - 2>&1 <<-EOF || true
			apiVersion: actions-gateway.com/v2
			kind: ActionsGateway
			metadata:
			  name: webhook-probe-${phase}
			  namespace: kube-system
			spec:
			  githubURL: https://github.com/example-org
			  credentials:
			    type: GitHubApp
			    githubApp:
			      name: chart-upgrade-check-nonexistent
		EOF
		)"
		[[ "${answer}" == *"reserved namespace"* ]] && break
		sleep 2
	done
	if [[ "${answer}" != *"reserved namespace"* ]]; then
		echo "FAIL [${phase}]: the GMC validating webhook never denied an ActionsGateway in" >&2
		echo "  kube-system. Last answer from the apiserver:" >&2
		echo "  ${answer}" >&2
		echo "  A connection/timeout error means the webhook is unreachable after the upgrade" >&2
		echo "  (Service endpoints, serving cert, or the manager itself); an admitted create" >&2
		echo "  means the webhook is reachable but no longer validating." >&2
		kc get pods -n "${RELEASE_NS}" -o wide >&2 || true
		return 1
	fi
	echo "OK [${phase}]: the validating webhook is reachable and enforcing"
}

# manager_probe_annotation echoes the marker annotation from the LIVE GMC
# Deployment's pod template (empty when absent). manager_deploy is resolved once,
# below, so this cannot silently read some other Deployment.
manager_probe_annotation() {
	kc get deployment -n "${RELEASE_NS}" "${manager_deploy}" \
		-o jsonpath="{.spec.template.metadata.annotations['actions-gateway\.github\.com/upgrade-probe']}" \
		2>/dev/null || true
}

step "target: context=${KUBE_CONTEXT} release=${HELM_RELEASE} ns=${RELEASE_NS}"
kc config current-context >/dev/null

if ! helm status "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" >/dev/null 2>&1; then
	die "release ${HELM_RELEASE} is not installed in ${RELEASE_NS}; install it first."
fi

# Helm 4 applies server-side, so a field another manager owns makes `helm upgrade`
# fail outright rather than silently overwrite. The e2e suite deliberately
# `kubectl patch`es the GMC Deployment's args (--allow-agc-extra-env, an e2e-only
# knob with no chart value) and `kubectl set env`s its env, which leaves
# kubectl-owned fields on a chart-owned object — so every post-suite upgrade,
# including this check's own, dies on:
#
#   conflict with "kubectl-patch" using apps/v1:
#   .spec.template.spec.containers[name="manager"].args
#
# That is a property of the TEST HARNESS, not of the chart, so the normalize step
# below reclaims ownership with --force-conflicts before any assertion runs. The
# upgrade the assertions actually rest on is then performed WITHOUT it, so a
# genuine ownership conflict introduced by the chart still fails this check.
#
# --force-conflicts is Helm 4 (server-side apply) only. Under Helm 3 there is no
# SSA and therefore no ownership to reclaim, so omitting the flag is correct, not
# a degradation. --force-replace is NOT a substitute: it replaces objects
# wholesale and can be destructive.
FORCE_CONFLICTS=()
if helm help upgrade 2>/dev/null | grep -q -- '--force-conflicts'; then
	FORCE_CONFLICTS=(--force-conflicts)
fi

# Resolved by the release's own selector labels rather than a hardcoded name, so
# a chart that renames the manager does not silently make the Deployment
# assertions read an absent object as "annotation not delivered".
manager_deploy="$(kc get deployment -n "${RELEASE_NS}" \
	-l control-plane=controller-manager,app.kubernetes.io/name=gmc \
	-o jsonpath='{.items[*].metadata.name}')"
if [[ "$(wc -w <<<"${manager_deploy}")" -ne 1 ]]; then
	die "expected exactly one GMC Deployment in ${RELEASE_NS}, found: '${manager_deploy:-<none>}'."
fi
echo "     GMC Deployment: ${manager_deploy}"

step "capturing the installed release's values (replayed on every upgrade below)"
helm get values "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
	-o yaml >"${work_dir}/values.yaml"
# `helm get values` prints the literal "null" for a release installed with no
# user-supplied values; -f null is not a valid overrides file.
if [[ "$(tr -d '[:space:]' <"${work_dir}/values.yaml")" == "null" ]]; then
	echo "{}" >"${work_dir}/values.yaml"
fi

step "building the upgrade candidate (a copy of the chart plus one deliberate change)"
readonly CANDIDATE_DIR="${work_dir}/chart"
cp -R "${CHART_DIR}" "${CANDIDATE_DIR}"

# The change: a pod-template annotation on the GMC Deployment, so the upgrade
# performs a real manager rollout rather than a no-op on the workload side.
readonly DEPLOY_FILE="${CANDIDATE_DIR}/templates/deployment.yaml"
awk -v ann="${MARKER_ANNOTATION}" -v val="${MARKER_VALUE}" '
	{ print }
	!done && /^        kubectl\.kubernetes\.io\/default-container: manager$/ {
		printf "        %s: %s\n", ann, val
		done = 1
	}
' "${DEPLOY_FILE}" >"${DEPLOY_FILE}.new"
mv "${DEPLOY_FILE}.new" "${DEPLOY_FILE}"
if ! grep -q "^        ${MARKER_ANNOTATION}: ${MARKER_VALUE}$" "${DEPLOY_FILE}"; then
	die "could not inject the probe annotation into the GMC Deployment's pod template —
       the anchor annotation (kubectl.kubernetes.io/default-container) is gone.
       Re-point the awk anchor in scripts/e2e/chart-upgrade-check.sh."
fi

step "normalizing field ownership (reclaiming any kubectl-patched fields for Helm)"
# A no-op content-wise — the SAME chart the release already runs — so the only
# thing it changes is which field manager owns what. Any harness patch applied on
# top of the release (the e2e suite's --allow-agc-extra-env arg and AGC_EXTRA_*
# env vars) is reverted here; that is intended, and safe, because this check only
# ever runs once the suite has finished.
helm upgrade "${HELM_RELEASE}" "${CHART_DIR}" \
	--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
	-f "${work_dir}/values.yaml" "${FORCE_CONFLICTS[@]}" --wait --timeout 5m >/dev/null

assert_webhook_enforces baseline

before_revision="$(release_revision)"
step "helm upgrade ${HELM_RELEASE} -> the candidate chart (revision ${before_revision} -> $((before_revision + 1)))"
mutated_release_live="yes"
helm upgrade "${HELM_RELEASE}" "${CANDIDATE_DIR}" \
	--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
	-f "${work_dir}/values.yaml" --wait --timeout 5m >/dev/null

after_revision="$(release_revision)"
if [[ "${after_revision}" -le "${before_revision}" ]]; then
	die "the release revision did not advance (${before_revision} -> ${after_revision});
       helm treated the candidate as a no-op, so nothing below is being tested."
fi

step "the Deployment change must have been delivered, and the manager must be healthy"
live_annotation="$(manager_probe_annotation)"
if [[ "${live_annotation}" != "${MARKER_VALUE}" ]]; then
	echo "FAIL: the GMC Deployment's pod template does not carry ${MARKER_ANNOTATION}" >&2
	echo "  (got '${live_annotation:-<absent>}'). An ordinary template change is not reaching" >&2
	echo "  an existing release." >&2
	exit 1
fi
# `helm upgrade --wait` already blocked on the rollout, so reaching here means the
# restarted manager passed its probes. Say so explicitly — it is half the point.
echo "OK: the pod-template change rolled out and the restarted manager became ready"

assert_webhook_enforces upgraded

step "helm upgrade ${HELM_RELEASE} -> back to the real chart"
# The mirror image of the delivery assertion: a REMOVED annotation must also
# reach the live Deployment, and the release must be left exactly as it was found.
helm upgrade "${HELM_RELEASE}" "${CHART_DIR}" \
	--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
	-f "${work_dir}/values.yaml" --wait --timeout 5m >/dev/null
mutated_release_live=""

if [[ -n "$(manager_probe_annotation)" ]]; then
	die "the probe annotation survived the upgrade back to the real chart; \`helm upgrade\`
       is not removing fields it previously added."
fi
assert_webhook_enforces restored

echo
echo "OK: a live release upgraded to a changed chart and back — the template change"
echo "    and its removal were both delivered, and admission worked at every step."
