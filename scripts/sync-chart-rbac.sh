#!/usr/bin/env bash
#
# Single source of truth for the Helm chart's GMC manager-role RBAC rules
# (Q142 slice C).
#
# The chart's manager ClusterRole (charts/actions-gateway/templates/rbac.yaml)
# templates the role's metadata/names/binding, but its *rules* are the
# controller-gen output of the `+kubebuilder:rbac` markers on the GMC
# controllers. Hand-copying those rules into the chart invites silent drift: a
# permission added via a marker but not propagated to the chart copy is missing
# from the deployed GMC, which then fails closed at runtime when it first needs
# the grant. This script regenerates the chart's rules fragment FROM the
# controller-gen source so the chart can `.Files.Get` it — the rules live in one
# place (the markers → role.yaml), the chart wraps them.
#
#   scripts/sync-chart-rbac.sh            # write the chart rules fragment (make chart-rbac)
#   scripts/sync-chart-rbac.sh --check    # fail if the chart fragment is stale (make chart-rbac-check)
#
# The --check mode backs the CI drift gate (manifest-validate.yml) and `make check`.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Temp file used by --check; cleaned up on exit. Initialised empty so the EXIT
# trap never trips `set -u` regardless of which path the script exits from.
TMP_RULES=""
trap 'rm -f "$TMP_RULES"' EXIT

# Authoritative controller-gen source (owned by `make -C cmd/gmc manifests`,
# generated from the GMC controllers' +kubebuilder:rbac markers).
SRC_ROLE="cmd/gmc/config/rbac/role.yaml"

# Generated chart data fragment (derived — do not hand-edit; run `make chart-rbac`).
# Lives under files/ (not templates/) so `.Files.Get` can read it at render time;
# templates/rbac.yaml embeds it under the manager ClusterRole's `rules:` key.
DST_RULES="charts/actions-gateway/files/manager-role-rules.yaml"

# render — extract the `rules:` list body from the controller-gen role and write
# it to DST, prefixed with a generated-by banner. Everything after the top-level
# `rules:` line in role.yaml is the policy-rule sequence; metadata/name/kind are
# templated by the chart, so they are dropped here.
render() {
	local src="$1" dst="$2"
	{
		printf '# Generated from %s by '\''make chart-rbac'\'' — DO NOT EDIT.\n' "$src"
		printf '# The GMC manager ClusterRole rules are the controller-gen output of the\n'
		printf '# +kubebuilder:rbac markers; templates/rbac.yaml embeds this fragment so the\n'
		printf '# rules stay single-sourced. Re-run after changing any RBAC marker (Q142).\n'
		awk '
			found { print }
			/^rules:/ { found = 1 }
			END {
				if (!found) {
					print "sync-chart-rbac: no top-level rules: line found in source" > "/dev/stderr"
					exit 1
				}
			}
		' "$src"
	} > "$dst"
}

sync() {
	render "$SRC_ROLE" "$DST_RULES"
}

# check renders the fragment to a temp file and compares it against the on-disk
# chart copy — it does NOT mutate the working tree, so it detects drift in the
# committed chart (and any uncommitted hand-edit), not just whether a regen
# produced a git diff.
check() {
	TMP_RULES="$(mktemp)"
	render "$SRC_ROLE" "$TMP_RULES"
	if ! diff -u "$DST_RULES" "$TMP_RULES"; then
		echo "ERROR: $DST_RULES is out of sync with $SRC_ROLE." >&2
		echo "Re-sync and commit: make chart-rbac" >&2
		exit 1
	fi
	echo "chart manager-role rules fragment is in sync with $SRC_ROLE."
}

main() {
	case "${1:-}" in
	--check)
		check
		;;
	"")
		sync
		echo "wrote $DST_RULES from $SRC_ROLE."
		;;
	*)
		echo "usage: $0 [--check]" >&2
		exit 2
		;;
	esac
}

main "$@"
