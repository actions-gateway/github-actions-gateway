#!/usr/bin/env bash
#
# check-registry-mirror-render.sh - Render every shipped registry-mirror
# kustomize target and assert what each render must contain (Q1024).
#
# deploy/registry-mirror/ ships four targets an operator applies with
# `kubectl apply -k`, and until this gate nothing rendered any of them.
# manifest-validate.sh yamllints the tree and kubeconforms the files named in its
# standalone_manifests list -- the base plus overlays/persistent/pvc.yaml. A
# kustomization.yaml that does not render is caught by neither, and first fails
# at apply time on the operator's cluster.
#
# Exit status alone is not enough here, because the shared topology's patch
# REPLACES the mirror-side `ingress` wholesale: NetworkPolicyIngressRule has no
# patch merge key, so the list is atomic. That is what is wanted -- the base's
# single-namespace peer must go -- and it is also the hazard. A second ingress
# rule added to the base does not reach the shared render, and it disappears at
# exit 0 (measured 2026-09-04 by adding one, port 5999, and rendering: present in
# the root render, absent from overlays/shared-tenants). Both failure modes read
# as green, which is what the assertions below are for.
#
# What this asserts:
#
#   1. The declared target list still describes the tree. Every directory with a
#      kustomization.yaml is a declared render target or a declared non-target
#      with its reason. Enumerated rather than counted, so the next overlay
#      cannot fall silently outside the check. This runs without kubectl.
#   2. Every target renders.
#   3. The isolated targets keep the base's single-namespace peer and carry no
#      tenant marker; the shared targets are the exact inverse -- the marker and
#      the workload label present, the single-namespace peer gone.
#   4. Nothing ELSE the base's mirror-side policy declares is lost to the shared
#      render. The dropped set must be exactly EXPECTED_DROPPED; anything more is
#      collateral from the wholesale replacement.
#   5. The five PVCs survive both persistent targets and appear in neither
#      ephemeral one, which is what tells shared-tenants-persistent apart from
#      shared-tenants.
#
# Rules 2-5 need kubectl, which is e2e-tier in scripts/ci/check-tools.sh, so
# `make check` cannot require it: without it they degrade to a printed skip, as
# check-template-library.sh's render half does. --require-render turns that skip
# into a failure, and the CI step passes it -- a hosted runner that lost kubectl
# would otherwise leave this gate green while checking nothing, which is the same
# silent-pass shape the gate exists to close.
#
# Backs `make registry-mirror-render-check`; assertions in
# check-registry-mirror-render-test.sh under `make scripts-test`.
#
# Options (for the test suite; both default to the real tree):
#   --tree PATH        the registry-mirror tree to check
#   --require-render   fail rather than skip when kubectl is absent
set -euo pipefail
shopt -s inherit_errexit

cd "$(git rev-parse --show-toplevel)"

TREE="deploy/registry-mirror"
REQUIRE_RENDER=0

# The shipped render targets, relative to TREE. Enumerated, never counted: a
# count cannot tell a renamed overlay from a new one, and the failure this gate
# exists for is precisely a target nothing renders.
RENDER_TARGETS=(. overlays/persistent overlays/shared-tenants overlays/shared-tenants-persistent)

# Directories carrying a kustomization.yaml that are NOT render targets, each
# with the reason it is covered elsewhere rather than skipped.
#
#   base                      - not applied directly; `.` is the kustomization
#                               that renders it, and is a target above.
#   components/shared-tenants - a Component does not render alone. `kubectl
#                               kustomize` on it exits 1 with "no matches for Id
#                               NetworkPolicy...registry-mirror-worker-access",
#                               because the patch has no base to attach to. It is
#                               covered through the two overlays that compose it.
NON_TARGETS=(base components/shared-tenants)

# The two topologies, as subsets of RENDER_TARGETS.
SHARED_TARGETS=(overlays/shared-tenants overlays/shared-tenants-persistent)
ISOLATED_TARGETS=(. overlays/persistent)
PERSISTENT_TARGETS=(overlays/persistent overlays/shared-tenants-persistent)
EPHEMERAL_TARGETS=(. overlays/shared-tenants)

# One PVC per mirror instance -- overlays/persistent/pvc.yaml. Named rather than
# counted for the reason the targets are.
PVCS=(
	mirror-docker-io-storage
	mirror-ghcr-io-storage
	mirror-quay-io-storage
	mirror-registry-k8s-io-storage
	mirror-gcr-io-storage
)

# The mirror-side policy the shared component patches.
POLICY="registry-mirror-worker-access"

# Indentation is significant in every literal below: these are lines of
# kustomize-normalized output, and the indent is what separates the AND form of a
# peer from the OR form. `podSelector:` at six spaces is a second KEY of the same
# `from` element (a managed tenant namespace AND a workload pod); at four, behind
# a dash, it would be a second ELEMENT, which ORs and is strictly wider (Q1026).
ISOLATED_PEER='          kubernetes.io/metadata.name: gag-dogfood-e2e'
TENANT_MARKER='          actions-gateway.com/tenant: managed'
WORKLOAD_LABEL='          actions-gateway/component: workload'
AND_PODSELECTOR='      podSelector:'

# The ONLY line the shared render may lose relative to the base's. The base's
# single-namespace peer is what the shared topology exists to replace; anything
# else in this set is collateral from the wholesale list replacement.
EXPECTED_DROPPED="$ISOLATED_PEER"

while (($# > 0)); do
	case "$1" in
	--tree)
		TREE="$2"
		shift
		;;
	--require-render)
		REQUIRE_RENDER=1
		;;
	*)
		printf 'check-registry-mirror-render.sh: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done

fails=0
fail() {
	printf 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

# contains ITEM ELEMENT... - literal equality against the remaining args.
# Deliberately not a `grep -qx` over a printed list: `grep -q` exits on the
# match, the writer takes SIGPIPE, and under `set -o pipefail` the dead pipeline
# reads as no-match (Q982). Literal, not anchored-regex, because a `.` in a
# target name was never meant to match any character -- and `.` IS a target here.
contains() {
	local item="$1" element
	shift
	for element in "$@"; do
		[[ "$element" == "$item" ]] && return 0
	done
	return 1
}

[[ -d "$TREE" ]] || {
	printf 'check-registry-mirror-render.sh: no such tree: %s\n' "$TREE" >&2
	exit 2
}

# ------------------------------------------------------- rule 1: the list ----
#
# Reconcile the declared targets with the tree. Both directions: a directory the
# lists do not name is an unchecked target, and a named target that is gone
# leaves the gate asserting over a tree that no longer exists.

found=()
while IFS= read -r kust; do
	dir="$(dirname "$kust")"
	dir="${dir#"$TREE"}"
	dir="${dir#/}"
	found+=("${dir:-.}")
done < <(find "$TREE" -name kustomization.yaml -type f | LC_ALL=C sort)

if ((${#found[@]} == 0)); then
	fail "$TREE contains no kustomization.yaml — this gate is no longer checking anything"
fi

for dir in "${found[@]}"; do
	if ! contains "$dir" "${RENDER_TARGETS[@]}" && ! contains "$dir" "${NON_TARGETS[@]}"; then
		fail "$TREE/$dir carries a kustomization.yaml but is neither a declared render target nor a declared non-target.
       An operator can run \`kubectl apply -k $TREE/$dir\`, so add it to RENDER_TARGETS
       in this script — or to NON_TARGETS with the reason it cannot render alone."
	fi
done

for target in "${RENDER_TARGETS[@]}" "${NON_TARGETS[@]}"; do
	contains "$target" "${found[@]}" ||
		fail "this script declares '$target', which has no kustomization.yaml under $TREE; the declaration and the tree have separated"
done

# --------------------------------------------------------------- renders ----

if ! command -v kubectl >/dev/null 2>&1; then
	if ((REQUIRE_RENDER)); then
		fail "--require-render was passed and kubectl is not on PATH, so nothing below ran.
       This gate's render assertions are its whole point; a silent skip here is the
       same shape of green-while-checking-nothing that it exists to catch."
	else
		printf 'note: kubectl not on PATH (it is e2e-tier in scripts/ci/check-tools.sh), so the render assertions were skipped\n'
	fi
	((fails == 0)) || exit 1
	printf 'registry-mirror target list agrees with the tree: %d render targets, %d non-targets (renders not checked)\n' \
		"${#RENDER_TARGETS[@]}" "${#NON_TARGETS[@]}"
	exit 0
fi

# render TARGET - the target's rendered manifests, or non-zero with the error.
render() { kubectl kustomize "$TREE/$1" 2>&1; }

# policy_lines - the POLICY document out of a render on stdin, trailing
# whitespace stripped and sorted unique. Kustomize normalizes its output, so the
# two sides of a comparison cannot disagree on formatting the sources chose.
# Documents split on a bare `---`, which is how kustomize separates them.
policy_lines() {
	awk -v want="  name: $POLICY" '
		/^---$/ { if (index(buf, want "\n")) printf "%s", buf; buf = ""; next }
		{ buf = buf $0 "\n" }
		END { if (index(buf, want "\n")) printf "%s", buf }
	' | sed 's/[[:space:]]*$//' | LC_ALL=C sort -u
}

declare -A RENDERS=()
for target in "${RENDER_TARGETS[@]}"; do
	if out="$(render "$target")"; then
		RENDERS["$target"]="$out"
	else
		fail "\`kubectl kustomize $TREE/$target\` does not render, so an operator cannot apply it:
$(awk '{ print "         " $0 }' <<<"$out")"
	fi
done

# base_policy - the isolated topology's own mirror-side policy, which the shared
# renders are compared against. `.` is the target that renders the base.
base_policy=""
if [[ -n "${RENDERS[.]:-}" ]]; then
	base_policy="$(policy_lines <<<"${RENDERS[.]}")"
	[[ -n "$base_policy" ]] ||
		fail "the root render declares no $POLICY; every assertion about the shared topology is relative to it"
fi

# ------------------------------------------- rules 3 and 4: the two topologies ----

for target in "${ISOLATED_TARGETS[@]}"; do
	[[ -n "${RENDERS[$target]:-}" ]] || continue
	lines="$(policy_lines <<<"${RENDERS[$target]}")"

	grep -qxF -- "$ISOLATED_PEER" <<<"$lines" ||
		fail "$TREE/$target renders $POLICY WITHOUT the base's single-namespace peer,
       $ISOLATED_PEER
       This is the isolated topology: one mirror set serving one named tenant."
	if grep -qxF -- "$TENANT_MARKER" <<<"$lines"; then
		fail "$TREE/$target renders $POLICY WITH the shared topology's tenant marker.
       The isolated targets must not admit every managed tenant namespace; that is
       what the shared-tenants component is for, and it is a widening, not a default."
	fi
done

for target in "${SHARED_TARGETS[@]}"; do
	[[ -n "${RENDERS[$target]:-}" ]] || continue
	lines="$(policy_lines <<<"${RENDERS[$target]}")"

	grep -qxF -- "$TENANT_MARKER" <<<"$lines" ||
		fail "$TREE/$target renders $POLICY WITHOUT the managed-tenant marker; the shared topology's whole effect is missing from its own render"
	grep -qxF -- "$WORKLOAD_LABEL" <<<"$lines" ||
		fail "$TREE/$target renders $POLICY WITHOUT the workload podSelector. Without it the
       policy admits any pod in EVERY managed tenant namespace, and the GMC's
       default-deny selects only workload-labelled pods — so an unlabelled pod
       would be governed by no egress policy at all (Q1026)."
	grep -qxF -- "$AND_PODSELECTOR" <<<"$lines" ||
		fail "$TREE/$target renders the podSelector at the wrong depth: it must be a second KEY
       of the same \`from\` element, which ANDs it with the namespaceSelector. As a
       second list ELEMENT it ORs instead, and the result is strictly wider (Q1026)."
	if grep -qxF -- "$ISOLATED_PEER" <<<"$lines"; then
		fail "$TREE/$target renders $POLICY still carrying the base's single-namespace peer.
       The shared patch must REPLACE that peer, not be joined to it."
	fi

	# Rule 4. The direct catch for the wholesale replacement: anything the base
	# declares and this render lost, beyond the peer the patch exists to remove.
	if [[ -n "$base_policy" ]]; then
		collateral="$(comm -23 <(printf '%s\n' "$base_policy") <(printf '%s\n' "$lines") | grep -vxF -- "$EXPECTED_DROPPED" || true)"
		if [[ -n "$collateral" ]]; then
			fail "$TREE/$target renders $POLICY WITHOUT line(s) the base declares:
$(awk '{ print "         " $0 }' <<<"$collateral")
       Strategic merge replaces \`ingress\` wholesale — NetworkPolicyIngressRule has no
       patch merge key — so anything the base's ingress declares beyond the peer being
       replaced is dropped here silently, at exit 0. Restate it in
       $TREE/components/shared-tenants/kustomization.yaml, or narrow this patch."
		fi
	fi
done

# ----------------------------------------------------- rule 5: the storage ----

for target in "${PERSISTENT_TARGETS[@]}"; do
	[[ -n "${RENDERS[$target]:-}" ]] || continue
	for pvc in "${PVCS[@]}"; do
		grep -qxF -- "  name: $pvc" <<<"${RENDERS[$target]}" ||
			fail "$TREE/$target renders no PVC '$pvc'. The persistent targets exist to swap each
       instance's emptyDir for a PVC-backed disk; a missing claim leaves that
       instance on an ephemeral cache with nothing to say so."
	done
done

for target in "${EPHEMERAL_TARGETS[@]}"; do
	[[ -n "${RENDERS[$target]:-}" ]] || continue
	for pvc in "${PVCS[@]}"; do
		if grep -qxF -- "  name: $pvc" <<<"${RENDERS[$target]}"; then
			fail "$TREE/$target renders PVC '$pvc', but it is an ephemeral target — \$0 at rest is
       its whole point, and a PVC here bills continuously. This is the assertion that
       tells shared-tenants-persistent apart from shared-tenants."
		fi
	done
done

# ---------------------------------------------------------------- verdict ----

if ((fails > 0)); then
	printf '\n%d registry-mirror render check(s) failed. These are the targets an operator\n' "$fails" >&2
	printf 'applies with kubectl apply -k; a target that does not render, or a shared\n' >&2
	printf 'render that silently lost a rule the base declares, first fails on their\n' >&2
	printf 'cluster. See %s/README.md.\n' "$TREE" >&2
	exit 1
fi

printf 'registry-mirror renders: %d targets (%d shared, %d persistent), %d PVCs where persistence is on, base ingress intact under the shared patch\n' \
	"${#RENDER_TARGETS[@]}" "${#SHARED_TARGETS[@]}" "${#PERSISTENT_TARGETS[@]}" "${#PVCS[@]}"
