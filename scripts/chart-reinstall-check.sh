#!/usr/bin/env bash
#
# Chart uninstall/reinstall check (Q444).
#
# Every other test path in this repo starts from a cluster that has never had the
# chart installed, so nothing exercises what an ordinary operator does on day two:
# `helm uninstall` followed by a reinstall. That gap hid a total outage of the
# product's CRs.
#
# The mechanism: kube-apiserver keeps ONE ValidatingAdmissionPolicy param informer
# per paramKind GVR and tears it down when the last policy naming that GVR is
# deleted. The teardown is permanent for the life of the apiserver process — the
# informer is cached by GVR and the cached instance is already stopped — so a
# policy recreated by a reinstall gets a dead informer with an empty store. Every
# binding resolving a param of that kind then fails with `no params found for
# policy binding`, and under parameterNotFoundAction: Deny that denies EVERY
# matched write (runnergroups, runnersets, runnertemplates) cluster-wide until
# kube-apiserver restarts — impossible on a managed control plane. The chart's fix
# is helm.sh/resource-policy: keep on the paramKind-bearing policy; this script is
# what proves the fix still holds against a real apiserver.
#
# Run against a cluster that already has the chart installed (CI runs it after the
# e2e suite, which leaves the release up under E2E_SKIP_TEARDOWN). The release's
# current values are captured and replayed, so no image refs are needed here.
#
# Usage: scripts/chart-reinstall-check.sh
#   KIND_CLUSTER    kind cluster name (default actions-gateway-e2e); the kube
#                   context is kind-<name>. Set KUBE_CONTEXT to override directly.
#   HELM_RELEASE    release name (default actions-gateway)
#   RELEASE_NS      release namespace (default gmc-system)
set -euo pipefail

KIND_CLUSTER="${KIND_CLUSTER:-actions-gateway-e2e}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER}}"
HELM_RELEASE="${HELM_RELEASE:-actions-gateway}"
RELEASE_NS="${RELEASE_NS:-gmc-system}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly CHART_DIR="${REPO_ROOT}/charts/actions-gateway"
readonly PROBE_NS="chart-reinstall-check"

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
	kubectl --context "${KUBE_CONTEXT}" delete namespace "${PROBE_NS}" \
		--ignore-not-found --wait=false >/dev/null 2>&1 || true
	rm -rf "${work_dir}"
}
trap cleanup EXIT

kc() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

# probe_write attempts a RunnerGroup CREATE and echoes the apiserver's answer.
# RunnerGroups have no validating webhook — the PriorityClass allowlist policy is
# the only admission gate on them — so the answer is unambiguously the policy's.
probe_write() {
	local name="$1" class="${2:-}" pc=""
	[[ -n "${class}" ]] && pc="      priorityClassName: ${class}"
	kc create -f - 2>&1 <<-EOF || true
		apiVersion: actions-gateway.github.com/v1alpha1
		kind: RunnerGroup
		metadata:
		  name: ${name}
		  namespace: ${PROBE_NS}
		spec:
		  maxListeners: 1
		  runnerLabels: ["self-hosted"]
		  podTemplate:
		    spec:
		${pc}
		      containers:
		        - name: runner
		          image: runner:test
	EOF
}

# assert_params_resolve fails loudly on the Q444 signature and on a silent
# non-enforcement, so neither a broken guard nor an absent one passes.
#
# The gating probe is the class-NAMING one, because it is the only write whose
# answer distinguishes every state: the allowlist denial proves the param resolved
# AND the binding is live; `no params found` is unresolved (benign while the
# recreated ConfigMap propagates, the Q444 defect once it persists for the whole
# budget); an admitted write means enforcement has not propagated yet (a binding
# recreated by the reinstall takes a moment to take effect). A class-free write is
# admitted whether or not the guard is bound, so it can only be a follow-up check
# that ordinary writes are not collateral — never the gate.
#
# Names carry the phase and the attempt: a fixed name would turn every retry after
# the first success into AlreadyExists and mask the real answer.
assert_params_resolve() {
	local phase="$1" answer="" plain="" i
	for ((i = 1; i <= 45; i++)); do
		answer="$(probe_write "probe-${phase}-class-${i}" "system-cluster-critical")"
		case "${answer}" in
		*"not in the platform PriorityClass allowlist"*)
			break
			;;
		*"no params found"*)
			# NOT fatal on its own. `helm uninstall` deletes the param ConfigMap and
			# the reinstall recreates it, so until the apiserver observes the new
			# ConfigMap this is the CORRECT fail-closed answer — a couple of seconds
			# in practice. What distinguishes the Q444 breakage is that it never
			# recovers: the param informer is dead for the life of the apiserver
			# process, so every probe in the whole budget below answers this way.
			;;
		*created*)
			# Enforcement not propagated yet; drop the object and keep polling.
			kc delete runnergroup -n "${PROBE_NS}" "probe-${phase}-class-${i}" \
				--ignore-not-found >/dev/null 2>&1 || true
			;;
		esac
		sleep 2
	done
	if [[ "${answer}" == *"no params found"* ]]; then
		echo "FAIL [${phase}]: the policy binding never resolved its param ConfigMap." >&2
		echo "  ${answer}" >&2
		echo "  This is Q444: the apiserver's param informer for this paramKind was torn down" >&2
		echo "  with the policy and cannot be restarted, so it stayed unresolved for the whole" >&2
		echo "  retry budget rather than recovering once the ConfigMap came back. Every" >&2
		echo "  runnergroups/runnersets/runnertemplates write is now denied cluster-wide." >&2
		echo "  Check that the ValidatingAdmissionPolicy still carries helm.sh/resource-policy: keep." >&2
		return 1
	fi
	if [[ "${answer}" != *"not in the platform PriorityClass allowlist"* ]]; then
		echo "FAIL [${phase}]: the guard never denied an off-allowlist PriorityClass; last answer:" >&2
		echo "  ${answer}" >&2
		return 1
	fi

	# ...and ordinary writes must still get through, so a guard that denies
	# everything (the Q444 blast radius) cannot pass as healthy.
	plain="$(probe_write "probe-${phase}-plain")"
	if [[ "${plain}" != *created* ]]; then
		echo "FAIL [${phase}]: a class-free RunnerGroup was not admitted; answer:" >&2
		echo "  ${plain}" >&2
		return 1
	fi
	echo "OK [${phase}]: params resolve and the guard enforces"
}

echo "==> target: context=${KUBE_CONTEXT} release=${HELM_RELEASE} ns=${RELEASE_NS}"
kc config current-context >/dev/null

if ! helm status "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" >/dev/null 2>&1; then
	echo "ERROR: release ${HELM_RELEASE} is not installed in ${RELEASE_NS}; install it first." >&2
	exit 1
fi

echo "==> capturing the installed release's values (replayed on reinstall)"
helm get values "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
	-o yaml >"${work_dir}/values.yaml"

# The policies that MUST survive the uninstall, read from the render of the very
# values this release is running. Asserting their survival is the deterministic
# half of this check: whether a deleted policy actually breaks param resolution
# depends on the apiserver noticing the empty policy set before the reinstall
# recreates it, so the functional probe below can pass by luck on a fast cycle.
# Retention cannot.
param_policies="$(helm template "${HELM_RELEASE}" "${CHART_DIR}" \
	--namespace "${RELEASE_NS}" -f "${work_dir}/values.yaml" |
	awk '
		function flush() {
			if (kind == "ValidatingAdmissionPolicy" && param == 1) { print name }
			kind = ""; name = ""; param = 0
		}
		/^---[[:space:]]*$/ { flush(); next }
		/^[[:space:]]*#/ { next }
		/^kind: / { kind = substr($0, 7) }
		name == "" && /^  name: / { name = substr($0, 9) }
		/^  paramKind:/ { param = 1 }
		END { flush() }
	')"
if [[ -z "${param_policies}" ]]; then
	echo "ERROR: the chart renders no paramKind ValidatingAdmissionPolicy; this check no longer" >&2
	echo "       guards anything — update it alongside whatever replaced the param policy." >&2
	exit 1
fi
echo "==> param-using policies that must survive uninstall:"
echo "${param_policies}" | while read -r p; do echo "     ${p}"; done

# A previous run's cleanup deletes the probe namespace with --wait=false, so a
# back-to-back invocation can find it Terminating — which rejects every create
# with "namespace is being terminated" and would look like an admission verdict.
# Wait it out before creating.
for _ in $(seq 1 60); do
	phase="$(kc get namespace "${PROBE_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
	[[ "${phase}" == "Terminating" ]] || break
	sleep 2
done
if [[ "${phase:-}" == "Terminating" ]]; then
	echo "ERROR: namespace ${PROBE_NS} is still Terminating; rerun once it is gone." >&2
	exit 1
fi
kc create namespace "${PROBE_NS}" --dry-run=client -o yaml | kc apply -f - >/dev/null

echo "==> baseline: admission works before the cycle"
assert_params_resolve baseline

echo "==> helm uninstall ${HELM_RELEASE}"
helm uninstall "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" >/dev/null

echo "==> asserting the param-using policies survived the uninstall"
retention_failed=0
while read -r policy; do
	[[ -n "${policy}" ]] || continue
	if kc get validatingadmissionpolicy "${policy}" >/dev/null 2>&1; then
		echo "     ${policy}: retained"
	else
		echo "FAIL: ValidatingAdmissionPolicy '${policy}' was deleted by helm uninstall." >&2
		echo "  It declares paramKind, so its deletion permanently tears down the apiserver's param" >&2
		echo "  informer for that GVR: the policy the reinstall recreates gets a dead informer with an" >&2
		echo "  empty store, and every runnergroups/runnersets/runnertemplates write is then denied" >&2
		echo "  cluster-wide with 'no params found for policy binding' until kube-apiserver restarts." >&2
		echo "  Restore helm.sh/resource-policy: keep on it (Q444)." >&2
		retention_failed=1
	fi
done <<<"${param_policies}"
((retention_failed == 0)) || exit 1

# Bindings must be gone: they are what make a policy enforce, so a retained one
# would leave the guard active after uninstall and make admissionPolicy.enabled=false
# a silent no-op.
if kc get validatingadmissionpolicybinding -o name 2>/dev/null | grep -q priorityclass; then
	echo "FAIL: a priorityclass policy BINDING survived the uninstall; enforcement never stops." >&2
	kc get validatingadmissionpolicybinding -o name | grep priorityclass >&2
	exit 1
fi

echo "==> helm install ${HELM_RELEASE} (reinstall)"
helm install "${HELM_RELEASE}" "${CHART_DIR}" \
	--kube-context "${KUBE_CONTEXT}" \
	--namespace "${RELEASE_NS}" --create-namespace \
	-f "${work_dir}/values.yaml" >/dev/null

echo "==> after reinstall: admission must still work"
assert_params_resolve reinstall

echo "OK: the chart survives a helm uninstall/reinstall cycle (Q444)"
