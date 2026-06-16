#!/usr/bin/env bash
#
# Validate the shipped install artifact: yamllint over the controller-gen
# manifests and chart, kubeconform schema-validation of every chart render,
# helm lint, and the fail-closed digest-pinning assertion (Q96). Backs
# `make manifest-validate` and mirrors the CI `validate` job in
# .github/workflows/manifest-validate.yml exactly so local and CI verdicts
# match. Requires yamllint, kubeconform, and helm on PATH.
#
# The Helm chart is the SOLE install path (Q142): there is no kustomize overlay
# to render. The plain-YAML files left under cmd/*/config/ are controller-gen
# output (CRDs, RBAC, webhook) retained as the codegen + envtest substrate and
# the single-source inputs to the chart CRD/RBAC generators; they are
# schema-validated below as standalone manifests.
#
# Env:
#   MANIFEST_K8S_VERSION   Kubernetes version kubeconform validates against
#                          (default 1.30.0 — the chart's kubeVersion floor in
#                          Chart.yaml: validating against the oldest supported
#                          version catches a field that does not exist there).
#   KUBECONFORM_CACHE      Directory persisting kubeconform's downloaded JSON
#                          schemas between runs (CI points it at a cached path
#                          to avoid re-downloading the schema set every run);
#                          empty by default for local use.
#   POLARIS_RENDER_DIGEST  Placeholder digest used for the digest-pinned chart
#                          renders — see scripts/lib/common.sh.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

require_cmd yamllint "https://yamllint.readthedocs.io/en/stable/quickstart.html"
require_cmd kubeconform "https://github.com/yannh/kubeconform#installation"
require_cmd helm "https://helm.sh/docs/intro/install/"

MANIFEST_K8S_VERSION="${MANIFEST_K8S_VERSION:-1.30.0}"
KUBECONFORM_CACHE="${KUBECONFORM_CACHE:-}"

chart="$REPO_ROOT/charts/actions-gateway"

# kubeconform flags: -strict rejects unknown fields; -ignore-missing-schemas
# skips resources whose schema is not in the upstream Kubernetes set —
# cert-manager (Certificate/Issuer), the Prometheus Operator (ServiceMonitor)
# and our own CRs (ActionsGateway/RunnerGroup). Those are third-party/custom
# kinds; the CRDs that define them ARE validated (CustomResourceDefinition is
# a native apiextensions kind).
kubeconform_flags="-strict -summary -kubernetes-version $MANIFEST_K8S_VERSION -ignore-missing-schemas"
[[ -n "$KUBECONFORM_CACHE" ]] && kubeconform_flags+=" -cache $KUBECONFORM_CACHE"

yamllint_paths="charts/actions-gateway cmd/agc/config cmd/gmc/config"

# The plain-YAML files retained under cmd/*/config/: the controller-gen outputs
# (CRDs, manager RBAC role, webhook config) that are the codegen substrate and
# single-source inputs to the chart CRD/RBAC generators, plus the two
# ValidatingAdmissionPolicies the GMC integration suite applies in envtest.
# Schema-validate them directly since there is no longer a kustomize overlay that
# renders them.
standalone_manifests="cmd/agc/config/rbac/role.yaml
cmd/agc/config/crd/actions-gateway.github.com_runnergroups.yaml
cmd/gmc/config/rbac/role.yaml
cmd/gmc/config/webhook/manifests.yaml
cmd/gmc/config/crd/bases/actions-gateway.github.com_actionsgateways.yaml
cmd/gmc/config/crd/bases/actions-gateway.github.com_runnergroups.yaml
cmd/gmc/config/admission-policy/namespace-psa-guard.yaml
cmd/gmc/config/admission-policy/tenant-resource-guard.yaml"

echo "==> yamllint (static manifests + chart metadata)"
# shellcheck disable=SC2086  # path and flag lists word-split intentionally
yamllint --strict -c "$REPO_ROOT/.yamllint.yaml" $yamllint_paths

echo "==> kubeconform: controller-gen manifests (codegen substrate; k8s $MANIFEST_K8S_VERSION)"
# shellcheck disable=SC2086
kubeconform $kubeconform_flags $standalone_manifests

echo "==> helm lint (digest-pinned: default values must not render — checked next)"
helm lint "$chart" --set-string "gmc.image.digest=$POLARIS_RENDER_DIGEST"

echo "==> helm template: default values must FAIL closed (gmc.image digest unpinned; Q96)"
if out="$(helm template ag "$chart" 2>&1)"; then
	echo "ERROR: chart rendered with default values — gmc.image digest pinning regressed to fail-open" >&2
	exit 1
elif ! grep -q "gmc.image must be pinned by digest" <<<"$out"; then
	echo "ERROR: default-values render failed, but not with the digest-pinning rejection:" >&2
	echo "$out" >&2
	exit 1
fi

echo "==> kubeconform: Helm chart render (digest-pinned defaults)"
# shellcheck disable=SC2086
helm template ag "$chart" --set-string "gmc.image.digest=$POLARIS_RENDER_DIGEST" \
	| kubeconform $kubeconform_flags

echo "==> kubeconform: Helm chart render (dev/test opt-out: allowFloatingImageTags=true)"
# shellcheck disable=SC2086
helm template ag "$chart" --set allowFloatingImageTags=true \
	| kubeconform $kubeconform_flags

echo "==> kubeconform: Helm chart render (all optional features: ServiceMonitor + sample CR + self-signed cert)"
# shellcheck disable=SC2086
helm template ag "$chart" --set-string "gmc.image.digest=$POLARIS_RENDER_DIGEST" \
	--set metrics.serviceMonitor.enabled=true --set sampleGateway.create=true --set certManager.enabled=false \
	| kubeconform $kubeconform_flags

echo "OK: install artifact validates"
