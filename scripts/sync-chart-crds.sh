#!/usr/bin/env bash
#
# Single source of truth for the Helm chart's CRD templates (Q73 / Q142 slice A).
#
# The chart ships every CRD under charts/actions-gateway/templates/crds/, but the
# authoritative schema is the controller-gen output under cmd/*/config/crd.
# Hand-copying invites silent drift: a field added to a CRD type but not
# propagated to the chart copy is silently pruned at apply time. This script
# regenerates the chart copies FROM the controller-gen sources, injecting the
# per-CRD `helm.sh/resource-policy: keep` annotation the chart needs (so
# `helm upgrade` carries field changes and `helm uninstall` preserves the CRD).
#
# It covers both API groups served side by side: the v1alpha1
# actions-gateway.github.com CRDs (ActionsGateway, RunnerGroup) and the v2alpha1
# actions-gateway.com CRDs (ActionsGateway, EgressProxy, RunnerSet, RunnerTemplate,
# ClusterRunnerTemplate — the v2 decomposition, Q149).
#
#   scripts/sync-chart-crds.sh            # write the chart CRD templates (make chart-crds)
#   scripts/sync-chart-crds.sh --check    # fail if the chart copies are stale, or if
#                                         # the GMC-bundled RunnerGroup CRD has drifted
#                                         # from the AGC authoritative copy (make chart-crds-check)
#
# The --check mode backs the CI drift gate (manifest-validate.yml) and `make check`.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Temp files used by --check; cleaned up on exit. Declared empty so the EXIT
# trap's cleanup is a no-op when no temp file was created (and never trips
# `set -u` on macOS bash 3.2's empty-array expansion).
TMP_FILES=()
cleanup() {
	if [[ ${#TMP_FILES[@]} -gt 0 ]]; then
		rm -f "${TMP_FILES[@]}"
	fi
}
trap cleanup EXIT

# The GMC bundles its own controller-gen copy of the (imported, v1alpha1)
# RunnerGroup type; it must stay byte-identical to the AGC authoritative copy or a
# k8s.io/api skew silently prunes fields on deploy (Q73).
SRC_RUNNERGROUP="cmd/agc/config/crd/actions-gateway.github.com_runnergroups.yaml"
GMC_BUNDLED_RUNNERGROUP="cmd/gmc/config/crd/bases/actions-gateway.github.com_runnergroups.yaml"

# Per-CRD annotation block injected after the controller-gen version line. Kept
# here (not in the chart copies) so the chart files are fully generated. The
# v1alpha1 ActionsGateway and RunnerGroup blocks are preserved verbatim so their
# rendered chart copies stay byte-identical; the v2 CRDs share one generic block.
ACTIONSGATEWAY_BLOCK="$(cat <<'EOF'
    # Shipped under templates/crds/ (not the chart-root crds/ dir) so day-2
    # `helm upgrade` carries CRD field changes — Helm never upgrades crds/.
    # resource-policy: keep also makes `helm uninstall` preserve the CRD (and
    # therefore every tenant's ActionsGateway objects) rather than cascade-delete.
    helm.sh/resource-policy: keep
EOF
)"
RUNNERGROUP_BLOCK="$(cat <<'EOF'
    # Sourced from the AGC authoritative copy (cmd/agc/config/crd/), not the
    # GMC's stale crd/bases/ copy (Q73 drift). Shipped under templates/crds/
    # with resource-policy: keep so `helm upgrade` carries field changes and
    # `helm uninstall` preserves tenant RunnerGroup objects. See docs/operations/upgrade.md.
    helm.sh/resource-policy: keep
EOF
)"
V2_BLOCK="$(cat <<'EOF'
    # Sourced from the controller-gen output under cmd/*/config/crd (do not
    # hand-edit). Shipped under templates/crds/ with resource-policy: keep so
    # day-2 `helm upgrade` carries CRD field changes and `helm uninstall`
    # preserves every tenant's v2 (actions-gateway.com) objects.
    helm.sh/resource-policy: keep
EOF
)"

# Parallel arrays describing every chart CRD template: controller-gen source,
# chart destination, and the annotation block to inject. A single add_crd call
# registers one CRD; sync() and check() both iterate this table.
CRD_SRCS=()
CRD_DSTS=()
CRD_BLOCKS=()

add_crd() { # add_crd SRC DST BLOCK
	CRD_SRCS+=("$1")
	CRD_DSTS+=("$2")
	CRD_BLOCKS+=("$3")
}

# v1alpha1 — actions-gateway.github.com.
add_crd "cmd/gmc/config/crd/bases/actions-gateway.github.com_actionsgateways.yaml" \
	"charts/actions-gateway/templates/crds/actionsgateway-crd.yaml" "$ACTIONSGATEWAY_BLOCK"
add_crd "$SRC_RUNNERGROUP" \
	"charts/actions-gateway/templates/crds/runnergroup-crd.yaml" "$RUNNERGROUP_BLOCK"

# v2alpha1 — actions-gateway.com (Q149). GMC owns ActionsGateway + EgressProxy;
# AGC owns RunnerSet + RunnerTemplate + ClusterRunnerTemplate.
add_crd "cmd/gmc/config/crd/bases/actions-gateway.com_actionsgateways.yaml" \
	"charts/actions-gateway/templates/crds/actionsgateway-v2-crd.yaml" "$V2_BLOCK"
add_crd "cmd/gmc/config/crd/bases/actions-gateway.com_egressproxies.yaml" \
	"charts/actions-gateway/templates/crds/egressproxy-crd.yaml" "$V2_BLOCK"
add_crd "cmd/agc/config/crd/actions-gateway.com_runnersets.yaml" \
	"charts/actions-gateway/templates/crds/runnerset-crd.yaml" "$V2_BLOCK"
add_crd "cmd/agc/config/crd/actions-gateway.com_runnertemplates.yaml" \
	"charts/actions-gateway/templates/crds/runnertemplate-crd.yaml" "$V2_BLOCK"
add_crd "cmd/agc/config/crd/actions-gateway.com_clusterrunnertemplates.yaml" \
	"charts/actions-gateway/templates/crds/clusterrunnertemplate-crd.yaml" "$V2_BLOCK"

# render SRC DST BLOCK — copy SRC to DST, inserting BLOCK right after the
# `controller-gen.kubebuilder.io/version:` annotation line. BLOCK is passed
# through the environment (ENVIRON), not `-v`: a multi-line `-v` assignment is
# rejected by BSD awk (macOS), while ENVIRON is portable across BSD awk and gawk.
render() {
	local src="$1" dst="$2" block="$3"
	BLOCK="$block" awk '
		{ print }
		!injected && /^    controller-gen\.kubebuilder\.io\/version:/ {
			printf "%s\n", ENVIRON["BLOCK"]
			injected = 1
		}
		END {
			if (!injected) {
				print "sync-chart-crds: no controller-gen version line found in source" > "/dev/stderr"
				exit 1
			}
		}
	' "$src" > "$dst"
}

sync() {
	local i
	for i in "${!CRD_SRCS[@]}"; do
		render "${CRD_SRCS[$i]}" "${CRD_DSTS[$i]}" "${CRD_BLOCKS[$i]}"
	done
}

# check renders each chart CRD to a temp file and compares it against the on-disk
# chart copy — it does NOT mutate the working tree, so it detects any drift in the
# committed chart (and any uncommitted hand-edit), not just whether a regen-in-place
# produced a git diff.
check() {
	local rc=0 i tmp
	for i in "${!CRD_SRCS[@]}"; do
		tmp="$(mktemp)"
		TMP_FILES+=("$tmp")
		render "${CRD_SRCS[$i]}" "$tmp" "${CRD_BLOCKS[$i]}"
		if ! diff -u "${CRD_DSTS[$i]}" "$tmp"; then
			echo "ERROR: ${CRD_DSTS[$i]} is out of sync with ${CRD_SRCS[$i]}." >&2
			echo "Re-sync and commit: make chart-crds" >&2
			rc=1
		fi
	done
	if ! diff -u "$GMC_BUNDLED_RUNNERGROUP" "$SRC_RUNNERGROUP"; then
		echo "ERROR: the GMC-bundled RunnerGroup CRD ($GMC_BUNDLED_RUNNERGROUP) has drifted" >&2
		echo "from the AGC authoritative copy ($SRC_RUNNERGROUP) — likely a k8s.io/api skew (Q73)." >&2
		echo "Align the k8s.io/api versions (see Q68) and re-run 'make -C cmd/gmc manifests'." >&2
		rc=1
	fi
	if [[ "$rc" -ne 0 ]]; then
		exit 1
	fi
	echo "chart CRD templates and the GMC-bundled RunnerGroup CRD are in sync."
}

main() {
	case "${1:-}" in
	--check)
		check
		;;
	"")
		sync
		echo "wrote ${#CRD_DSTS[@]} chart CRD templates from controller-gen sources."
		;;
	*)
		echo "usage: $0 [--check]" >&2
		exit 2
		;;
	esac
}

main "$@"
