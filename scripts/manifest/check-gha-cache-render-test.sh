#!/usr/bin/env bash
#
# Tests for scripts/manifest/check-gha-cache-render.sh — the gate that renders
# the deploy/gha-cache/ tenant example with and without its persistence
# component (Q215).
#
# Every failure mode is asserted, not only the happy path: the shipped tree
# renders, so "it passes" is the state of the tree rather than evidence the gate
# works. Three of the failures it exists for exit 0 from kubectl — a replacement
# writing the namespace into the wrong label, a component that keeps the
# emptyDir beside its PVC, and a budget that outgrows its disk — so no case rests
# on exit status alone.
#
# Each case mutates a copy of the real tree under the gitignored tmp/, because
# the checker resolves its tree against the repo root.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "${REPO_ROOT}"

CHECKER=scripts/manifest/check-gha-cache-render.sh
SRC=deploy/gha-cache
WORKDIR=tmp/gha-cache-render-test

rm -rf "${WORKDIR}"
mkdir -p "${WORKDIR}"
trap 'rm -rf "${WORKDIR}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fails=0
out=""
rc=0

# fixture — a fresh copy of the shipped tree, printed as a repo-relative path.
fixture() {
	local root
	root="$(mktemp -d "${WORKDIR}/case.XXXXXX")"
	cp -R "${SRC}" "${root}/gha-cache"
	printf '%s' "${root}/gha-cache"
}

run_checker() {
	rc=0
	out="$(./"${CHECKER}" --tree "$@" 2>&1)" || rc=$?
}

# edit FILE OLD NEW — a literal substitution, refusing when it matched nothing,
# so a mutation that did not land cannot read as a clean tree.
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

echo "scripts/manifest/check-gha-cache-render-test.sh"

have_kubectl=0
command -v kubectl >/dev/null 2>&1 && have_kubectl=1

# render_case NAME NEEDLE — assert the render-rule verdict when kubectl is
# present, and the skip's contract when it is not, so the case still fires on a
# lane without kubectl.
render_case() {
	if ((have_kubectl)); then
		expect "$1" 1 "$2"
	else
		expect "$1" 0 'renders not checked'
	fi
}

# --- the tree as shipped -----------------------------------------------------

run_checker "$(fixture)"
if ((have_kubectl)); then
	expect 'the shipped tree renders' 0 'gha-cache renders: default and persistent'
else
	expect 'the shipped tree renders' 0 'renders not checked'
fi

# --- rule 1: the declared list against the tree ------------------------------

root="$(fixture)"
mkdir -p "${root}/overlays/regional"
cp "${root}/example/kustomization.yaml" "${root}/overlays/regional/kustomization.yaml"
run_checker "${root}"
expect 'a kustomization nothing declares is caught' 1 'neither the render target nor a declared non-target'

# --- rule 2: a render that does not build ------------------------------------

root="$(fixture)"
edit "${root}/example/kustomization.yaml" '  - ../base' '  - ../nonexistent'
run_checker "${root}"
render_case 'an example that does not render is caught' 'does not build'

root="$(fixture)"
edit "${root}/example/kustomization.yaml" '# components:
#   - ../components/persistent' ''
run_checker "${root}"
render_case 'an example that lost its commented components entry is caught' 'no longer carries the commented lines'

# --- rule 3: THE silent one --------------------------------------------------
#
# Measured 2026-11-01: `index: 2` exits 0 and renders the namespace into the
# wrong label of the address, leaving the NAMESPACE placeholder in it.

root="$(fixture)"
edit "${root}/example/kustomization.yaml" 'index: 1' 'index: 2'
run_checker "${root}"
render_case 'a replacement writing the wrong label is caught' 'NAMESPACE.tenant-a.cluster.local'

root="$(fixture)"
python3 - "${root}/example/kustomization.yaml" <<'PY'
import sys
path = sys.argv[1]
s = open(path).read()
if "\nreplacements:\n" not in s:
    sys.exit(f"mutation did not match in {path}: replacements block")
open(path, "w").write(s[: s.index("\nreplacements:\n") + 1])
PY
run_checker "${root}"
render_case 'an example without its replacement is caught' 'http://gha-cache.NAMESPACE.svc.cluster.local:3000'

# --- rule 4: the volume ------------------------------------------------------

root="$(fixture)"
edit "${root}/components/persistent/kustomization.yaml" '                emptyDir: null
' ''
run_checker "${root}"
render_case 'a persistent render keeping the emptyDir is caught' 'must mount gha-cache-data and carry no emptyDir (emptyDir=1, claim=1)'

# --- rule 5: the NetworkPolicies ---------------------------------------------

root="$(fixture)"
edit "${root}/base/kustomization.yaml" '  - networkpolicy.yaml
' ''
run_checker "${root}"
render_case 'a render without its NetworkPolicies is caught' 'has no NetworkPolicy workload-to-gha-cache'

# --- rule 6: the budget against the disk -------------------------------------

root="$(fixture)"
edit "${root}/base/deployment.yaml" '"10737418240"' '"17179869184"'
run_checker "${root}"
render_case 'a budget at the emptyDir limit is caught' 'is not below its emptyDir sizeLimit (16Gi)'
if ((have_kubectl)); then
	expect 'and at the PVC request' 1 'is not below its PVC request (16Gi)'
fi

# --- the kubectl guard -------------------------------------------------------
#
# kubectl is hidden by replacing each PATH directory that provides one with a
# directory of symlinks to everything else in it, so the bash 5 that often sits
# beside kubectl stays reachable. With kubectl genuinely absent the lane under
# test is the one already running.

nopath=""
if ((have_kubectl)); then
	shim_n=0
	while IFS= read -r d; do
		[[ -n "$d" ]] || continue
		if [[ -x "$d/kubectl" ]]; then
			shim_n=$((shim_n + 1))
			shim="${WORKDIR}/shim-${shim_n}"
			mkdir -p "${shim}"
			for f in "$d"/*; do
				b="${f##*/}"
				[[ "$b" == kubectl ]] && continue
				[[ -e "${shim}/${b}" ]] || ln -s "$f" "${shim}/${b}"
			done
			d="${shim}"
		fi
		nopath="${nopath:+${nopath}:}${d}"
	done < <(tr ':' '\n' <<<"${PATH}")
else
	nopath="${PATH}"
fi

if ((have_kubectl)) && PATH="${nopath}" command -v kubectl >/dev/null 2>&1; then
	echo "skip kubectl-absent cases: kubectl is still reachable with every providing directory shimmed"
elif ! PATH="${nopath}" bash -c 'shopt -s inherit_errexit' 2>/dev/null; then
	echo "skip kubectl-absent cases: the shimmed PATH provides no bash supporting inherit_errexit"
else
	root="$(fixture)"
	rc=0
	out="$(PATH="${nopath}" ./"${CHECKER}" --tree "${root}" 2>&1)" || rc=$?
	expect 'without kubectl the render rules skip and say so' 0 'kubectl not on PATH'

	root="$(fixture)"
	mkdir -p "${root}/overlays/regional"
	cp "${root}/example/kustomization.yaml" "${root}/overlays/regional/kustomization.yaml"
	rc=0
	out="$(PATH="${nopath}" ./"${CHECKER}" --tree "${root}" 2>&1)" || rc=$?
	expect 'and an undeclared kustomization still fails without kubectl' 1 'neither the render target'

	root="$(fixture)"
	rc=0
	out="$(PATH="${nopath}" ./"${CHECKER}" --tree "${root}" --require-render 2>&1)" || rc=$?
	expect '--require-render fails rather than skipping' 1 'nothing below ran'
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
echo "all gha-cache render checks pass"
