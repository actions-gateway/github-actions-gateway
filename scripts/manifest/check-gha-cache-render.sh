#!/usr/bin/env bash
#
# check-gha-cache-render.sh - Render the deploy/gha-cache/ tenant example, with
# and without its persistence component, and assert what each render must
# contain (Q215).
#
# An operator copies deploy/gha-cache/example/, sets its namespace, and runs
# `kubectl apply -k`. manifest-validate.sh yamllints the tree and kubeconforms
# its standalone files, and neither builds a kustomization, so a copy that does
# not render first fails on the operator's cluster.
#
# Exit status alone is not enough, because the example's replacement can be
# wrong and still render. Measured 2026-11-01 on kubectl v1.34 (kustomize
# v5.8.1): `index: 2` in place of `index: 1` exits 0 and renders API_BASE_URL as
# http://gha-cache.NAMESPACE.tenant-a.cluster.local:3000, a URL that resolves
# nowhere. Every job's cache then fails its upload, and its artifacts with it.
#
# What this asserts:
#
#   1. The declared target list still describes the tree: every directory with a
#      kustomization.yaml is the render target or a declared non-target. Runs
#      without kubectl.
#   2. The example renders, and so does a copy with its `components` entry
#      uncommented, which is the README's persistence step.
#   3. In both renders API_BASE_URL is the server's own fully qualified address
#      in the rendered namespace. The worker's NO_PROXY exempts
#      svc.cluster.local and nothing shorter.
#   4. The default render keeps the emptyDir and carries no PVC; the persistent
#      render carries the namespaced PVC and no emptyDir. A patch that lost its
#      `emptyDir: null` renders a volume with two sources at exit 0.
#   5. Both NetworkPolicies survive both renders.
#   6. CACHE_MAX_SIZE_BYTES sits below the emptyDir's sizeLimit and below the
#      PVC's request. The server enforces its budget after an upload finalizes,
#      so a disk sized at the budget fills before eviction runs.
#
# Rules 2-6 need kubectl, which is e2e-tier in scripts/ci/check-tools.sh, so
# without it they degrade to a printed skip; --require-render turns that skip
# into a failure, and the CI step passes it, for the reason
# check-registry-mirror-render.sh gives.
#
# Backs `make gha-cache-render-check`; assertions in check-gha-cache-render-test.sh
# under `make scripts-test`.
#
# Options (for the test suite; both default to the real tree):
#   --tree PATH        the gha-cache tree to check
#   --require-render   fail rather than skip when kubectl is absent
set -euo pipefail
shopt -s inherit_errexit

cd "$(git rev-parse --show-toplevel)"

TREE="deploy/gha-cache"
REQUIRE_RENDER=0

TARGET=example
# base               - not applied directly: its API_BASE_URL carries a
#                      NAMESPACE placeholder that only the example's
#                      replacement fills in.
# components/persistent - a Component does not render alone; covered by the
#                      persistent render of the example.
NON_TARGETS=(base components/persistent)

# The two lines the README tells an operator to uncomment, exactly as shipped.
COMPONENTS_LINE='# components:'
PERSISTENT_LINE='#   - ../components/persistent'

POLICIES=(workload-to-gha-cache gha-cache-ingress)
PVC=gha-cache-data

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
		printf 'check-gha-cache-render.sh: unknown argument: %s\n' "$1" >&2
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

contains() {
	local item="$1" element
	shift
	for element in "$@"; do
		[[ "$element" == "$item" ]] && return 0
	done
	return 1
}

[[ -d "$TREE" ]] || {
	printf 'check-gha-cache-render.sh: no such tree: %s\n' "$TREE" >&2
	exit 2
}

# ------------------------------------------------------- rule 1: the list ----

found=()
while IFS= read -r kust; do
	dir="$(dirname "$kust")"
	dir="${dir#"$TREE"}"
	dir="${dir#/}"
	found+=("${dir:-.}")
done < <(find "$TREE" -name kustomization.yaml -type f | LC_ALL=C sort)

for dir in "${found[@]}"; do
	if [[ "$dir" != "$TARGET" ]] && ! contains "$dir" "${NON_TARGETS[@]}"; then
		fail "$TREE/$dir carries a kustomization.yaml but is neither the render target nor a declared non-target.
       Render it in this script, or add it to NON_TARGETS with the reason it cannot render alone."
	fi
done
for target in "$TARGET" "${NON_TARGETS[@]}"; do
	contains "$target" "${found[@]}" ||
		fail "this script declares '$target', which has no kustomization.yaml under $TREE"
done

# --------------------------------------------------------------- renders ----

if ! command -v kubectl >/dev/null 2>&1; then
	if ((REQUIRE_RENDER)); then
		fail "--require-render was passed and kubectl is not on PATH, so nothing below ran."
	else
		printf 'note: kubectl not on PATH (it is e2e-tier in scripts/ci/check-tools.sh), so the render assertions were skipped\n'
	fi
	((fails == 0)) || exit 1
	printf 'gha-cache target list agrees with the tree (renders not checked)\n'
	exit 0
fi

mkdir -p tmp
scratch="$(mktemp -d tmp/gha-cache-render.XXXXXX)"
trap 'rm -rf "$scratch"' EXIT

# The persistent copy: the whole tree, so the example's relative paths hold,
# with the two commented lines uncommented as the README says.
cp -R "$TREE" "$scratch/tree"
persistent_kust="$scratch/tree/$TARGET/kustomization.yaml"
uncommented="$(awk -v c="$COMPONENTS_LINE" -v p="$PERSISTENT_LINE" -v out="$persistent_kust.new" '
	$0 == c { print substr($0, 3) > out; n++; next }
	$0 == p { print substr($0, 3) > out; n++; next }
	{ print > out }
	END { print n + 0 }
' "$persistent_kust")"
mv "$persistent_kust.new" "$persistent_kust"
if ((uncommented != 2)); then
	fail "$TREE/$TARGET/kustomization.yaml no longer carries the commented lines
         $COMPONENTS_LINE
         $PERSISTENT_LINE
       which the README tells an operator to uncomment for persistence."
fi

declare -A RENDERS=()
render() {
	local name="$1" dir="$2" out
	if out="$(kubectl kustomize "$dir" 2>&1)"; then
		RENDERS["$name"]="$out"
	else
		fail "\`kubectl kustomize\` of the $name render does not build, so an operator cannot apply it:
$(awk '{ print "         " $0 }' <<<"$out")"
	fi
}
render default "$TREE/$TARGET"
((uncommented == 2)) && render persistent "$scratch/tree/$TARGET"

# doc KIND NAME - the named document out of a render on stdin.
doc() {
	awk -v kind="kind: $1" -v name="  name: $2" '
		function flush() { if (index(buf, "\n" kind "\n") && index(buf, "\n" name "\n")) printf "%s", buf }
		/^---$/ { flush(); buf = "\n"; next }
		{ buf = buf $0 "\n" }
		BEGIN { buf = "\n" }
		END { flush() }
	'
}

# env_value NAME - the value of the named env var in a document on stdin.
env_value() {
	awk -v want="- name: $1" '
		found && /value:/ { sub(/^[^:]*value:[[:space:]]*/, ""); gsub(/"/, ""); print; exit }
		{ line = $0; sub(/^[[:space:]]*/, "", line) }
		line == want { found = 1 }
	'
}

# to_bytes QTY - a Kubernetes binary quantity (Ki/Mi/Gi/Ti) or plain bytes.
to_bytes() {
	local qty="$1"
	case "$qty" in
	*Ki) echo $((${qty%Ki} * 1024)) ;;
	*Mi) echo $((${qty%Mi} * 1024 ** 2)) ;;
	*Gi) echo $((${qty%Gi} * 1024 ** 3)) ;;
	*Ti) echo $((${qty%Ti} * 1024 ** 4)) ;;
	*[!0-9]* | '') echo -1 ;;
	*) echo "$qty" ;;
	esac
}

for name in default persistent; do
	[[ -n "${RENDERS[$name]:-}" ]] || continue
	r="${RENDERS[$name]}"
	deploy="$(doc Deployment gha-cache <<<"$r")"
	if [[ -z "$deploy" ]]; then
		fail "the $name render has no Deployment gha-cache"
		continue
	fi

	# Rule 3.
	ns="$(awk '/^  namespace: / { print $2; exit }' <<<"$deploy")"
	url="$(env_value API_BASE_URL <<<"$deploy")"
	want="http://gha-cache.$ns.svc.cluster.local:3000"
	[[ -n "$ns" && "$url" == "$want" ]] ||
		fail "the $name render sets API_BASE_URL to '$url', not '$want'.
       The server hands this address to every job for its uploads, so a wrong one
       fails every cache save and every artifact. Check the replacement in
       $TREE/$TARGET/kustomization.yaml."

	# Rule 4.
	has_empty=0
	grep -qE '^ +(- )?emptyDir:' <<<"$deploy" && has_empty=1
	has_claim=0
	grep -qE "^ +claimName: $PVC\$" <<<"$deploy" && has_claim=1
	pvc_doc="$(doc PersistentVolumeClaim "$PVC" <<<"$r")"
	if [[ "$name" == default ]]; then
		((has_empty && !has_claim)) && [[ -z "$pvc_doc" ]] ||
			fail "the default render must keep the emptyDir and carry no PVC (emptyDir=$has_empty, claim=$has_claim, PVC document=$([[ -n "$pvc_doc" ]] && echo yes || echo no)).
       A PVC bills while it exists, so persistence is opt-in."
	else
		((has_claim && !has_empty)) ||
			fail "the persistent render must mount $PVC and carry no emptyDir (emptyDir=$has_empty, claim=$has_claim).
       A volume with both sources is rejected by the API server; check the
       component's \`emptyDir: null\`."
		if [[ -z "$pvc_doc" ]]; then
			fail "the persistent render has no PersistentVolumeClaim $PVC"
		elif ! grep -qxF "  namespace: $ns" <<<"$pvc_doc"; then
			fail "the persistent render's PVC $PVC is not in the server's namespace '$ns'"
		fi
	fi

	# Rule 5.
	for policy in "${POLICIES[@]}"; do
		[[ -n "$(doc NetworkPolicy "$policy" <<<"$r")" ]] ||
			fail "the $name render has no NetworkPolicy $policy. Without both, workers cannot
       reach the server, or something other than a worker can."
	done

	# Rule 6.
	budget="$(env_value CACHE_MAX_SIZE_BYTES <<<"$deploy")"
	if [[ "$name" == default ]]; then
		disk_name="emptyDir sizeLimit"
		disk="$(awk '/^ +sizeLimit:/ { print $2; exit }' <<<"$deploy")"
	else
		disk_name="PVC request"
		disk="$(awk '/^ +storage:/ { print $2; exit }' <<<"$pvc_doc")"
	fi
	budget_b="$(to_bytes "$budget")"
	disk_b="$(to_bytes "$disk")"
	if ((budget_b < 0 || disk_b < 0)); then
		fail "the $name render's storage figures did not parse: CACHE_MAX_SIZE_BYTES='$budget', $disk_name='$disk'"
	elif ((budget_b >= disk_b)); then
		fail "the $name render's CACHE_MAX_SIZE_BYTES ($budget) is not below its $disk_name ($disk).
       The server evicts only after an upload finalizes, so the disk must hold the
       budget plus the uploads in flight."
	fi
done

# ---------------------------------------------------------------- verdict ----

if ((fails > 0)); then
	printf '\n%d gha-cache render check(s) failed. See %s/README.md.\n' "$fails" "$TREE" >&2
	exit 1
fi

printf 'gha-cache renders: default and persistent, API_BASE_URL fully qualified, %d NetworkPolicies, budget below the disk in both\n' "${#POLICIES[@]}"
