#!/usr/bin/env bash
#
# Tests for scripts/manifest/check-webhook-versions.py — the gate holding every
# admission webhook rule to a version some CRD serves (Q1068).
#
# The shipped tree passes, and so would a checker that had stopped reading
# anything, so every case below breaks a copy of the real files and demands the
# verdict change. The central one is a removal: stop serving v2, the one version
# the five actions-gateway.com rules name (Q1150), and all five must fail, because
# that is the state in which admission validation for those kinds would otherwise
# vanish without an error. An admission policy rule fails the same way.
#
# Each case mutates a copy of the real files rather than a hand-written fixture,
# so the shape under test is the one controller-gen actually emits.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
CHECKER="${REPO_ROOT}/scripts/manifest/check-webhook-versions.py"

WEBHOOKS=cmd/gmc/config/webhook/manifests.yaml
V2_CRDS=api/config/crd
CHART_POLICIES=charts/actions-gateway/templates
DEV_POLICIES=cmd/gmc/config/admission-policy

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0
out=""
rc=0

# fixture — a fresh copy of the webhook rules and every CRD under a throwaway root, printed.
fixture() {
	local root="${WORKDIR}/case.$$.${RANDOM}"
	mkdir -p "${root}/$(dirname "${WEBHOOKS}")" "${root}/${V2_CRDS}" "${root}/${CHART_POLICIES}" "${root}/${DEV_POLICIES}"
	cp "${REPO_ROOT}/${WEBHOOKS}" "${root}/${WEBHOOKS}"
	cp "${REPO_ROOT}/${V2_CRDS}"/*.yaml "${root}/${V2_CRDS}/"
	cp "${REPO_ROOT}/${CHART_POLICIES}"/*.yaml "${root}/${CHART_POLICIES}/"
	cp "${REPO_ROOT}/${DEV_POLICIES}"/*.yaml "${root}/${DEV_POLICIES}/"
	printf '%s' "${root}"
}

# run_checker ROOT — run the gate against a fixture root, capturing rc and output.
run_checker() {
	rc=0
	out="$( (cd "$1" && python3 "${CHECKER}") 2>&1 )" || rc=$?
}

expect() {
	local name="$1" want_rc="$2" needle="$3"
	die_if_killed "$name" "$rc" "$want_rc"
	if [[ "${rc}" != "${want_rc}" ]]; then
		echo "FAIL ${name}: want rc ${want_rc}, got ${rc}" >&2
		echo "     output: ${out}" >&2
		fails=$((fails + 1))
		return
	fi
	if [[ -n "${needle}" && "${out}" != *"${needle}"* ]]; then
		echo "FAIL ${name}: '${needle}' not in output" >&2
		echo "     output: ${out}" >&2
		fails=$((fails + 1))
		return
	fi
	echo "ok   ${name}"
}

# retarget FILE OLD NEW — replace OLD with NEW in FILE under a fixture, failing the
# case outright if nothing changed.
retarget() {
	python3 - "$1" "$2" "$3" <<'PY'
import sys
p, old, new = sys.argv[1:]
s = open(p).read()
if old not in s:
    sys.exit(f"retarget: {old!r} not in {p}")
open(p, "w").write(s.replace(old, new))
PY
}

# unserve ROOT VERSION — mark VERSION served: false in every actions-gateway.com CRD,
# failing the case outright if no file changed (a mutation that did not land would
# otherwise read as a gate that correctly passed).
unserve() {
	python3 - "$1/${V2_CRDS}" "$2" <<'PY'
import re, sys
from pathlib import Path
changed = 0
for p in Path(sys.argv[1]).glob("*.yaml"):
    s = p.read_text()
    t = re.sub(rf"(\n    name: {sys.argv[2]}\n(?:.*\n)*?    served: )true", r"\1false", s)
    if t != s:
        p.write_text(t)
        changed += 1
if changed == 0:
    sys.exit(f"unserve: no CRD under {sys.argv[1]} served {sys.argv[2]}")
PY
}

# --- the tree as shipped -----------------------------------------------------

root="$(fixture)"
run_checker "${root}"
expect 'the shipped tree matches' 0 'webhook rules match a served version: 5 rules over 6 CRDs'

# --- an admission policy left on a version nothing serves --------------------
#
# Q1167's defect: tenant-resource-guard named v2alpha1 alone, so dropping that
# version would have stopped it confining the GMC's RunnerSet writes, with no error
# anywhere. Each policy file is its own case, because the dev copy and the chart
# copy are hand-written and drift apart independently.

for file in "${CHART_POLICIES}/tenant-resource-guard.yaml" "${DEV_POLICIES}/tenant-resource-guard.yaml"; do
	root="$(fixture)"
	retarget "${root}/${file}" 'apiVersions: ["v2"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["runnersets"]' 'apiVersions: ["v2alpha1"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["runnersets"]'
	run_checker "${root}"
	expect "a policy rule at an unserved version fails: ${file}" 1 "${file}: names runnersets.actions-gateway.com at v2alpha1"
done

for file in "${CHART_POLICIES}/priorityclass-allowlist-guard.yaml" "${DEV_POLICIES}/priorityclass-allowlist-guard.yaml"; do
	root="$(fixture)"
	retarget "${root}/${file}" 'apiVersion: actions-gateway.com/v2
    kind: PriorityClassAllowlist' 'apiVersion: actions-gateway.com/v2beta1
    kind: PriorityClassAllowlist'
	run_checker "${root}"
	expect "a param kind at an unserved version fails: ${file}" 1 "${file}: names PriorityClassAllowlist param at v2beta1"
done

# The autoscaling.k8s.io rule names a group no CRD here defines, on purpose (Q360).
root="$(fixture)"
run_checker "${root}"
expect 'a policy rule in another group is not this gate'"'"'s business' 0 'admission policy rules too:'

# --- a removal the rules still depend on: v2 is no longer served -------------
#
# The same defect aimed at the version the rules do name. Each actions-gateway.com
# rule names v2 alone, so each must be named.

root="$(fixture)"
unserve "${root}" v2
run_checker "${root}"
expect 'unserving v2 fails the rules that name only it' 1 'at v2, none of which the CRD serves'
for wh in vactionsgateway vclusterrunnertemplate vegressproxy vrunnerset vrunnertemplate; do
	expect "  and names ${wh}-v2" 1 "${wh}-v2.kb.io: names"
done

# --- a rule naming a removed version beside a served one ---------------------
#
# The rule still matches through the version that survives.

root="$(fixture)"
python3 - "${root}/${WEBHOOKS}" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
t = s.replace("    - v2\n", "    - v2\n    - v2beta1\n")
if t == s:
    sys.exit("retarget: no v2 rule to extend")
open(p, "w").write(t)
PY
run_checker "${root}"
expect 'a rule that also names a served version passes' 0 'match a served version'

# --- a whole CRD removed -----------------------------------------------------
#
# Deleting a CRD outright leaves its webhook as unmatched as dropping one
# version does, and the resource no longer appears anywhere to compare against.

root="$(fixture)"
rm "${root}/${V2_CRDS}/actions-gateway.com_runnersets.yaml"
run_checker "${root}"
expect 'a rule whose CRD is gone fails' 1 'vrunnerset-v2.kb.io: names runnersets.actions-gateway.com, which no CRD in this repo defines'

# --- reads that cannot be taken are refusals, never passes ------------------

root="$(fixture)"
printf -- '---\napiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingWebhookConfiguration\n' >"${root}/${WEBHOOKS}"
run_checker "${root}"
expect 'a manifest with no webhooks is refused' 2 'no webhook rules extracted'

root="$(fixture)"
rm "${root}/${V2_CRDS}"/*.yaml
run_checker "${root}"
expect 'no CRDs at all is refused' 2 'no CRDs under'

root="$(fixture)"
python3 - "${root}/${V2_CRDS}/actions-gateway.com_runnersets.yaml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
t = s.replace("\n    served: true\n", "\n    served-by: true\n", 1)
if t == s:
    sys.exit("reshape: no served field to rename")
open(p, "w").write(t)
PY
run_checker "${root}"
expect 'a version entry the parser cannot read is refused' 2 'no name or served field'

root="$(fixture)"
rm "${root}/${CHART_POLICIES}"/*.yaml "${root}/${DEV_POLICIES}"/*.yaml
run_checker "${root}"
expect 'no admission policies at all is refused' 2 'no actions-gateway.com admission policy rules'

if ((fails > 0)); then
	echo "${fails} case(s) failed" >&2
	exit 1
fi
echo "all cases passed"
