#!/usr/bin/env bash
#
# Tests for scripts/manifest/check-registry-mirror-wiring.py — the gate holding
# the mirror instances, their Services and the tenant's wiring ConfigMap to one
# endpoint set (Q408 Phase 3).
#
# Why every drift class is asserted rather than just the happy path. Under the
# Phase 3 posture the Kata overlay's allow-all `e2e-open-egress` is still in
# place, so a mirror the job never reaches changes nothing an e2e run can see:
# the suite is green, the pulls simply go direct. This gate is the only thing
# between that drift and a booked dogfood session, so a gate that stopped firing
# would be indistinguishable from a tree that is in step — which is exactly the
# state the real tree is in, and why "it passes" proves nothing on its own.
#
# Each case mutates a copy of the real files rather than a hand-written fixture:
# a fixture asserts the shape its author had in mind, and the shape that matters
# is the one the repo actually ships.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
CHECKER="${REPO_ROOT}/scripts/manifest/check-registry-mirror-wiring.py"

DEPLOYMENTS=deploy/registry-mirror/base/deployment.yaml
SERVICES=deploy/registry-mirror/base/service.yaml
WIRING=deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0
out=""
rc=0

# fixture — a fresh copy of the three files under a throwaway root, printed.
fixture() {
	local root="${WORKDIR}/case.$$.${RANDOM}"
	mkdir -p "${root}/$(dirname "${DEPLOYMENTS}")" "${root}/$(dirname "${WIRING}")"
	cp "${REPO_ROOT}/${DEPLOYMENTS}" "${root}/${DEPLOYMENTS}"
	cp "${REPO_ROOT}/${SERVICES}" "${root}/${SERVICES}"
	cp "${REPO_ROOT}/${WIRING}" "${root}/${WIRING}"
	printf '%s' "${root}"
}

# run_checker ROOT — run the gate against a fixture root, capturing rc and output.
run_checker() {
	rc=0
	out="$( (cd "$1" && python3 "${CHECKER}") 2>&1 )" || rc=$?
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

# --- the tree as shipped -----------------------------------------------------

root="$(fixture)"
run_checker "${root}"
expect 'the shipped tree is consistent' 0 'consistent over 5 upstreams'

# --- an upstream gained on one side only -------------------------------------
#
# The drift the gate exists for: a sixth mirror instance whose pulls nothing
# points at, and its mirror image, a map entry with no instance behind it.

root="$(fixture)"
python3 - "${root}/${WIRING}" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
# Drop gcr.io from the ref-rewrite map alone: an instance exists, and the half
# of the wiring that redirects its non-Hub pulls forgot it.
s = re.sub(r"\n *gcr\.io=\S+", "", s)
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'an instance missing from the map fails' 1 'gcr.io: served by a mirror instance but absent'

root="$(fixture)"
python3 - "${root}/${WIRING}" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
s = s.replace(
    "    gcr.io=mirror-gcr-io.gag-registry-mirror.svc.cluster.local:5000",
    "    gcr.io=mirror-gcr-io.gag-registry-mirror.svc.cluster.local:5000\n"
    "    example.com=mirror-example-com.gag-registry-mirror.svc.cluster.local:5000",
)
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'a map entry with no instance fails' 1 'example.com: named in the ConfigMap'

# --- an endpoint that points at the wrong place ------------------------------

root="$(fixture)"
python3 - "${root}/${WIRING}" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
s = s.replace("quay.io=mirror-quay-io.gag-registry-mirror",
              "quay.io=mirror-quay-io.gag-dogfood-e2e")
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'a wrong namespace in an endpoint fails' 1 'quay.io: the map points at'

# --- the instance/upstream pairing itself ------------------------------------
#
# Read from REGISTRY_PROXY_REMOTEURL rather than from the name, so an instance
# proxying the wrong upstream is caught here instead of at pull time.

root="$(fixture)"
python3 - "${root}/${DEPLOYMENTS}" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
s = s.replace("value: https://quay.io", "value: https://gcr.io", 1)
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'an instance proxying the wrong upstream fails' 1 'must be named mirror-gcr-io'

# --- the daemon.json half ----------------------------------------------------

root="$(fixture)"
python3 - "${root}/${WIRING}" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
s = re.sub(r'\n *"mirror-registry-k8s-io\.[^"]+",', "", s)
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'an endpoint missing from insecure-registries fails' 1 'insecure-registries must list every mirror endpoint'

root="$(fixture)"
python3 - "${root}/${WIRING}" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
s = s.replace('"http://mirror-docker-io.gag-registry-mirror.svc.cluster.local:5000"',
              '"http://mirror-ghcr-io.gag-registry-mirror.svc.cluster.local:5000"')
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'daemon.json mirroring the wrong registry fails' 1 'must name the docker.io instance and only it'

# --- a Service and its Deployment ---------------------------------------------

root="$(fixture)"
python3 - "${root}/${SERVICES}" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
s = s.replace("  name: mirror-gcr-io\n", "  name: mirror-gcr-iox\n", 1)
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'an instance with no Service fails' 1 'mirror-gcr-io: a Deployment with no Service'

# --- refusals ----------------------------------------------------------------
#
# An empty extraction is a parser that stopped matching, and every check above
# would then pass over nothing. Those cases must refuse (rc 2), never report a
# consistent tree.

root="$(fixture)"
: > "${root}/${DEPLOYMENTS}"
run_checker "${root}"
expect 'an empty deployment file refuses' 2 'no mirror Deployments found'

root="$(fixture)"
python3 - "${root}/${WIRING}" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
s = re.sub(r"\n  registry-mirrors: >-\n(    .*\n)+", "\n", s)
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'a missing map key refuses' 2 'no block scalar named registry-mirrors'

root="$(fixture)"
python3 - "${root}/${WIRING}" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
s = s.replace('"registry-mirrors": [', '"registry-mirrors" [')
open(p, "w").write(s)
PY
run_checker "${root}"
expect 'invalid daemon.json refuses' 2 'not valid JSON'

root="$(fixture)"
rm "${root}/${SERVICES}"
run_checker "${root}"
expect 'an unreadable file refuses' 2 'cannot read'

if ((fails)); then
	echo "${fails} assertion(s) failed" >&2
	exit 1
fi
echo
echo "all check-registry-mirror-wiring assertions passed"
