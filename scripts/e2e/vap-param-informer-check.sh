#!/usr/bin/env bash
#
# vap-param-informer-check.sh — reproduce the kube-apiserver ValidatingAdmissionPolicy
# param-informer defect (Q444) deterministically, with no chart involved.
#
# The defect: the param informer for a `paramKind` dies permanently, in that
# kube-apiserver process, the moment the set of BINDINGS whose policy names that
# paramKind becomes empty for at least one policy-refresh tick (default 1s).
#
# Why, from staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go:
#   - calculatePolicyData builds usedParams by iterating bindings, so a policy
#     with no binding contributes nothing.
#   - Dropping a paramKind from usedParams calls info.cancelFunc().
#   - For a CORE type, ensureParamsForPolicyLocked takes the informer from the
#     shared informerFactory and calls informerFactory.Start(instanceContext.Done()).
#     sharedInformerFactory marks startedInformers[type]=true and never clears it,
#     so cancelling that context stops a shared informer that Start() will never
#     run again. Its store FREEZES at its last contents rather than emptying.
#   - For a CRD paramKind the fallback path allocates a fresh dynamic informer per
#     context, so it is immune.
#
# Three arms on one apiserver isolate the trigger — object churn, gap length and
# Helm are held constant between them, and only the empty-set transition and the
# paramKind's type differ:
#
#   Arm 1  a second ConfigMap-paramKind binding is held throughout, so usedParams
#          never loses the GVK. The probe binding and its param are still deleted
#          and recreated with a long gap.        Expected: FRESH-PARAM.
#   Arm 3  the SAME empty-set transition as arm 2 below, but against a
#          cluster-scoped CRD paramKind — the shape Q492 migrates the product's
#          PriorityClass guard to. Runs BEFORE arm 2 so no one can attribute a
#          pass to contamination from the ConfigMap break (the two GVKs are
#          independent, but the ordering removes the argument).
#          Expected: FRESH-PARAM — the dynamic-informer path is per-context.
#   Arm 2  the keeper is dropped too, so NO binding names v1/ConfigMap for more
#          than one tick, then everything is restored.
#          Expected: STALE-PARAM or NO-PARAMS — both mean the informer is dead.
#
# It then shows the two consequences directly: the frozen store still answers for
# a ConfigMap deleted from etcd, and a ConfigMap created after the break is
# invisible (`no params found`, the exact error operators report).
#
# Arm 3 is the load-bearing evidence for Q492: arm 2 failing and arm 3 passing on
# one apiserver, under an identical transition, is what makes "move the paramKind
# to a CRD" a fix rather than an inference from reading the source.
#
# DISPOSABLE CLUSTERS ONLY. Arm 2 permanently breaks ConfigMap param resolution
# for the target apiserver process; the only recovery is restarting it. Never
# point this at a shared or managed cluster. The script refuses a non-kind context
# unless ALLOW_NON_KIND=1.
#
# Usage: scripts/e2e/vap-param-informer-check.sh
#   KUBE_CONTEXT    kube context (default kind-q444-lab)
#   GAP             seconds to hold the empty state, must exceed the 1s refresh
#                   tick (default 8)
#   ALLOW_NON_KIND  set to 1 to permit a context whose name does not start "kind-"
set -euo pipefail
shopt -s inherit_errexit

KUBE_CONTEXT="${KUBE_CONTEXT:-kind-q444-lab}"
GAP="${GAP:-8}"
ALLOW_NON_KIND="${ALLOW_NON_KIND:-0}"
readonly KUBE_CONTEXT GAP ALLOW_NON_KIND
readonly NS="vap-param-check"
readonly PARAM="vap-param-check"
readonly PROBE_CRD="vapparamprobes.vap-param-check.test"
readonly API_GROUP="vap-param-check.test"
# Arm 3's own kinds: a cluster-scoped CRD used AS the param (the Q492 shape) and a
# separate probe kind, so the CRD arm shares no object with the ConfigMap arms and
# the two sets of results cannot conflate.
readonly CRD_PARAM_CRD="vapparamconfigs.vap-param-check.test"
readonly CRD_PROBE_CRD="vapcrdprobes.vap-param-check.test"
readonly CRD_PARAM="vap-param-check-crd"

if [[ "${KUBE_CONTEXT}" != kind-* && "${ALLOW_NON_KIND}" != "1" ]]; then
	echo "refusing to run against non-kind context '${KUBE_CONTEXT}'." >&2
	echo "This check permanently breaks ConfigMap param resolution on the target" >&2
	echo "apiserver. Set ALLOW_NON_KIND=1 only for a cluster you can throw away." >&2
	exit 2
fi

kc() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

say() { printf '\n=== %s ===\n' "$*"; }

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11 misses
# that whenever the script ends in an explicit `exit`.
cleanup() {
	kc delete validatingadmissionpolicybinding \
		vap-param-check-probe-binding vap-param-check-keeper-binding \
		vap-param-check-crd-probe-binding vap-param-check-crd-keeper-binding \
		--ignore-not-found --wait=false >/dev/null 2>&1 || true
	kc delete validatingadmissionpolicy \
		vap-param-check-probe vap-param-check-keeper \
		vap-param-check-crd-probe vap-param-check-crd-keeper \
		--ignore-not-found --wait=false >/dev/null 2>&1 || true
	kc delete crd "${PROBE_CRD}" "${CRD_PROBE_CRD}" "${CRD_PARAM_CRD}" \
		--ignore-not-found --wait=false >/dev/null 2>&1 || true
	kc delete namespace "${NS}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

# param_write (re)creates the param ConfigMap carrying a generation token, so a
# probe can tell which generation the policy actually saw.
param_write() {
	local token="$1"
	kc create configmap "${PARAM}" -n "${NS}" --from-literal="token=${token}" \
		--dry-run=client -o yaml | kc apply -f - >/dev/null
}

# probe attempts a CREATE declaring which param generation it expects. The policy
# expression compares params.data.token to object.spec.token, so the answer
# separates the three states the informer can be in.
probe() {
	local name="$1" token="$2"
	kc create -f - 2>&1 <<-EOF || true
		apiVersion: ${API_GROUP}/v1
		kind: VapParamProbe
		metadata:
		  name: ${name}
		  namespace: ${NS}
		spec:
		  token: ${token}
	EOF
}

# verdict maps an apiserver answer to informer state.
#   FRESH-PARAM  informer live, serving the current ConfigMap
#   STALE-PARAM  informer dead, store frozen on an older ConfigMap
#   NO-PARAMS    informer dead, store has no ConfigMap under that key
verdict() {
	local out="$1"
	if grep -q 'no params found' <<<"${out}"; then
		echo "NO-PARAMS"
	elif grep -q 'token mismatch' <<<"${out}"; then
		echo "STALE-PARAM"
	elif grep -q 'created' <<<"${out}"; then
		echo "FRESH-PARAM"
	else
		echo "OTHER(${out})"
	fi
}

# bind creates the binding for one of the two policies. The keeper's binding
# selects a namespace label nothing carries, so it never gates a real request —
# it exists only to hold the GVK in usedParams during arm 1.
bind() {
	local policy="$1" extra=""
	if [[ "${policy}" == "vap-param-check-keeper" ]]; then
		extra=$'  matchResources:\n    namespaceSelector:\n      matchLabels:\n        vap-param-check-keeper: matches-nothing'
	fi
	kc apply -f - >/dev/null <<-EOF
		apiVersion: admissionregistration.k8s.io/v1
		kind: ValidatingAdmissionPolicyBinding
		metadata:
		  name: ${policy}-binding
		spec:
		  policyName: ${policy}
		  validationActions: [Deny]
		  paramRef:
		    name: ${PARAM}
		    namespace: ${NS}
		    parameterNotFoundAction: Deny
		${extra}
	EOF
}

# --- arm 3 helpers: the same three operations against a CRD paramKind ----------

# crd_param_write (re)creates the cluster-scoped param CR carrying a generation
# token. The CRD analogue of param_write.
crd_param_write() {
	local token="$1"
	kc apply -f - >/dev/null <<-EOF
		apiVersion: ${API_GROUP}/v1
		kind: VapParamConfig
		metadata:
		  name: ${CRD_PARAM}
		spec:
		  token: ${token}
	EOF
}

# crd_probe attempts a CREATE against arm 3's own probe kind, gated only by the
# CRD-paramKind policy.
crd_probe() {
	local name="$1" token="$2"
	kc create -f - 2>&1 <<-EOF || true
		apiVersion: ${API_GROUP}/v1
		kind: VapCrdProbe
		metadata:
		  name: ${name}
		  namespace: ${NS}
		spec:
		  token: ${token}
	EOF
}

# bind_crd is bind() for arm 3: a cluster-scoped paramRef (no namespace), which is
# the shape Q492 ships. The keeper again selects a label nothing carries.
bind_crd() {
	local policy="$1" param="${2:-${CRD_PARAM}}" extra=""
	if [[ "${policy}" == "vap-param-check-crd-keeper" ]]; then
		extra=$'  matchResources:\n    namespaceSelector:\n      matchLabels:\n        vap-param-check-keeper: matches-nothing'
	fi
	kc apply -f - >/dev/null <<-EOF
		apiVersion: admissionregistration.k8s.io/v1
		kind: ValidatingAdmissionPolicyBinding
		metadata:
		  name: ${policy}-binding
		spec:
		  policyName: ${policy}
		  validationActions: [Deny]
		  paramRef:
		    name: ${param}
		    parameterNotFoundAction: Deny
		${extra}
	EOF
}

say "setup"
kc apply -f - >/dev/null <<-EOF
	apiVersion: apiextensions.k8s.io/v1
	kind: CustomResourceDefinition
	metadata:
	  name: ${PROBE_CRD}
	spec:
	  group: ${API_GROUP}
	  scope: Namespaced
	  names:
	    plural: vapparamprobes
	    singular: vapparamprobe
	    kind: VapParamProbe
	  versions:
	    - name: v1
	      served: true
	      storage: true
	      schema:
	        openAPIV3Schema:
	          type: object
	          properties:
	            spec:
	              type: object
	              properties:
	                token:
	                  type: string
EOF
kc wait --for=condition=Established "crd/${PROBE_CRD}" --timeout=60s >/dev/null
kc create namespace "${NS}" --dry-run=client -o yaml | kc apply -f - >/dev/null
param_write v1

# Two policies sharing the v1/ConfigMap paramKind: one gates the probe CRD, the
# other only holds the GVK alive.
for policy in vap-param-check-probe vap-param-check-keeper; do
	kc apply -f - >/dev/null <<-EOF
		apiVersion: admissionregistration.k8s.io/v1
		kind: ValidatingAdmissionPolicy
		metadata:
		  name: ${policy}
		spec:
		  failurePolicy: Fail
		  paramKind:
		    apiVersion: v1
		    kind: ConfigMap
		  matchConstraints:
		    resourceRules:
		      - apiGroups: ["${API_GROUP}"]
		        apiVersions: ["v1"]
		        operations: ["CREATE"]
		        resources: ["vapparamprobes"]
		  validations:
		    - expression: "params.data['token'] == object.spec.token"
		      message: "token mismatch: the policy saw a different param generation"
	EOF
done
bind vap-param-check-probe
bind vap-param-check-keeper

say "baseline"
baseline="OTHER(never ran)"
for _ in $(seq 1 30); do
	baseline="$(verdict "$(probe baseline v1)")"
	[[ "${baseline}" == "FRESH-PARAM" ]] && break
	sleep 2
done
echo "baseline (expect FRESH-PARAM): ${baseline}"
if [[ "${baseline}" != "FRESH-PARAM" ]]; then
	echo "baseline never became healthy — cannot interpret the arms." >&2
	exit 1
fi

say "arm 1 — keeper binding held, probe binding and param churned over a ${GAP}s gap"
kc delete validatingadmissionpolicybinding vap-param-check-probe-binding >/dev/null
sleep "${GAP}"
kc delete configmap "${PARAM}" -n "${NS}" >/dev/null
sleep "${GAP}"
param_write v2
bind vap-param-check-probe
sleep "${GAP}"
arm1="$(verdict "$(probe arm1 v2)")"
echo "arm 1 (expect FRESH-PARAM): ${arm1}"

say "arm 3 setup — the same policy shape over a cluster-scoped CRD paramKind"
# The param kind (cluster-scoped, as Q492 ships) and arm 3's own probe kind.
kc apply -f - >/dev/null <<-EOF
	apiVersion: apiextensions.k8s.io/v1
	kind: CustomResourceDefinition
	metadata:
	  name: ${CRD_PARAM_CRD}
	spec:
	  group: ${API_GROUP}
	  scope: Cluster
	  names:
	    plural: vapparamconfigs
	    singular: vapparamconfig
	    kind: VapParamConfig
	  versions:
	    - name: v1
	      served: true
	      storage: true
	      schema:
	        openAPIV3Schema:
	          type: object
	          properties:
	            spec:
	              type: object
	              properties:
	                token:
	                  type: string
	---
	apiVersion: apiextensions.k8s.io/v1
	kind: CustomResourceDefinition
	metadata:
	  name: ${CRD_PROBE_CRD}
	spec:
	  group: ${API_GROUP}
	  scope: Namespaced
	  names:
	    plural: vapcrdprobes
	    singular: vapcrdprobe
	    kind: VapCrdProbe
	  versions:
	    - name: v1
	      served: true
	      storage: true
	      schema:
	        openAPIV3Schema:
	          type: object
	          properties:
	            spec:
	              type: object
	              properties:
	                token:
	                  type: string
EOF
kc wait --for=condition=Established "crd/${CRD_PARAM_CRD}" "crd/${CRD_PROBE_CRD}" --timeout=60s >/dev/null
crd_param_write c1

for policy in vap-param-check-crd-probe vap-param-check-crd-keeper; do
	kc apply -f - >/dev/null <<-EOF
		apiVersion: admissionregistration.k8s.io/v1
		kind: ValidatingAdmissionPolicy
		metadata:
		  name: ${policy}
		spec:
		  failurePolicy: Fail
		  paramKind:
		    apiVersion: ${API_GROUP}/v1
		    kind: VapParamConfig
		  matchConstraints:
		    resourceRules:
		      - apiGroups: ["${API_GROUP}"]
		        apiVersions: ["v1"]
		        operations: ["CREATE"]
		        resources: ["vapcrdprobes"]
		  validations:
		    - expression: "params.spec.token == object.spec.token"
		      message: "token mismatch: the policy saw a different param generation"
	EOF
done
bind_crd vap-param-check-crd-probe
bind_crd vap-param-check-crd-keeper

crd_baseline="OTHER(never ran)"
for _ in $(seq 1 30); do
	crd_baseline="$(verdict "$(crd_probe crd-baseline c1)")"
	[[ "${crd_baseline}" == "FRESH-PARAM" ]] && break
	sleep 2
done
echo "arm 3 baseline (expect FRESH-PARAM): ${crd_baseline}"
if [[ "${crd_baseline}" != "FRESH-PARAM" ]]; then
	echo "arm 3 baseline never became healthy — cannot interpret the CRD arm." >&2
	exit 1
fi

say "arm 3 — every CRD-paramKind binding removed for ${GAP}s, then restored"
# Byte-for-byte the arm 2 transition, one GVK over: drop every binding that names
# the paramKind, hold the empty set past the refresh tick, churn the param, restore.
kc delete validatingadmissionpolicybinding \
	vap-param-check-crd-probe-binding vap-param-check-crd-keeper-binding >/dev/null
sleep "${GAP}"
kc delete vapparamconfig "${CRD_PARAM}" >/dev/null
crd_param_write c2
bind_crd vap-param-check-crd-probe
bind_crd vap-param-check-crd-keeper
sleep "${GAP}"
arm3="$(verdict "$(crd_probe arm3 c2)")"
echo "arm 3 (expect FRESH-PARAM): ${arm3}"

# And the operator-facing consequence that arm 2 produces: a param created AFTER
# the empty-set transition. On the shared-informer path this is the `no params
# found` denial; the dynamic path must see it.
kc apply -f - >/dev/null <<-EOF
	apiVersion: ${API_GROUP}/v1
	kind: VapParamConfig
	metadata:
	  name: ${CRD_PARAM}-fresh
	spec:
	  token: c9
EOF
bind_crd vap-param-check-crd-probe "${CRD_PARAM}-fresh"
sleep "${GAP}"
crd_fresh="$(verdict "$(crd_probe crd-fresh-install c9)")"
echo "arm 3 param created after the transition (expect FRESH-PARAM): ${crd_fresh}"

say "arm 2 — every ConfigMap-paramKind binding removed for ${GAP}s, then restored"
kc delete validatingadmissionpolicybinding \
	vap-param-check-probe-binding vap-param-check-keeper-binding >/dev/null
sleep "${GAP}"
kc delete configmap "${PARAM}" -n "${NS}" >/dev/null
param_write v3
bind vap-param-check-probe
bind vap-param-check-keeper
sleep "${GAP}"
arm2="$(verdict "$(probe arm2 v3)")"
echo "arm 2 (expect STALE-PARAM or NO-PARAMS): ${arm2}"

say "consequences of a dead informer"
# A ConfigMap created after the break is invisible — the operator-facing error.
kc create configmap "${PARAM}-fresh" -n "${NS}" --from-literal=token=v9 >/dev/null
kc patch validatingadmissionpolicybinding vap-param-check-probe-binding --type=merge \
	-p "{\"spec\":{\"paramRef\":{\"name\":\"${PARAM}-fresh\",\"namespace\":\"${NS}\",\"parameterNotFoundAction\":\"Deny\"}}}" >/dev/null
sleep "${GAP}"
fresh="$(verdict "$(probe fresh-install v9)")"
echo "param created after the break (expect NO-PARAMS): ${fresh}"
echo "  the ConfigMap is present: $(kc get cm "${PARAM}-fresh" -n "${NS}" -o jsonpath='{.metadata.name} uid={.metadata.uid}')"

say "RESULT"
printf 'arm 1  ConfigMap, keeper held      : %s\n' "${arm1}"
printf 'arm 2  ConfigMap, binding set empty: %s\n' "${arm2}"
printf 'arm 2  post-break fresh param       : %s\n' "${fresh}"
printf 'arm 3  CRD,       binding set empty: %s\n' "${arm3}"
printf 'arm 3  post-transition fresh param  : %s\n' "${crd_fresh}"

reproduced=0
if [[ "${arm1}" == "FRESH-PARAM" && "${arm2}" != "FRESH-PARAM" && "${fresh}" == "NO-PARAMS" ]]; then
	reproduced=1
	echo
	echo "REPRODUCED: emptying the binding set kills the param informer for this"
	echo "apiserver process. Recovery is a kube-apiserver restart."
fi

crd_immune=0
if [[ "${arm3}" == "FRESH-PARAM" && "${crd_fresh}" == "FRESH-PARAM" ]]; then
	crd_immune=1
	echo
	echo "CRD paramKind IMMUNE: the identical transition that killed the ConfigMap"
	echo "informer left the CRD one serving fresh params, including a param created"
	echo "after the empty-set window. This is the evidence Q492's fix rests on."
fi

if (( reproduced == 1 && crd_immune == 1 )); then
	exit 0
fi
echo
if (( reproduced == 0 )); then
	echo "NOT REPRODUCED — the defect may be fixed on this version, or the run raced."
fi
if (( crd_immune == 0 )); then
	echo "CRD paramKind NOT IMMUNE — Q492's premise (move the paramKind to a CRD)"
	echo "does not hold on this version. Do not ship the migration on this evidence."
fi
exit 1
