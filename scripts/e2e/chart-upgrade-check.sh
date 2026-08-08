#!/usr/bin/env bash
#
# Chart `helm upgrade` check (Q475).
#
# Every test tier in this repo installs the chart into a cluster that has never
# had it before — `make deploy` runs `helm upgrade --install`, but always with no
# prior release, so the `--install` half is the only half ever exercised. Day-2
# upgrade over a LIVE release was untested, and the riskiest claim in the chart
# rests on it: the CRDs ship under templates/crds/ rather than the chart-root
# crds/ directory precisely BECAUSE Helm never upgrades crds/. Moving them (or
# regressing the layout in any other way) would silently stop delivering CRD
# field changes to every existing installation, and nothing would have caught it.
#
# So this check upgrades a live release to a chart that differs in exactly two
# deliberate ways, and asserts both differences arrive:
#
#   1. A new optional property on the RunnerGroup CRD's v1alpha1 spec schema.
#      Asserted end-to-end, not by reading the object: a RunnerGroup carrying the
#      field is PRUNED before the upgrade and ROUND-TRIPS after it. That is proof
#      the apiserver is serving the upgraded schema, which is the property that
#      actually matters, and it fails closed if the CRDs ever move to crds/.
#   2. A pod-template annotation on the GMC Deployment, which forces a real
#      manager rollout — so the check also covers the thing an operator fears
#      most about an upgrade: that the manager comes back and admission still
#      works once it has restarted.
#
# It also asserts a pre-existing tenant object survives the upgrade with its UID
# intact (an upgrade that recreated CRs would destroy every tenant's state), and
# then upgrades BACK to the real chart, asserting the removal is delivered too —
# leaving the cluster as it was found.
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
readonly PROBE_NS="chart-upgrade-check"
readonly RUNNERGROUP_CRD="runnergroups.actions-gateway.github.com"
# The synthetic CRD property and Deployment annotation injected into the upgrade
# candidate. Namespaced so they cannot collide with a real API field or with any
# annotation the chart, cert-manager, or kubectl sets.
readonly MARKER_FIELD="upgradeProbeMarker"
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
	kubectl --context "${KUBE_CONTEXT}" delete namespace "${PROBE_NS}" \
		--ignore-not-found --wait=false >/dev/null 2>&1 || true
	rm -rf "${work_dir}"
}
trap cleanup EXIT

kc() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

# release_revision echoes the release's current Helm revision number.
release_revision() {
	helm get metadata "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" \
		--namespace "${RELEASE_NS}" -o json | jq -r '.revision'
}

# marker_roundtrip PHASE creates a RunnerGroup carrying the marker field and sets
# MARKER_STORED to the value the apiserver stored — empty when the field was
# pruned, i.e. when the live CRD schema does not (yet) declare it. A create that
# failed outright also leaves MARKER_STORED empty but sets MARKER_CREATE_ERR to
# the apiserver's answer: "the field was pruned" and "the create never happened"
# are very different diagnoses and must not read alike.
#
# Results come back through globals rather than stdout because a command
# substitution would run this in a subshell, where MARKER_CREATE_ERR could not
# survive.
#
# --validate=ignore is load-bearing: kubectl's default strict field validation
# would REJECT the unknown field client-side before the upgrade, and the
# rejection text is kubectl/apiserver-version-specific. Letting the apiserver
# prune instead makes the pre- and post-upgrade probes the same one-line read of
# the stored object, with no version-dependent error matching.
#
# The name carries the phase so the post-upgrade create cannot collide with the
# pre-upgrade one and fail AlreadyExists (which would read as a pruned field).
MARKER_STORED=""
MARKER_CREATE_ERR=""
marker_roundtrip() {
	local phase="$1" out
	MARKER_STORED=""
	MARKER_CREATE_ERR=""
	if ! out="$(kc create --validate=ignore -f - 2>&1 <<-EOF
		apiVersion: actions-gateway.github.com/v1alpha1
		kind: RunnerGroup
		metadata:
		  name: marker-${phase}
		  namespace: ${PROBE_NS}
		spec:
		  maxListeners: 1
		  runnerLabels: ["self-hosted"]
		  ${MARKER_FIELD}: ${MARKER_VALUE}
		  podTemplate:
		    spec:
		      containers:
		        - name: runner
		          image: runner:test
	EOF
	)"; then
		MARKER_CREATE_ERR="${out}"
		return 0
	fi
	MARKER_STORED="$(kc get runnergroup -n "${PROBE_NS}" "marker-${phase}" \
		-o jsonpath="{.spec.${MARKER_FIELD}}" 2>/dev/null || true)"
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
			apiVersion: actions-gateway.github.com/v1alpha1
			kind: ActionsGateway
			metadata:
			  name: webhook-probe-${phase}
			  namespace: kube-system
			spec:
			  gitHubURL: https://github.com/example-org
			  gitHubAppRef:
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

# crd_declares_marker echoes the marker property's type from the LIVE RunnerGroup
# CRD ("string" when the upgrade delivered it, empty when it did not).
crd_declares_marker() {
	kc get crd "${RUNNERGROUP_CRD}" -o jsonpath="{.spec.versions[?(@.name=='v1alpha1')]\
.schema.openAPIV3Schema.properties.spec.properties.${MARKER_FIELD}.type}" 2>/dev/null || true
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

step "building the upgrade candidate (a copy of the chart plus two deliberate changes)"
readonly CANDIDATE_DIR="${work_dir}/chart"
cp -R "${CHART_DIR}" "${CANDIDATE_DIR}"

# Change 1 — a new optional property on the RunnerGroup v1alpha1 spec schema.
# The anchor is the `spec:` schema node (10-space indent) followed by its
# `properties:` map (12-space): both appear exactly once in the controller-gen
# output, which is a single-version CRD. The assertion below fails loudly if the
# generator ever restructures that, rather than silently injecting nothing.
readonly CRD_FILE="${CANDIDATE_DIR}/templates/crds/runnergroup-crd.yaml"
awk -v marker="${MARKER_FIELD}" '
	{ print }
	!done && /^          spec:$/ { in_spec = 1; next }
	in_spec && !done && /^            properties:$/ {
		printf "              %s:\n", marker
		print  "                description: Injected by scripts/e2e/chart-upgrade-check.sh (Q475); not a real API field."
		print  "                type: string"
		in_spec = 0
		done = 1
	}
' "${CRD_FILE}" >"${CRD_FILE}.new"
mv "${CRD_FILE}.new" "${CRD_FILE}"
if ! grep -q "^              ${MARKER_FIELD}:$" "${CRD_FILE}"; then
	die "could not inject the probe property into ${RUNNERGROUP_CRD}'s schema — the
       controller-gen layout this script anchors on has changed. Re-point the awk
       anchors in scripts/e2e/chart-upgrade-check.sh at the new spec/properties nodes."
fi

# Change 2 — a pod-template annotation on the GMC Deployment, so the upgrade
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

# A previous run's cleanup deletes the probe namespace with --wait=false, so a
# back-to-back invocation can find it Terminating — which rejects every create
# with "namespace is being terminated" and would look like a pruned field.
for _ in $(seq 1 60); do
	ns_phase="$(kc get namespace "${PROBE_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
	[[ "${ns_phase}" == "Terminating" ]] || break
	sleep 2
done
if [[ "${ns_phase:-}" == "Terminating" ]]; then
	die "namespace ${PROBE_NS} is still Terminating; rerun once it is gone."
fi
kc create namespace "${PROBE_NS}" --dry-run=client -o yaml | kc apply -f - >/dev/null

step "normalizing field ownership (reclaiming any kubectl-patched fields for Helm)"
# A no-op content-wise — the SAME chart the release already runs — so the only
# thing it changes is which field manager owns what. Any harness patch applied on
# top of the release (the e2e suite's --allow-agc-extra-env arg and AGC_EXTRA_*
# env vars) is reverted here; that is intended, and safe, because this check only
# ever runs once the suite has finished.
helm upgrade "${HELM_RELEASE}" "${CHART_DIR}" \
	--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
	-f "${work_dir}/values.yaml" "${FORCE_CONFLICTS[@]}" --wait --timeout 5m >/dev/null

step "baseline: the marker field must NOT exist yet"
# Without this the post-upgrade presence would prove nothing — the field could
# have been in the shipped schema all along.
if [[ -n "$(crd_declares_marker)" ]]; then
	die "the live ${RUNNERGROUP_CRD} already declares .spec.${MARKER_FIELD}. That name is
       supposed to be synthetic; if it became a real API field, rename MARKER_FIELD in
       scripts/e2e/chart-upgrade-check.sh."
fi
marker_roundtrip baseline
if [[ -n "${MARKER_CREATE_ERR}" ]]; then
	die "could not create the baseline probe RunnerGroup, so the pre/post comparison
       cannot be established. The apiserver said:
       ${MARKER_CREATE_ERR}"
fi
if [[ -n "${MARKER_STORED}" ]]; then
	die "the apiserver stored .spec.${MARKER_FIELD}=${MARKER_STORED} BEFORE the upgrade;
       the pre/post comparison this check rests on is meaningless. Is the CRD
       x-kubernetes-preserve-unknown-fields?"
fi
echo "OK [baseline]: the apiserver pruned the unknown field, as it must"

assert_webhook_enforces baseline

# A stand-in for tenant state: an object of a chart-owned CRD that exists before
# the upgrade and must exist, unrecreated, after it. The UID is what makes that
# assertion real — a delete/recreate cycle preserves the name but not the UID.
step "seeding a tenant object that must survive the upgrade"
kc create -f - >/dev/null <<-EOF
	apiVersion: actions-gateway.github.com/v1alpha1
	kind: RunnerGroup
	metadata:
	  name: tenant-survivor
	  namespace: ${PROBE_NS}
	spec:
	  maxListeners: 1
	  runnerLabels: ["self-hosted"]
	  podTemplate:
	    spec:
	      containers:
	        - name: runner
	          image: runner:test
EOF
survivor_uid="$(kc get runnergroup -n "${PROBE_NS}" tenant-survivor -o jsonpath='{.metadata.uid}')"
echo "     tenant-survivor uid=${survivor_uid}"

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

step "the CRD field change must have been delivered"
if [[ -z "$(crd_declares_marker)" ]]; then
	echo "FAIL: the live ${RUNNERGROUP_CRD} does not declare .spec.${MARKER_FIELD} after the" >&2
	echo "  upgrade. \`helm upgrade\` is NOT carrying CRD changes to existing installs — every" >&2
	echo "  operator on a prior release is running a stale CRD schema. The usual cause is the" >&2
	echo "  CRDs having moved from templates/crds/ to the chart-root crds/ directory, which" >&2
	echo "  Helm installs once and never upgrades. See docs/operations/upgrade.md." >&2
	kc get crd "${RUNNERGROUP_CRD}" -o jsonpath='{.metadata.annotations}' >&2 || true
	exit 1
fi
# The schema is in the object; now prove the apiserver is SERVING it. A CRD write
# that landed but whose schema has not propagated to the serving path would still
# prune, and that is the state that actually breaks an operator.
for ((i = 1; i <= 30; i++)); do
	marker_roundtrip "upgraded-${i}"
	[[ -n "${MARKER_STORED}" ]] && break
	sleep 2
done
if [[ "${MARKER_STORED}" != "${MARKER_VALUE}" ]]; then
	echo "FAIL: the upgraded CRD declares .spec.${MARKER_FIELD} but the apiserver still prunes" >&2
	echo "  it (stored value: '${MARKER_STORED:-<pruned>}'). The CRD object was updated but the" >&2
	echo "  served schema never caught up." >&2
	[[ -n "${MARKER_CREATE_ERR}" ]] &&
		echo "  The last probe create also failed: ${MARKER_CREATE_ERR}" >&2
	exit 1
fi
echo "OK: a CRD field change reached the live schema AND the serving path"

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

step "tenant objects must survive the upgrade"
survivor_uid_after="$(kc get runnergroup -n "${PROBE_NS}" tenant-survivor -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
if [[ -z "${survivor_uid_after}" ]]; then
	die "the RunnerGroup created before the upgrade is GONE. An upgrade that deletes tenant
       CRs destroys every tenant's configuration."
fi
if [[ "${survivor_uid_after}" != "${survivor_uid}" ]]; then
	die "the RunnerGroup created before the upgrade was recreated (uid ${survivor_uid} ->
       ${survivor_uid_after}). Tenant objects must be updated in place, never replaced."
fi
echo "OK: tenant-survivor survived with its uid intact"

assert_webhook_enforces upgraded

step "helm upgrade ${HELM_RELEASE} -> back to the real chart"
# The mirror image of the delivery assertion: a REMOVED CRD property must also
# reach the live schema, and the release must be left exactly as it was found.
helm upgrade "${HELM_RELEASE}" "${CHART_DIR}" \
	--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
	-f "${work_dir}/values.yaml" --wait --timeout 5m >/dev/null
mutated_release_live=""

if [[ -n "$(crd_declares_marker)" ]]; then
	die "the probe property survived the upgrade back to the real chart; \`helm upgrade\`
       delivers CRD additions but not removals, and this cluster is left with a
       schema the chart does not describe."
fi
if [[ -n "$(manager_probe_annotation)" ]]; then
	die "the probe annotation survived the upgrade back to the real chart; \`helm upgrade\`
       is not removing fields it previously added."
fi
assert_webhook_enforces restored

echo
echo "OK: a live release upgraded to a changed chart and back — CRD schema changes,"
echo "    ordinary template changes, and their removals were all delivered, tenant"
echo "    objects survived, and admission worked at every step."
