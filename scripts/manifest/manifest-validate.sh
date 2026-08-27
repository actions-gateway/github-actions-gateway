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
shopt -s inherit_errexit

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
# The opt-in v2alpha1 (actions-gateway.com) CRD chart, shipped separately so its
# large pod-template CRDs do not push the main chart's Helm release past the 1 MiB
# limit (Q149). CRD-only: no images, no digest pinning.
crds_v2_chart="$REPO_ROOT/charts/actions-gateway-crds-v2"

# kubeconform flags: -strict rejects unknown fields; -ignore-missing-schemas
# skips resources whose schema is not in the upstream Kubernetes set —
# cert-manager (Certificate/Issuer), the Prometheus Operator (ServiceMonitor)
# and our own CRs (ActionsGateway/RunnerGroup). Those are third-party/custom
# kinds; the CRDs that define them ARE validated (CustomResourceDefinition is
# a native apiextensions kind).
kubeconform_flags="-strict -summary -kubernetes-version $MANIFEST_K8S_VERSION -ignore-missing-schemas"
[[ -n "$KUBECONFORM_CACHE" ]] && kubeconform_flags+=" -cache $KUBECONFORM_CACHE"

# deploy/templates is the SHIPPED runner template library (Q554): hand-authored
# YAML an operator applies directly, so tabs, a duplicate key or a missing final
# newline would reach them. kubeconform cannot add to that — ClusterRunnerTemplate
# is one of our own CRDs, which -ignore-missing-schemas skips. The schema and CEL
# half is covered where a real apiserver is available instead
# (TestTemplateLibrary_Admitted in the AGC integration suite), and the
# shipped-vs-exercised half by `make template-library-check`.
# deploy/monitoring/prometheusrule.yaml is the shipped reference PrometheusRule
# (Q827), same class as deploy/templates above: deploy/monitoring/README.md tells
# an operator to kubectl apply it, so malformed YAML reaches them. kubeconform
# cannot add to that either — PrometheusRule is a Prometheus Operator kind, which
# -ignore-missing-schemas skips. The rules' PromQL and their agreement with the
# docs are checked separately, by `make promql-check`, which needs no host tool.
# preview/ is deliberately excluded: it is a throwaway kind-cluster harness for
# screenshotting the dashboards, "not part of the chart or any install path" per
# its own README, and its fixtures use flow mappings this config rejects.
# deploy/registry-mirror is the Q408 untrusted-PR pull-through cache: hand-authored
# YAML an operator applies with `kubectl apply -k`, same class as deploy/templates
# above, and a security surface (its NetworkPolicies are the workers' only
# registry path). Its base manifests are plain native kinds, so kubeconform covers
# them too via standalone_manifests below; the kustomization.yaml files are not
# Kubernetes manifests and are yamllint-only, as deploy/kata-ci/kata-values.yaml is.
yamllint_paths="charts/actions-gateway charts/actions-gateway-crds-v2 cmd/agc/config cmd/gmc/config deploy/kata-ci deploy/registry-mirror deploy/templates deploy/monitoring/prometheusrule.yaml"

# The shipped Grafana dashboards. Nothing parsed them before Q827, so a stray
# comma survived to whoever imported the file. jq is required-tier already.
#
# This step is a JSON syntax parse and nothing more: a panel query is a string,
# so `sum by ((((` satisfies it (Q910). The queries themselves are parsed by
# `make promql-check`, which needs no host tool.
dashboards="$REPO_ROOT/deploy/monitoring/grafana-dashboard-tenant.json
$REPO_ROOT/deploy/monitoring/grafana-dashboard-platform.json"

# The plain-YAML files retained under cmd/*/config/: the controller-gen outputs
# (CRDs, manager RBAC role, webhook config) that are the codegen substrate and
# single-source inputs to the chart CRD/RBAC generators, plus the two
# ValidatingAdmissionPolicies the GMC integration suite applies in envtest.
# Schema-validate them directly since there is no longer a kustomize overlay that
# renders them. The deploy/kata-ci/ manifests (Q226 Kata-on-GKE spike) are plain
# static YAML with no chart/overlay either, so they are schema-validated here too.
# deploy/kata-ci/kata-values.yaml is a Helm *values* file for upstream's
# kata-deploy chart, not a Kubernetes manifest — yamllint covers it (via
# yamllint_paths above); kubeconform must not try to schema-check it.
standalone_manifests="cmd/agc/config/rbac/role.yaml
cmd/agc/config/crd/actions-gateway.github.com_runnergroups.yaml
api/config/crd/actions-gateway.com_runnersets.yaml
api/config/crd/actions-gateway.com_runnertemplates.yaml
api/config/crd/actions-gateway.com_clusterrunnertemplates.yaml
api/config/crd/actions-gateway.com_actionsgateways.yaml
api/config/crd/actions-gateway.com_egressproxies.yaml
cmd/gmc/config/rbac/role.yaml
cmd/gmc/config/webhook/manifests.yaml
cmd/gmc/config/crd/bases/actions-gateway.github.com_actionsgateways.yaml
cmd/gmc/config/crd/bases/actions-gateway.github.com_runnergroups.yaml
cmd/gmc/config/admission-policy/namespace-psa-guard.yaml
cmd/gmc/config/admission-policy/namespace-security-profile-guard.yaml
cmd/gmc/config/admission-policy/tenant-resource-guard.yaml
cmd/gmc/config/admission-policy/priorityclass-allowlist-guard.yaml
deploy/kata-ci/runtimeclass.yaml
deploy/kata-ci/runner-pod.yaml
deploy/registry-mirror/base/namespace.yaml
deploy/registry-mirror/base/deployment.yaml
deploy/registry-mirror/base/service.yaml
deploy/registry-mirror/base/networkpolicy.yaml
deploy/registry-mirror/overlays/persistent/pvc.yaml"

echo "==> yamllint (static manifests + chart metadata)"
# shellcheck disable=SC2086  # path and flag lists word-split intentionally
yamllint --strict -c "$REPO_ROOT/.yamllint.yaml" $yamllint_paths

echo "==> jq: shipped Grafana dashboards parse as JSON (Q827; their queries are make promql-check)"
while IFS= read -r dashboard; do
	jq empty "$dashboard"
done <<<"$dashboards"

echo "==> kubeconform: controller-gen manifests (codegen substrate; k8s $MANIFEST_K8S_VERSION)"
# shellcheck disable=SC2086
kubeconform $kubeconform_flags $standalone_manifests

echo "==> helm lint + kubeconform: actions-gateway-crds-v2 (opt-in v2 CRD chart)"
helm lint "$crds_v2_chart"
# shellcheck disable=SC2086
helm template ag-crds-v2 "$crds_v2_chart" | kubeconform $kubeconform_flags

echo "==> helm lint (digest-pinned: default values must not render — checked next)"
helm lint "$chart" "${RENDER_DIGEST_ARGS[@]}"

echo "==> helm template: default values must FAIL closed (gmc.image digest unpinned; Q96)"
if out="$(helm template ag "$chart" 2>&1)"; then
	echo "ERROR: chart rendered with default values — gmc.image digest pinning regressed to fail-open" >&2
	exit 1
elif ! grep -q "gmc.image must be pinned by digest" <<<"$out"; then
	echo "ERROR: default-values render failed, but not with the digest-pinning rejection:" >&2
	echo "$out" >&2
	exit 1
fi

# Q307: every image (gmc/agc/proxy/wrapper) must fail closed at render, not just
# gmc. Pin the other three and assert the fourth is rejected with its own
# per-image message, so an unpinned agc/proxy/wrapper can never regress to
# fail-open (crash-looping the GMC later instead of failing the render).
echo "==> helm template: each image digest must FAIL closed when unpinned (Q307)"
for img in gmc agc proxy wrapper; do
	pins=()
	for other in gmc agc proxy wrapper; do
		[[ "$other" == "$img" ]] && continue
		pins+=(--set-string "$other.image.digest=$POLARIS_RENDER_DIGEST")
	done
	if out="$(helm template ag "$chart" "${pins[@]}" 2>&1)"; then
		echo "ERROR: chart rendered with $img.image.digest empty — $img digest pinning regressed to fail-open" >&2
		exit 1
	elif ! grep -q "$img.image must be pinned by digest" <<<"$out"; then
		echo "ERROR: $img.image-unpinned render failed, but not with the digest-pinning rejection:" >&2
		echo "$out" >&2
		exit 1
	fi
done

echo "==> kubeconform: Helm chart render (digest-pinned defaults)"
# shellcheck disable=SC2086
helm template ag "$chart" "${RENDER_DIGEST_ARGS[@]}" \
	| kubeconform $kubeconform_flags

echo "==> kubeconform: Helm chart render (dev/test opt-out: allowFloatingImageTags=true)"
# shellcheck disable=SC2086
helm template ag "$chart" --set allowFloatingImageTags=true \
	| kubeconform $kubeconform_flags

echo "==> kubeconform: Helm chart render (all optional features: ServiceMonitor + sample CR + self-signed cert)"
# shellcheck disable=SC2086
helm template ag "$chart" "${RENDER_DIGEST_ARGS[@]}" \
	--set metrics.serviceMonitor.enabled=true --set sampleGateway.create=true --set certManager.enabled=false \
	| kubeconform $kubeconform_flags

echo "==> kubeconform: Helm chart render (cert-manager-verified metrics scrape: ServiceMonitor + metrics.tls.certManager)"
# shellcheck disable=SC2086
helm template ag "$chart" "${RENDER_DIGEST_ARGS[@]}" \
	--set metrics.serviceMonitor.enabled=true \
	| kubeconform $kubeconform_flags

echo "==> helm template: admission-policy matchConditions bind to the install-specific GMC ServiceAccount (Q127)"
# The namespace-psa-guard and tenant-resource-guard policies gate the GMC
# ServiceAccount by username (system:serviceaccount:<ns>:<name>). That identity
# MUST track the install (.Release.Namespace + the serviceAccountName helper),
# never a hardcoded gmc-system identity — a GMC installed elsewhere would
# otherwise be silently exempt from its own confinement policies. Render under a
# non-default namespace and assert the rendered username follows it, and that the
# referenced ServiceAccount is actually one the chart creates.
psa_render_ns="psa-guard-render-ns"
psa_render="$(helm template ag "$chart" --namespace "$psa_render_ns" \
	"${RENDER_DIGEST_ARGS[@]}")"
if ! grep -q "system:serviceaccount:${psa_render_ns}:" <<<"$psa_render"; then
	echo "ERROR: admission-policy matchCondition username is not bound to the install namespace ($psa_render_ns); the GMC SA identity appears hardcoded" >&2
	exit 1
fi
if grep -q "system:serviceaccount:gmc-system:" <<<"$psa_render"; then
	echo "ERROR: admission-policy matchCondition still references the default gmc-system namespace under a custom --namespace install — username is not parameterized" >&2
	exit 1
fi
psa_sa_ref="$(grep -oE "system:serviceaccount:${psa_render_ns}:[A-Za-z0-9_.-]+" <<<"$psa_render" | head -1)"
psa_sa_name="${psa_sa_ref##*:}"
if ! awk -v want="$psa_sa_name" '/^kind:/{k=$2} k=="ServiceAccount" && $1=="name:"{if($2==want)f=1} END{exit f?0:1}' <<<"$psa_render"; then
	echo "ERROR: admission policies reference ServiceAccount '$psa_sa_name' but the chart renders no such ServiceAccount" >&2
	exit 1
fi
echo "OK: admission-policy matchConditions bind to the rendered GMC ServiceAccount ($psa_sa_ref)"

echo "==> helm template: no ValidatingAdmissionPolicyBinding survives uninstall"
# A binding is what makes a policy enforce. Retaining one across `helm uninstall`
# would leave the guard active after the release is gone, and would make
# admissionPolicy.enabled=false a silent no-op — the operator turns the guard off
# and it keeps denying. So bindings must never carry helm.sh/resource-policy: keep.
#
# There is deliberately NO matching assertion on the policies. Retaining the
# paramKind-bearing policy was the first attempted fix for Q444 and it did not
# work (reverted in 70b4b351); asserting it here would re-freeze a wrong answer.
# We now know why it could not work: the apiserver builds its set of live
# paramKinds from BINDINGS, so a retained policy with no binding is invisible to
# it. That also means retaining the *binding* is the one thing that would have
# held the informer open — and this assertion deliberately forbids it, because
# the silent-no-op cost above is worse. Q492 fixed Q444 the other way instead, by
# moving the paramKind off a core type onto a CRD, so this invariant survives
# intact. See docs/development/kubernetes-conventions.md.
# Reuses $psa_render — retention annotations do not vary with --namespace.
# Line-oriented, not a multi-character RS: BSD awk (the macOS default) supports
# only a single-character record separator, so `RS = "\n---\n"` silently never
# splits there and every document folds into one record.
policy_retention="$(awk '
	function flush() {
		if (kind != "") { printf "%s\t%s\t%d\n", kind, name, keep }
		kind = ""; name = ""; keep = 0
	}
	/^---[[:space:]]*$/ { flush(); next }
	# Skip YAML comments: the templates explain this very annotation in prose, and
	# a comment mentioning it must not read as the annotation being set.
	/^[[:space:]]*#/ { next }
	/^kind: / { kind = substr($0, 7) }
	name == "" && /^  name: / { name = substr($0, 9) }
	/^[[:space:]]+helm\.sh\/resource-policy:[[:space:]]*keep[[:space:]]*$/ { keep = 1 }
	END { flush() }
' <<<"$psa_render")"

retention_violations=0
while IFS=$'\t' read -r kind name keep; do
	[[ -n "$kind" ]] || continue
	if [[ "$kind" == "ValidatingAdmissionPolicyBinding" && "$keep" == 1 ]]; then
		echo "ERROR: ValidatingAdmissionPolicyBinding '$name' carries helm.sh/resource-policy: keep." >&2
		echo "       Bindings are what make a policy enforce; retaining one leaves the guard active after" >&2
		echo "       uninstall and makes admissionPolicy.enabled=false a silent no-op." >&2
		retention_violations=$((retention_violations + 1))
	fi
done <<<"$policy_retention"

if ((retention_violations > 0)); then
	exit 1
fi
if ! grep -q $'^ValidatingAdmissionPolicyBinding\t' <<<"$policy_retention"; then
	echo "ERROR: the render contains no ValidatingAdmissionPolicyBinding — this check is no longer" >&2
	echo "       exercising anything; update it alongside whatever replaced the bindings." >&2
	exit 1
fi
echo "OK: no admission-policy binding survives uninstall"

echo "OK: install artifact validates"
