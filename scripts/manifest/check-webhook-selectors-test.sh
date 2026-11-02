#!/usr/bin/env bash
#
# Tests for scripts/manifest/check-webhook-selectors.py — the gate holding every
# name=~ selector on the apiserver's webhook rejection counter, and the preview
# exporter's webhook list, to the webhooks the chart ships (Q1267).
#
# The shipped tree passes, and so would a checker that had stopped reading
# anything, so every case below breaks a copy of the real files and demands the
# verdict change. The central one is Q1267's own defect: the -v2alpha1 names
# every selector carried after Q1150 renamed the webhooks to -v2. The removal Q273
# makes, deleting the v1alpha1 webhook, must fail until the selectors drop it.
#
# Each case mutates a copy of the real files rather than a hand-written fixture,
# so the shapes under test are the ones the docs and dashboards actually carry.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
CHECKER="${REPO_ROOT}/scripts/manifest/check-webhook-selectors.py"

WEBHOOKS=cmd/gmc/config/webhook/manifests.yaml
ALERT=docs/operations/security-operations.md
TABLE=docs/operations/observability-dashboards.md
DASHBOARD=deploy/monitoring/grafana-dashboard-security.json
EXPORTER=deploy/monitoring/preview/exporter.py

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0
out=""
rc=0

# fixture — a fresh copy of the webhook names and every selector site under a throwaway root, printed.
fixture() {
	local root="${WORKDIR}/case.$$.${RANDOM}" f
	for f in "${WEBHOOKS}" "${ALERT}" "${TABLE}" "${DASHBOARD}" "${EXPORTER}"; do
		mkdir -p "${root}/$(dirname "${f}")"
		cp "${REPO_ROOT}/${f}" "${root}/${f}"
	done
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

# rewrite FILE OLD NEW — replace every OLD with NEW in FILE, failing the case
# outright if nothing changed (a mutation that did not land would otherwise read
# as a gate that correctly passed).
rewrite() {
	python3 - "$@" <<'PY'
import sys
path, old, new = sys.argv[1:]
s = open(path).read()
t = s.replace(old, new)
if t == s:
    sys.exit(f"rewrite: {old!r} not in {path}")
open(path, "w").write(t)
PY
}

# --- the tree as shipped -----------------------------------------------------

root="$(fixture)"
run_checker "${root}"
expect 'the shipped tree matches' 0 '3 selectors and the preview exporter over 6 webhooks'

# --- Q1267: the names every selector carried after Q1150 ---------------------
#
# Each -v2 webhook must be named as missed, in each of the three sites, and the
# v1alpha1 webhook, whose name never changed, must not be.

fixed='actionsgateway-v1alpha1|actionsgateway-v2|clusterrunnertemplate-v2|egressproxy-v2|runnerset-v2|runnertemplate-v2)'
stale='actionsgateway-v1alpha1|actionsgateway-v2alpha1|clusterrunnertemplate-v2alpha1|egressproxy-v2alpha1|runnerset-v2alpha1|runnertemplate-v2alpha1)'
root="$(fixture)"
rewrite "${root}/${ALERT}" "${fixed}" "${stale}"
rewrite "${root}/${DASHBOARD}" "${fixed}" "${stale}"
rewrite "${root}/${TABLE}" "${fixed//|/\\|}" "${stale//|/\\|}"
run_checker "${root}"
expect 'the pre-fix -v2alpha1 selectors fail' 1 'disagree with the webhooks the chart ships'
for f in "${ALERT}" "${TABLE}" "${DASHBOARD}"; do
	for kind in actionsgateway clusterrunnertemplate egressproxy runnerset runnertemplate; do
		expect "  ${f} misses v${kind}-v2" 1 "${f}: does not match v${kind}-v2.kb.io"
	done
done
if [[ "${out}" == *"vactionsgateway-v1alpha1"* ]]; then
	echo "FAIL the v1alpha1 webhook was named, though every selector still matches it" >&2
	fails=$((fails + 1))
else
	echo "ok   the v1alpha1 webhook is not named"
fi

# --- one site alone drifting is caught ---------------------------------------
#
# The dashboard's literal is JSON-escaped and the table's pipes are markdown-
# escaped, so each site is read through a different decoding. Break only one.

root="$(fixture)"
rewrite "${root}/${DASHBOARD}" 'runnerset-v2|' 'runnerset-v3|'
run_checker "${root}"
expect 'a drift in the dashboard alone fails' 1 "${DASHBOARD}: names vrunnerset-v3.kb.io, which the chart does not ship"

root="$(fixture)"
rewrite "${root}/${TABLE}" 'egressproxy-v2\|' ''
run_checker "${root}"
expect 'a webhook dropped from the table alone fails' 1 "${TABLE}: does not match vegressproxy-v2.kb.io"

root="$(fixture)"
rewrite "${root}/${EXPORTER}" '"vrunnerset-v2.kb.io"' '"vrunnerset-v2alpha1.kb.io"'
run_checker "${root}"
expect 'a stale name in the preview exporter fails' 1 "${EXPORTER}: does not match vrunnerset-v2.kb.io"

# --- Q273: the v1alpha1 webhook removed --------------------------------------
#
# Deleting the webhook leaves its alternative in every selector, so all three
# must fail until the selectors drop it, and pass once they do.

root="$(fixture)"
python3 - "${root}/${WEBHOOKS}" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
t = re.sub(r"^- [^\n]*\n(?:  [^\n]*\n|    [^\n]*\n)*?  name: vactionsgateway-v1alpha1\.kb\.io\n(?:  [^\n]*\n)*", "", s, flags=re.M)
if t == s:
    sys.exit("remove: no vactionsgateway-v1alpha1 webhook block")
open(p, "w").write(t)
PY
run_checker "${root}"
expect 'removing the v1alpha1 webhook fails the selectors that still name it' 1 "${ALERT}: names vactionsgateway-v1alpha1.kb.io, which the chart does not ship"
for f in "${ALERT}" "${DASHBOARD}"; do
	rewrite "${root}/${f}" 'v(actionsgateway-v1alpha1|' 'v('
done
rewrite "${root}/${TABLE}" 'v(actionsgateway-v1alpha1\|' 'v('
rewrite "${root}/${EXPORTER}" '"vactionsgateway-v1alpha1.kb.io"' '"vactionsgateway-v1alpha1-retired"'
run_checker "${root}"
expect '  and passes once every site drops it' 0 'over 5 webhooks'

# --- reads that cannot be taken are refusals, never passes ------------------

root="$(fixture)"
rewrite "${root}/${ALERT}" '\\.kb\\.io",' '\\.kb.io",'
run_checker "${root}"
expect 'a selector of another shape is refused' 2 'is not v(<alt>|...)'

root="$(fixture)"
for f in "${ALERT}" "${TABLE}" "${DASHBOARD}"; do
	rewrite "${root}/${f}" 'apiserver_admission_webhook_rejection_count' 'apiserver_admission_webhook_rejections'
done
run_checker "${root}"
expect 'no selector found at all is refused' 2 'no apiserver_admission_webhook_rejection_count name=~ selector'

root="$(fixture)"
printf -- '---\napiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingWebhookConfiguration\n' >"${root}/${WEBHOOKS}"
run_checker "${root}"
expect 'a manifest with no webhooks is refused' 2 'no webhook names extracted'

if ((fails > 0)); then
	echo "${fails} case(s) failed" >&2
	exit 1
fi
echo "all cases passed"
