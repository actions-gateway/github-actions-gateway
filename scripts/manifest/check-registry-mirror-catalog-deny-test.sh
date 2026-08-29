#!/usr/bin/env bash
#
# Tests for scripts/manifest/check-registry-mirror-catalog-deny.py — the gate
# holding every mirror instance behind the deny proxy that refuses
# /v2/_catalog (Q1022).
#
# Why every drift class is asserted rather than just the happy path. Nothing in
# CI renders these manifests (Q1024) and the cluster battery that would catch a
# regression needs a booked dogfood session, so this gate is the only thing
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

expect() {
	local name="$1" want_rc="$2" needle="$3"
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

echo
if ((fails)); then
	echo "${fails} check(s) failed" >&2
	exit 1
fi
echo "all checks passed"
