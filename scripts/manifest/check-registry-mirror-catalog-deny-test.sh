#!/usr/bin/env bash
#
# Tests for scripts/manifest/check-registry-mirror-catalog-deny.py — the gate
# holding every mirror instance behind the deny proxy that refuses
# /v2/_catalog (Q1022).
#
# Why every drift class is asserted rather than just the happy path. This gate
# reads the sources; check-registry-mirror-render.sh renders them (Q1024), and
# neither substitutes for the other — a deny sidecar dropped from a Deployment
# renders perfectly. The cluster battery that would catch a regression needs a
# booked dogfood session, so this gate is the only thing
# between the drift and a shared cache handing one tenant the list of what every
# other tenant pulled. A gate that stopped firing would be indistinguishable
# from a tree that is whole — which is the state the real tree is in, and why
# "it passes" proves nothing on its own.
#
# Each case mutates a copy of the real files rather than a hand-written fixture:
# a fixture asserts the shape its author had in mind, and the shape that matters
# is the one the repo actually ships.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
CHECKER="${REPO_ROOT}/scripts/manifest/check-registry-mirror-catalog-deny.py"

BASE=deploy/registry-mirror/base
SHARED=deploy/registry-mirror/components/shared-tenants/kustomization.yaml

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0
out=""
rc=0

# fixture — a fresh copy of the six files under a throwaway root, printed.
fixture() {
	local root="${WORKDIR}/case.$$.${RANDOM}" f
	mkdir -p "${root}/${BASE}" "${root}/$(dirname "${SHARED}")"
	for f in deployment.yaml catalog-deny.cfg kustomization.yaml service.yaml networkpolicy.yaml; do
		cp "${REPO_ROOT}/${BASE}/${f}" "${root}/${BASE}/${f}"
	done
	cp "${REPO_ROOT}/${SHARED}" "${root}/${SHARED}"
	printf '%s' "${root}"
}

run_checker() {
	rc=0
	out="$( (cd "$1" && python3 "${CHECKER}") 2>&1 )" || rc=$?
}

# edit ROOT FILE OLD NEW — a literal substitution, refusing when it matched
# nothing. A mutation that silently did not land leaves the gate reading clean
# input, and that green is indistinguishable from a gate that caught nothing.
edit() {
	python3 - "$1/$2" "$3" "$4" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path).read()
if old not in s:
    sys.exit(f"mutation did not match in {path}: {old!r}")
open(path, "w").write(s.replace(old, new))
PY
}

# expect_absent NAME RC NEEDLE — like expect, but the needle must NOT appear.
# For a fix whose whole effect is that output goes away: `expect` can only assert
# a substring is present, so a guard that suppresses noise has nothing it can
# fail on and the case beside it passes on the unfixed script too.
expect_absent() {
	local name="$1" want_rc="$2" needle="$3"
	die_if_killed "$name" "$rc" "$want_rc"
	if [[ "${rc}" != "${want_rc}" ]]; then
		echo "FAIL ${name}: want rc ${want_rc}, got ${rc}" >&2
		echo "     output: ${out}" >&2
		fails=$((fails + 1))
		return
	fi
	if [[ "${out}" == *"${needle}"* ]]; then
		echo "FAIL ${name}: '${needle}' unexpectedly present" >&2
		echo "     output: ${out}" >&2
		fails=$((fails + 1))
		return
	fi
	echo "ok   ${name}"
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

echo "scripts/manifest/check-registry-mirror-catalog-deny-test.sh"

# --- the tree as shipped -----------------------------------------------------

root="$(fixture)"
run_checker "${root}"
expect 'the shipped tree is whole' 0 'fronts all 5 mirror instances'

# --- an instance with no deny container --------------------------------------
#
# The drift this gate exists for: a sixth upstream added by copying a
# Deployment block and dropping the sidecar with it.

root="$(fixture)"
edit "${root}" "${BASE}/deployment.yaml" '        - name: catalog-deny
          image: haproxy' '        - name: catalog-deny-disabled
          image: haproxy'
run_checker "${root}"
expect 'an instance with no deny container fails' 1 'no catalog-deny container'

# --- a registry back on the pod network --------------------------------------
#
# The same hole by a different route: the proxy is still there and no longer the
# only way in, because the registry answers on its own address too.

root="$(fixture)"
edit "${root}" "${BASE}/deployment.yaml" 'value: 127.0.0.1:5002' 'value: 0.0.0.0:5002'
run_checker "${root}"
expect 'a registry on the pod network fails' 1 'which is on the pod network'

root="$(fixture)"
edit "${root}" "${BASE}/deployment.yaml" '            - name: REGISTRY_HTTP_ADDR
              value: 127.0.0.1:5002
' ''
run_checker "${root}"
expect 'a registry with no bind address fails' 1 'sets REGISTRY_HTTP_ADDR 0 times'

# --- ports that stop agreeing ------------------------------------------------

root="$(fixture)"
edit "${root}" "${BASE}/deployment.yaml" '        - name: catalog-deny
          image: haproxy:3.2.23-alpine@sha256:93de1368b406157be4cded231bb34336e8477e8db24e90f5fb830bec99142331
          ports:
            - containerPort: 5000' '        - name: catalog-deny
          image: haproxy:3.2.23-alpine@sha256:93de1368b406157be4cded231bb34336e8477e8db24e90f5fb830bec99142331
          ports:
            - containerPort: 5005'
run_checker "${root}"
expect 'a deny container off the admitted port fails' 1 'but 5000 is the port the Services target'

# networkpolicy.yaml holds a second, independent policy. A port added to the
# WORKER-side egress rule is nobody's business but that rule's, and the gate must
# not read it as the mirror's admitted port. This case demands rc 0 while its
# neighbours demand 1 -- that is the assertion, not a typo.
root="$(fixture)"
edit "${root}" "${BASE}/networkpolicy.yaml" '      ports:
        - protocol: TCP
          port: 5000
---' '      ports:
        - protocol: TCP
          port: 5000
        - protocol: TCP
          port: 5443
---'
run_checker "${root}"
expect 'an unrelated port on the worker policy is not the mirror'\''s' 0 'fronts all 5 mirror instances'

# The component's patch replaces the base's ingress wholesale, so its restated
# port is a second copy that can drift on its own. Its own header says so.
root="$(fixture)"
edit "${root}" "${SHARED}" '                port: 5000' '                port: 5001'
run_checker "${root}"
expect 'a shared component off the base port fails' 1 'admits 5001, but the base admits 5000'

# --- the pod label the two halves must share ---------------------------------
#
# The worker-side egress rule says which pods may leave for the mirror; the
# shared component's ingress peer restates that label as the one it admits
# (Q1026). Change either alone and workers lose the path entirely — fail-closed,
# and first read as a booked Kata window in which nothing pulls (Q1030).
#
# check-registry-mirror-render.sh caught only one of the two directions before
# this gate did: measured 2026-09-09 on both seeds below, it fails the component
# side (its render loses the literal it pins) and passes the base side at exit 0,
# because nothing rendered compares the two.

root="$(fixture)"
edit "${root}" "${BASE}/networkpolicy.yaml" 'spec:
  podSelector:
    matchLabels:
      actions-gateway/component: workload' 'spec:
  podSelector:
    matchLabels:
      actions-gateway/component: runner'
run_checker "${root}"
expect 'a worker label moved alone fails' 1 "lets out pods labelled ['actions-gateway/component: runner']"

root="$(fixture)"
edit "${root}" "${SHARED}" '                    actions-gateway/component: workload' '                    actions-gateway/component: runner'
run_checker "${root}"
expect 'an ingress peer moved alone fails' 1 "admits pods labelled ['actions-gateway/component: runner']"

# A reconciliation, not a third copy of the string: a rename carried through both
# files must pass HERE. A gate pinning the literal instead would fail, which is
# the difference this case exists to hold.
#
# It is not an assertion that such a rename is safe, and the distinction is worth
# keeping straight: the same literal is set in Go (cmd/agc/.../pod.go,
# cmd/gmc/.../shared_labels.go) and pinned again by check-registry-mirror-render.sh,
# which fails a both-halves rename (measured). These two YAML halves are derived
# copies; the source is Go.
root="$(fixture)"
edit "${root}" "${BASE}/networkpolicy.yaml" '      actions-gateway/component: workload' '      actions-gateway/component: runner'
edit "${root}" "${SHARED}" '                    actions-gateway/component: workload' '                    actions-gateway/component: runner'
run_checker "${root}"
expect 'a rename carried through both halves passes' 0 'both halves of the worker path name actions-gateway/component: runner'

# The same document holds a second podSelector, naming the MIRROR pods the worker
# egress rule may reach. That is the wiring gate'"'"'s business, not this one, and
# reading it as the worker label would fail on a change that is none of this
# gate'"'"'s concern. Demands rc 0 while its neighbours demand 1 -- the assertion,
# not a typo.
root="$(fixture)"
edit "${root}" "${BASE}/networkpolicy.yaml" '          podSelector:
            matchLabels:
              app: registry-mirror' '          podSelector:
            matchLabels:
              app: registry-mirror-v2'
run_checker "${root}"
expect 'the mirror-pod peer selector is not the worker label' 0 'both halves of the worker path name actions-gateway/component: workload'

# --- a partial parse of matchLabels is a refusal, never a verdict -------------
#
# The regexes end at a repeated label line, so they stop at the first one they
# cannot read: an inline comment, or a value with a space in it. WITHOUT the
# trailing lookahead the truncated set is compared as though it were the whole
# selector, two selectors that genuinely differ agree on their first label, and
# the gate prints green naming only the labels it managed to read.
#
# Refusing on an EMPTY extraction does not cover this and reading it as though it
# did is the trap: here the extraction is non-empty and wrong. The asymmetry is
# what hides it -- a comment on the FIRST label line leaves nothing to match and
# refuses already, so probing that shape alone reports the guarantee holding.
#
# Measured 2026-09-10 without the lookaheads: all four shapes below rc 0.

root="$(fixture)"
edit "${root}" "${BASE}/networkpolicy.yaml" '      actions-gateway/component: workload
  policyTypes: [Egress]' '      actions-gateway/component: workload
      tenant: e2e  # per-tenant
  policyTypes: [Egress]'
run_checker "${root}"
expect 'a worker label line the parser cannot read refuses' 2 'expected exactly one e2e-mirror-egress spec.podSelector'

root="$(fixture)"
edit "${root}" "${BASE}/networkpolicy.yaml" '      actions-gateway/component: workload
  policyTypes: [Egress]' '      actions-gateway/component: workload
      tier: gold standard
  policyTypes: [Egress]'
run_checker "${root}"
expect 'a worker label value with a space refuses' 2 'expected exactly one e2e-mirror-egress spec.podSelector'

root="$(fixture)"
edit "${root}" "${SHARED}" '                    actions-gateway/component: workload' '                    actions-gateway/component: workload
                    tenant: e2e  # per-tenant'
run_checker "${root}"
expect 'an ingress peer label the parser cannot read refuses' 2 'expected exactly one ingress peer podSelector'

# The lookahead must not over-refuse: a selector legitimately carrying two
# READABLE labels on both sides is a configuration this gate has no quarrel with.
# Without this case the two above are satisfied by a pattern that refuses any
# multi-label selector at all, which would be a gate nobody could adopt.
root="$(fixture)"
edit "${root}" "${BASE}/networkpolicy.yaml" '      actions-gateway/component: workload
  policyTypes: [Egress]' '      actions-gateway/component: workload
      tenant: e2e
  policyTypes: [Egress]'
edit "${root}" "${SHARED}" '                    actions-gateway/component: workload' '                    actions-gateway/component: workload
                    tenant: e2e'
run_checker "${root}"
expect 'two readable labels agreeing on both sides passes' 0 'both halves of the worker path name actions-gateway/component: workload, tenant: e2e'

# --- the deny container probes itself, not the path it proxies ---------------
#
# The defect this pins: both probes were on /v2/, which is proxied through, so a
# registry fault failed the healthy proxy's own probe. Measured against the
# pinned images with no registry in the netns: the proxy answers /haproxy-up 200
# while /v2/ is 503 and /v2/_catalog is still 403.

root="$(fixture)"
edit "${root}" "${BASE}/deployment.yaml" '            httpGet:
              path: /haproxy-up' '            httpGet:
              path: /v2/'
run_checker "${root}"
expect 'a deny container probing a proxied path fails' 1 'a healthy proxy restarts on a registry fault'

root="$(fixture)"
edit "${root}" "${BASE}/catalog-deny.cfg" '    monitor-uri /haproxy-up
' ''
run_checker "${root}"
expect 'a config answering no monitor path fails' 1 'declares 0 monitor-uri paths'
# The case above passes on the pre-fix script too, so it cannot see the guard
# that stops the per-instance check naming a Python None at an operator when
# there is no path to name. This is the assertion that can.
expect_absent 'and does not render a Python None at an operator' 1 'answers None itself'

# The two files hold one string. Moving either alone must fail, so this mutates
# the config side where the case above mutated the Deployment side.
root="$(fixture)"
edit "${root}" "${BASE}/catalog-deny.cfg" 'monitor-uri /haproxy-up' 'monitor-uri /proxy-up'
run_checker "${root}"
expect 'a monitor path the probes do not name fails' 1 'but catalog-deny.cfg answers /proxy-up itself'

# --- the config the ConfigMap is generated from ------------------------------

root="$(fixture)"
edit "${root}" "${BASE}/catalog-deny.cfg" '    acl catalog path,url_dec -m beg -i /v2/_catalog' '    acl catalog path -m beg -i /v2/_catalog'
run_checker "${root}"
expect 'a deny matching the raw path fails' 1 'reachable as /v2/%5Fcatalog'

# The rule deleted and the paragraph arguing for it left behind — the shape a
# hand edit produces, and the one a whole-file grep grades green.
root="$(fixture)"
edit "${root}" "${BASE}/catalog-deny.cfg" '    acl catalog path,url_dec -m beg -i /v2/_catalog
' ''
run_checker "${root}"
expect 'a config whose rule survives only in its comments fails' 1 'carries no /v2/_catalog rule'

root="$(fixture)"
edit "${root}" "${BASE}/catalog-deny.cfg" 'server local 127.0.0.1:5002' 'server local 127.0.0.1:5003'
run_checker "${root}"
expect 'a backend the registry does not listen on fails' 1 'forwards nowhere the registry listens'

root="$(fixture)"
edit "${root}" "${BASE}/kustomization.yaml" 'haproxy.cfg=catalog-deny.cfg' 'haproxy.cfg=other.cfg'
run_checker "${root}"
expect 'a generator naming another file fails' 1 'no generator turning catalog-deny.cfg'

# --- a read that could not be taken is not a verdict -------------------------

root="$(fixture)"
rm "${root}/${BASE}/catalog-deny.cfg"
run_checker "${root}"
expect 'a missing config refuses rather than failing' 2 'REFUSED: cannot read'

root="$(fixture)"
: >"${root}/${BASE}/deployment.yaml"
run_checker "${root}"
expect 'an empty deployment file refuses' 2 'no mirror Deployments found'

# A selector the parser can no longer find is a read it could not take, never a
# verdict that the two halves agree: an empty extraction graded green is the
# failure this gate would otherwise become.
root="$(fixture)"
edit "${root}" "${BASE}/networkpolicy.yaml" '  podSelector:
    matchLabels:
      actions-gateway/component: workload' '  podSelector:
    matchLabels: {}'
run_checker "${root}"
expect 'a worker podSelector the parser cannot read refuses' 2 'expected exactly one e2e-mirror-egress spec.podSelector'

echo
if ((fails)); then
	echo "${fails} check(s) failed" >&2
	exit 1
fi
echo "all checks passed"
