#!/usr/bin/env bash
#
# gen-api-reference.sh - Generate docs/reference/api.md from the api/v2beta1
# kubebuilder markers (Q632).
#
# The CRD schemas were the only field-level authority, so an operator asking what
# a field does had `kubectl explain` and nothing on the docs site. crd-ref-docs
# reads the same Go doc comments and validation markers controller-gen turns into
# the CRD schemas, so the page cannot describe a field the API does not have.
#
# Scope is v2beta1 only — the served, storage, non-deprecated version. v1alpha1
# and v2alpha1 are deprecated and removed at v2.0.0; documenting them field by
# field beside v2beta1 would read as a supported choice. Their readers are served
# by docs/operations/v1alpha1-deprecation.md and migration-v1-to-v2.md.
#
#   scripts/docs/gen-api-reference.sh          # write docs/reference/api.md (make api-reference)
#   scripts/docs/gen-api-reference.sh --check  # fail if the committed page is stale
#                                              # (make api-reference-check, in `make check`)
#
# Inputs: api/hack/crd-ref-docs/config.yaml and the markdown templates beside it.
# Params: CRD_REF_DOCS (path to the tool binary; the Makefile builds it from the
# vendored tools/ module into .build/).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

CRD_REF_DOCS="${CRD_REF_DOCS:-$REPO_ROOT/.build/crd-ref-docs}"
SOURCE_PATH="api/v2beta1"
CONFIG="api/hack/crd-ref-docs/config.yaml"
TEMPLATES_DIR="api/hack/crd-ref-docs/templates"
OUTPUT="docs/reference/api.md"

# crd-ref-docs loads the source package with go/packages. The api module is
# reached through its generated workspace file (the same one api/Makefile hands
# controller-gen), and go rejects a relative GOWORK.
API_GOWORK="$REPO_ROOT/api/go.work.gen"

check_mode=false
if [[ "${1:-}" == "--check" ]]; then
	check_mode=true
elif [[ $# -gt 0 ]]; then
	echo "usage: $0 [--check]" >&2
	exit 2
fi

if [[ ! -x "$CRD_REF_DOCS" ]]; then
	echo "error: crd-ref-docs not found at $CRD_REF_DOCS — run 'make tools'" >&2
	exit 1
fi

# render.kubernetesVersion picks which kubernetes.io schema the embedded core
# types (PodTemplateSpec, Affinity, ObjectMeta) link to. Nothing else reads it, so
# left alone it silently falls behind the k8s.io/api the CRDs actually embed and
# the page links a schema the API no longer matches. k8s.io/api v0.N.x is
# Kubernetes 1.N. Both modes assert it, so the writing path cannot mint stale
# links either.
api_minor="$(awk '$1 == "k8s.io/api" && $2 ~ /^v0\.[0-9]+\./ { split($2, v, "."); print v[2]; exit }' api/go.mod)"
config_version="$(awk -F': *' '$1 ~ /kubernetesVersion$/ { print $2; exit }' "$CONFIG")"
if [[ -z "$api_minor" ]]; then
	echo "error: no k8s.io/api requirement found in api/go.mod — has the module file moved?" >&2
	exit 1
fi
if [[ "$config_version" != "1.${api_minor}" ]]; then
	echo "error: $CONFIG pins render.kubernetesVersion: ${config_version:-<unset>}, but api/go.mod" >&2
	echo "       requires k8s.io/api v0.${api_minor}.x (Kubernetes 1.${api_minor}). Set it to" >&2
	echo "       1.${api_minor} and run 'make api-reference'." >&2
	exit 1
fi

work_dir="tmp/api-reference"
# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11 misses
# that whenever the script ends in an explicit `exit`.
cleanup() {
	rm -rf "${work_dir}"
}
trap cleanup EXIT INT TERM
rm -rf "${work_dir}"
mkdir -p "${work_dir}"

generated="${work_dir}/api.md"
GOWORK="$API_GOWORK" "$CRD_REF_DOCS" \
	--config "$CONFIG" \
	--source-path "$SOURCE_PATH" \
	--templates-dir "$TEMPLATES_DIR" \
	--renderer markdown \
	--output-path "$generated" \
	--log-level ERROR

# A tool that silently rendered nothing would make --check pass on any tree and
# the writing mode replace a good page with a stub. Every v2beta1 kind must be in
# the output before it counts as a generation.
for kind in ActionsGateway ClusterRunnerTemplate EgressProxy PriorityClassAllowlist RunnerSet RunnerTemplate; do
	if ! grep -q "^#### ${kind}\$" "$generated"; then
		echo "error: generated reference has no '#### ${kind}' section — crd-ref-docs found no API types?" >&2
		exit 1
	fi
done

if [[ "$check_mode" == true ]]; then
	if ! diff -u "$OUTPUT" "$generated" > "${work_dir}/api.diff" 2>&1; then
		echo "error: $OUTPUT is stale relative to $SOURCE_PATH — run 'make api-reference'" >&2
		cat "${work_dir}/api.diff" >&2
		exit 1
	fi
	echo "$OUTPUT is in sync with $SOURCE_PATH"
	exit 0
fi

mkdir -p "$(dirname "$OUTPUT")"
cp "$generated" "$OUTPUT"
echo "wrote $OUTPUT from $SOURCE_PATH"
exit 0
