#!/usr/bin/env bash
#
# Tests for scripts/manifest/check-registry-mirror-render.sh — the gate that
# renders every shipped registry-mirror kustomize target and asserts what each
# render must contain (Q1024).
#
# Why every failure mode is asserted rather than just the happy path. The gate's
# subject is a set of renders that all currently succeed, so "it passes" is the
# state of the tree, not evidence the gate works: an assertion that stopped
# firing would be indistinguishable from a whole tree. Two of the failures it
# exists for return exit 0 from kubectl — a rule silently dropped by the shared
# topology's wholesale ingress replacement, and a peer widened from AND to OR —
# so exit status cannot stand in for any case below.
#
# Each case mutates a copy of the real tree rather than a hand-written fixture: a
# fixture asserts the shape its author had in mind, and the shape that matters is
# the one the repo actually ships.
#
# Fixtures live under the repo's gitignored tmp/ rather than host temp, because
# the checker resolves its tree against `git rev-parse --show-toplevel`.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPO_ROOT}"

CHECKER=scripts/manifest/check-registry-mirror-render.sh
SRC=deploy/registry-mirror
WORKDIR=tmp/registry-mirror-render-test

rm -rf "${WORKDIR}"
mkdir -p "${WORKDIR}"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0
out=""
rc=0

# fixture — a fresh copy of the shipped tree, printed as a repo-relative path.
#
# The directory is removed before it is filled, and named uniquely rather than
# from a counter: fixture runs in a command substitution, so a counter increment
# here never reaches the caller. Every case would then reuse one directory, `cp`
# would nest the tree inside the copy already there, and each case would run
# against the accumulated mutations of the ones before it.
fixture() {
	local root
	root="${WORKDIR}/case.$$.${RANDOM}.${SECONDS}"
	rm -rf "${root}"
	mkdir -p "${root}"
	cp -R "${SRC}" "${root}/registry-mirror"
	printf '%s' "${root}/registry-mirror"
}

run_checker() {
	rc=0
	out="$(./"${CHECKER}" --tree "$@" 2>&1)" || rc=$?
}

# edit FILE OLD NEW — a literal substitution, refusing when it matched nothing.
# A mutation that silently did not land leaves the gate reading clean input, and
# that green is indistinguishable from a gate that caught nothing.
edit() {
	python3 - "$1" "$2" "$3" <<'PY'
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

# expect_absent — for an assertion whose whole effect is that a message does NOT
# appear. `expect` can only require a substring present, so a case meant to prove
# the gate stays quiet has nothing it can fail on otherwise.
expect_absent() {
	local name="$1" want_rc="$2" needle="$3"
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

echo "scripts/manifest/check-registry-mirror-render-test.sh"

have_kubectl=0
command -v kubectl >/dev/null 2>&1 && have_kubectl=1

# --- the tree as shipped -----------------------------------------------------

run_checker "$(fixture)"
if ((have_kubectl)); then
	expect 'the shipped tree renders' 0 '4 targets (2 shared, 2 persistent), 5 PVCs'
else
	expect 'the shipped tree renders' 0 'renders not checked'
fi

# --- rule 1: the declared list against the tree ------------------------------
#
# The trap the row names: a fifth target added later falling silently outside the
# check. This is the case that makes the list enumerated rather than counted —
# and it must fire with or without kubectl, since it reads no render.

root="$(fixture)"
mkdir -p "${root}/overlays/regional"
cp "${root}/overlays/shared-tenants/kustomization.yaml" "${root}/overlays/regional/kustomization.yaml"
run_checker "${root}"
expect 'a new overlay nothing declares is caught' 1 'neither a declared render target nor a declared non-target'

root="$(fixture)"
rm -rf "${root}/overlays/persistent"
run_checker "${root}"
expect 'a declared target that left the tree is caught' 1 "declares 'overlays/persistent', which has no kustomization.yaml"

# --- rule 2: a target that does not render -----------------------------------

root="$(fixture)"
edit "${root}/overlays/shared-tenants/kustomization.yaml" '  - ../../base' '  - ../../nonexistent'
run_checker "${root}"
expect 'a target that does not render is caught' 1 'does not render, so an operator cannot apply it'

# --- rule 4: THE silent one --------------------------------------------------
#
# A second ingress rule added to the base disappears from both shared renders at
# exit 0, because the component's strategic merge replaces `ingress` wholesale
# (NetworkPolicyIngressRule has no patch merge key). Measured 2026-09-04: port
# 5999 renders under the root and is absent from overlays/shared-tenants. This is
# the case the gate exists for and the one nothing else in CI can see.

root="$(fixture)"
cat >>"${root}/base/networkpolicy.yaml" <<'YAML'
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: gag-probe-canary
      ports:
        - protocol: TCP
          port: 5999
YAML
run_checker "${root}"
if ((have_kubectl)); then
	expect 'a base ingress rule the shared render drops is caught' 1 'WITHOUT line(s) the base declares'
	expect 'and the dropped line itself is named' 1 'gag-probe-canary'
else
	expect 'a base ingress rule the shared render drops is caught' 0 'renders not checked'
fi

# --- rule 3: the two topologies keep their own posture -----------------------

root="$(fixture)"
edit "${root}/components/shared-tenants/kustomization.yaml" '                podSelector:
                  matchLabels:
                    actions-gateway/component: workload
' ''
run_checker "${root}"
if ((have_kubectl)); then
	expect 'a shared render with no workload podSelector is caught' 1 'WITHOUT the workload podSelector'
fi

# The AND/OR widening (Q1026). Both forms render at exit 0 and differ only in
# depth: as a second KEY the peer is a managed namespace AND a workload pod; as a
# second list ELEMENT it is either, which is strictly wider. The line set alone
# cannot tell them apart, which is why the assertion pins the indent.
root="$(fixture)"
edit "${root}/components/shared-tenants/kustomization.yaml" '                podSelector:' '              - podSelector:'
run_checker "${root}"
if ((have_kubectl)); then
	expect 'a peer widened from AND to OR is caught' 1 'renders the podSelector at the wrong depth'
fi

# A patch that ADDS its peer instead of replacing the base's leaves the isolated
# single-namespace peer in a shared render — the topology half-applied.
root="$(fixture)"
edit "${root}/components/shared-tenants/kustomization.yaml" '          - from:
              - namespaceSelector:' '          - from:
              - namespaceSelector:
                  matchLabels:
                    kubernetes.io/metadata.name: gag-dogfood-e2e
              - namespaceSelector:'
run_checker "${root}"
if ((have_kubectl)); then
	expect 'a shared render still carrying the base peer is caught' 1 'still carrying the base'\''s single-namespace peer'
fi

# The other direction: the isolated targets must not quietly acquire the shared
# marker. Applying the component to the ROOT kustomization is how that happens.
root="$(fixture)"
edit "${root}/kustomization.yaml" 'resources:
  - base' 'resources:
  - base
components:
  - components/shared-tenants'
run_checker "${root}"
if ((have_kubectl)); then
	expect 'an isolated target given the tenant marker is caught' 1 'WITH the shared topology'\''s tenant marker'
fi

# --- rule 5: the storage tells the two persistent targets apart --------------

root="$(fixture)"
edit "${root}/overlays/persistent/pvc.yaml" '---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: mirror-gcr-io-storage' '---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: mirror-gcr-io-storage-renamed'
run_checker "${root}"
if ((have_kubectl)); then
	expect 'a PVC missing from a persistent target is caught' 1 "renders no PVC 'mirror-gcr-io-storage'"
fi

# The inverse, and the assertion that gives shared-tenants its own identity: a
# PVC reaching an ephemeral target bills continuously where $0 at rest was the
# point, and nothing else in this gate would notice.
# Added to the ROOT kustomization rather than the base, so only the ephemeral
# target acquires it: a PVC in the base would reach overlays/persistent too,
# where it collides with pvc.yaml's own copy and fails the render instead —
# a different assertion firing, and this one never exercised.
root="$(fixture)"
cat >"${root}/stray-pvc.yaml" <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: mirror-gcr-io-storage
  namespace: gag-registry-mirror
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
  storageClassName: standard-rwo
YAML
edit "${root}/kustomization.yaml" 'resources:
  - base' 'resources:
  - base
  - stray-pvc.yaml'
run_checker "${root}"
if ((have_kubectl)); then
	expect 'a PVC reaching an ephemeral target is caught' 1 'but it is an ephemeral target'
fi

# --- the kubectl guard -------------------------------------------------------
#
# Without kubectl the render rules degrade to a printed skip, and --require-render
# turns that skip into a failure. The CI step passes it, so a hosted runner that
# lost kubectl fails loudly instead of leaving this gate green while checking
# nothing — the same silent-pass shape the gate exists to close.
#
# kubectl is hidden by replacing each PATH directory that provides one with a
# directory of symlinks to everything in it EXCEPT kubectl.
#
# Dropping those directories outright is the obvious move and it is wrong: on
# this machine kubectl sits in /opt/homebrew/bin, which also provides bash 5, so
# a stripped PATH sent `#!/usr/bin/env bash` to macOS's bash 3.2 and the checker
# died on `shopt -s inherit_errexit` before reaching anything these cases assert.
# All four still "failed as expected" on rc, which is what makes the shortcut
# worth naming: the cases were green-adjacent while measuring nothing.
#
# More than one directory can provide it (this machine has /opt/homebrew/bin and
# /usr/local/bin), so every one is shimmed, and the result is verified below
# before anything is asserted over it.
if ((have_kubectl)); then
	nopath=""
	shim_n=0
	while IFS= read -r d; do
		[[ -n "$d" ]] || continue
		if [[ -x "$d/kubectl" ]]; then
			shim_n=$((shim_n + 1))
			shim="${WORKDIR}/shim-${shim_n}"
			mkdir -p "${shim}"
			for f in "$d"/*; do
				b="$(basename "$f")"
				[[ "$b" == kubectl ]] && continue
				[[ -e "${shim}/${b}" ]] || ln -s "$f" "${shim}/${b}"
			done
			d="${shim}"
		fi
		nopath="${nopath:+${nopath}:}${d}"
	done < <(tr ':' '\n' <<<"${PATH}")
	if PATH="${nopath}" command -v kubectl >/dev/null 2>&1; then
		echo "skip kubectl-absent cases: kubectl is still reachable with every providing directory shimmed"
	elif ! PATH="${nopath}" bash -c 'shopt -s inherit_errexit' 2>/dev/null; then
		# The shim has to leave a bash 4+ reachable, or every case below reports a
		# shell error as though it were the verdict it was asserting.
		echo "skip kubectl-absent cases: the shimmed PATH provides no bash supporting inherit_errexit"
	else
		root="$(fixture)"
		rc=0
		out="$(PATH="${nopath}" ./"${CHECKER}" --tree "${root}" 2>&1)" || rc=$?
		expect 'without kubectl the render rules skip and say so' 0 'kubectl not on PATH'
		expect 'and the target list is still reconciled' 0 'renders not checked'

		# The degradation must not be total: rule 1 reads no render, so it has to
		# keep firing. Without this case a skip that silently checked NOTHING would
		# pass the two assertions above.
		root="$(fixture)"
		mkdir -p "${root}/overlays/regional"
		cp "${root}/overlays/shared-tenants/kustomization.yaml" "${root}/overlays/regional/kustomization.yaml"
		rc=0
		out="$(PATH="${nopath}" ./"${CHECKER}" --tree "${root}" 2>&1)" || rc=$?
		expect 'and an undeclared target still fails without kubectl' 1 'neither a declared render target'

		root="$(fixture)"
		rc=0
		out="$(PATH="${nopath}" ./"${CHECKER}" --tree "${root}" --require-render 2>&1)" || rc=$?
		expect '--require-render fails rather than skipping' 1 'nothing below ran'
		expect_absent 'and does not also print the skip note' 1 'the render assertions were skipped'
	fi
fi

# --- argument handling -------------------------------------------------------

rc=0
out="$(./"${CHECKER}" --nonsense 2>&1)" || rc=$?
expect 'an unknown argument is rejected' 2 'unknown argument'

# --- verdict -----------------------------------------------------------------

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all registry-mirror render checks pass"
