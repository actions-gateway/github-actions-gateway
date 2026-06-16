#!/usr/bin/env bash
#
# Single source of truth for the Helm chart's CRD templates (Q73 / Q142 slice A).
#
# The chart ships the two CRDs under charts/actions-gateway/templates/crds/, but
# the authoritative schema is the controller-gen output under cmd/*/config/crd.
# Hand-copying invites silent drift: a field added to a CRD type but not
# propagated to the chart copy is silently pruned at apply time. This script
# regenerates the chart copies FROM the controller-gen sources, injecting the
# per-CRD `helm.sh/resource-policy: keep` annotation the chart needs (so
# `helm upgrade` carries field changes and `helm uninstall` preserves the CRD).
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

# Temp files used by --check; cleaned up on exit. Initialised empty so the EXIT
# trap never trips `set -u` regardless of which path the script exits from.
TMP_AG=""
TMP_RG=""
trap 'rm -f "$TMP_AG" "$TMP_RG"' EXIT

# Authoritative controller-gen sources (owned by each module's `make manifests`).
SRC_ACTIONSGATEWAY="cmd/gmc/config/crd/bases/actions-gateway.github.com_actionsgateways.yaml"
SRC_RUNNERGROUP="cmd/agc/config/crd/actions-gateway.github.com_runnergroups.yaml"
# The GMC bundles its own controller-gen copy of the (imported) RunnerGroup type;
# it must stay byte-identical to the AGC authoritative copy or a k8s.io/api skew
# silently prunes fields on deploy (Q73).
GMC_BUNDLED_RUNNERGROUP="cmd/gmc/config/crd/bases/actions-gateway.github.com_runnergroups.yaml"

# Generated chart templates (derived — do not hand-edit; run `make chart-crds`).
DST_ACTIONSGATEWAY="charts/actions-gateway/templates/crds/actionsgateway-crd.yaml"
DST_RUNNERGROUP="charts/actions-gateway/templates/crds/runnergroup-crd.yaml"

# Per-CRD annotation block injected after the controller-gen version line. Kept
# here (not in the chart copies) so the chart files are fully generated.
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
	render "$SRC_ACTIONSGATEWAY" "$DST_ACTIONSGATEWAY" "$ACTIONSGATEWAY_BLOCK"
	render "$SRC_RUNNERGROUP" "$DST_RUNNERGROUP" "$RUNNERGROUP_BLOCK"
}

# check renders the chart CRDs to temp files and compares them against the
# on-disk chart copies — it does NOT mutate the working tree, so it detects any
# drift in the committed chart (and any uncommitted hand-edit), not just whether
# a regen-in-place produced a git diff.
check() {
	local rc=0
	TMP_AG="$(mktemp)"
	TMP_RG="$(mktemp)"
	render "$SRC_ACTIONSGATEWAY" "$TMP_AG" "$ACTIONSGATEWAY_BLOCK"
	render "$SRC_RUNNERGROUP" "$TMP_RG" "$RUNNERGROUP_BLOCK"
	if ! diff -u "$DST_ACTIONSGATEWAY" "$TMP_AG"; then
		echo "ERROR: $DST_ACTIONSGATEWAY is out of sync with $SRC_ACTIONSGATEWAY." >&2
		echo "Re-sync and commit: make chart-crds" >&2
		rc=1
	fi
	if ! diff -u "$DST_RUNNERGROUP" "$TMP_RG"; then
		echo "ERROR: $DST_RUNNERGROUP is out of sync with $SRC_RUNNERGROUP." >&2
		echo "Re-sync and commit: make chart-crds" >&2
		rc=1
	fi
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
		echo "wrote $DST_ACTIONSGATEWAY and $DST_RUNNERGROUP from controller-gen sources."
		;;
	*)
		echo "usage: $0 [--check]" >&2
		exit 2
		;;
	esac
}

main "$@"
