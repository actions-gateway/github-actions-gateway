#!/usr/bin/env bash
#
# Released-chart upgrade check (Q507).
#
# chart-upgrade-check.sh (Q475) proves `helm upgrade` delivers changes to a live
# release — but the release it upgrades is HEAD's own chart, so every gate
# answered "does HEAD upgrade to HEAD?" and never "does the chart an operator is
# actually RUNNING upgrade to HEAD?". Those differ whenever a change interacts
# with what Helm does between two chart versions. Q492 proved it: moving the
# PriorityClass guard's paramKind onto a CRD shipped that CRD in the chart-root
# crds/ dir, which Helm installs on `helm install` ONLY and never on upgrade —
# so every existing v1.2.0 release failed to upgrade, and all of CI was green
# because nothing in CI had an older release to upgrade from.
#
# This check closes that class. It replaces the live HEAD release with the LAST
# RELEASED chart — the published OCI artifact operators actually install, pulled
# from GHCR, not a rebuild from an old git ref — and then walks the operator's
# upgrade path to HEAD:
#
#   1. A release whose values set a key HEAD removed must fail AT RENDER with
#      the chart's migration message (today: priorityClassAllowlist.configMapName,
#      removed by Q492), never midway through applying.
#   2. A plain `helm upgrade` to HEAD either succeeds outright, or fails at
#      render with a message that names the documented pre-upgrade step and
#      points at docs/operations/upgrade.md. Any other failure — above all
#      Helm's bare "ensure CRDs are installed first" — is the Q492 shape
#      reintroduced, and fails this check.
#   3. After the documented step (`helm show crds <chart> | kubectl apply -f -`,
#      exactly what the preflight message tells the operator to run), the
#      upgrade must succeed, every CRD HEAD ships in crds/ must exist in the
#      cluster (an upgrade can succeed while silently never delivering one),
#      the restarted manager must come back, the validating webhook must
#      enforce, and the PriorityClass guard's params must resolve.
#   4. An upgrade that SETS the values every step above leaves at their defaults
#      must reach the CRD-SCHEMA preflight and then clear it (Q646). Steps 1-3
#      replay the release's own values, which are the chart defaults, so every
#      preflight gated on a value being set was dead code here: measured on
#      kind against the published v1.3.0 chart, the default-values upgrade
#      reaches none of the three guards in priorityclass-allowlist.yaml. This
#      step sets allowedInfraPriorityClasses (Q298) and allowedPriorityClasses,
#      the one pair whose guard no other step reaches.
#
# "Last released" is discovered dynamically so a new release re-points this
# check automatically: the highest stable vX.Y.Z tag on the origin remote
# (`git ls-remote` — prereleases like v1.2.0-rc.1 are excluded), which is the
# same tag publish.yml keys the chart's OCI version on (tag minus the leading
# "v"). A stable tag whose chart publish failed therefore fails this check
# loudly at `helm pull` — deliberate, since operators cannot install that
# release either. With no stable tag at all (a fresh fork), the check SKIPS
# cleanly. Detail: docs/development/testing.md, "The released-chart upgrade
# gate (Q507)".
#
# Run against a cluster that already has the chart installed (CI runs it after
# the e2e suite, which leaves the release up under E2E_SKIP_TEARDOWN). It runs
# LAST among the chart checks: it uninstalls the live release and leaves the
# cluster on a HEAD release freshly upgraded from the released chart.
#
# Usage: scripts/e2e/chart-released-upgrade-check.sh
#   KIND_CLUSTER        kind cluster name (default actions-gateway-e2e); the kube
#                       context is kind-<name>. Set KUBE_CONTEXT to override.
#   HELM_RELEASE        release name (default actions-gateway)
#   RELEASE_NS          release namespace (default gmc-system)
#   RELEASED_TAG        skip discovery and test the upgrade from this tag
#   RELEASED_CHART_OCI  OCI chart ref base (default oci://ghcr.io/<owner>/charts,
#                       owner parsed from the origin remote URL)
#   REGISTRY_MIRRORS    <upstream>=<mirror> map; when it covers the chart ref's
#                       registry the pull rides the mirror over plain HTTP
#                       (Q408 Phase 3, scripts/lib/registry-mirror.sh)
set -euo pipefail
shopt -s inherit_errexit

KIND_CLUSTER="${KIND_CLUSTER:-actions-gateway-e2e}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER}}"
HELM_RELEASE="${HELM_RELEASE:-actions-gateway}"
RELEASE_NS="${RELEASE_NS:-gmc-system}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/lib/registry-mirror.sh
source "${REPO_ROOT}/scripts/lib/registry-mirror.sh"

readonly CHART_DIR="${REPO_ROOT}/charts/actions-gateway"
readonly PROBE_NS="chart-released-upgrade-check"
# Allowlist entries for the values-set leg. Names only — neither list is checked
# against real PriorityClass objects — but they must be DISJOINT, or the CRD's
# CEL rule rejects the pair and the leg fails for the wrong reason.
readonly PROBE_WORKER_CLASS="released-upgrade-probe-worker"
readonly PROBE_INFRA_CLASS="released-upgrade-probe-infra"

require_cmd helm "https://helm.sh/docs/intro/install/"
require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
require_cmd jq "https://jqlang.github.io/jq/download/"

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
	kubectl --context "${KUBE_CONTEXT}" delete namespace "${PROBE_NS}" \
		--ignore-not-found --wait=false >/dev/null 2>&1 || true
	rm -rf "${work_dir}"
}
trap cleanup EXIT

kc() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

# release_revision echoes the release's current Helm revision number.
release_revision() {
	helm get metadata "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" \
		--namespace "${RELEASE_NS}" -o json | jq -r '.revision'
}

# release_chart_version echoes the chart version the release currently runs.
release_chart_version() {
	helm get metadata "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" \
		--namespace "${RELEASE_NS}" -o json | jq -r '.version'
}

# upgrade_to_head attempts the released -> HEAD upgrade with the captured
# values. Callers inspect UPGRADE_RC / UPGRADE_OUT; results come back through
# globals because a command substitution would swallow the exit code under
# `set -e`.
UPGRADE_RC=0
UPGRADE_OUT=""
upgrade_to_head() {
	UPGRADE_RC=0
	UPGRADE_OUT="$(helm upgrade "${HELM_RELEASE}" "${CHART_DIR}" \
		--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
		-f "${work_dir}/values.yaml" --wait --timeout 10m 2>&1)" || UPGRADE_RC=$?
}

# infra_field_declared echoes "yes" when the PriorityClassAllowlist CRD stored in
# the cluster declares spec.allowedInfraPriorityClasses on any version, and empty
# otherwise (CRD absent included). This is the exact condition the chart's
# schema preflight tests via `lookup`, read the same way, so the check can tell
# "the guard should fire" from "the CRD is already current" instead of assuming
# which branch step 2 took.
infra_field_declared() {
	local schema
	schema="$(kc get crd priorityclassallowlists.actions-gateway.com -o json 2>/dev/null)" || return 0
	jq -r 'if [.spec.versions[]
		| select(.schema.openAPIV3Schema.properties.spec.properties
			| has("allowedInfraPriorityClasses"))] | length > 0
		then "yes" else "" end' <<<"${schema}"
}

# upgrade_with_lists attempts the upgrade to HEAD with both PriorityClass
# allowlists SET rather than defaulted. Same globals-not-substitution reason as
# upgrade_to_head; extra helm args are forwarded verbatim.
upgrade_with_lists() {
	UPGRADE_RC=0
	UPGRADE_OUT="$(helm upgrade "${HELM_RELEASE}" "${CHART_DIR}" \
		--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
		-f "${work_dir}/values.yaml" \
		--set "allowedPriorityClasses={${PROBE_WORKER_CLASS}}" \
		--set "allowedInfraPriorityClasses={${PROBE_INFRA_CLASS}}" \
		"$@" 2>&1)" || UPGRADE_RC=$?
}

# assert_webhook_enforces fails unless the GMC validating webhook denies an
# ActionsGateway in a reserved namespace — the same one-probe health check
# chart-upgrade-check.sh uses: failurePolicy is Fail, so a connection error
# means the webhook is unreachable and an admitted create means it stopped
# validating. Retried because the upgrade restarts the manager and the Service
# endpoint takes a moment to follow.
assert_webhook_enforces() {
	local answer="" i
	for ((i = 1; i <= 45; i++)); do
		answer="$(kc create -f - 2>&1 <<-EOF || true
			apiVersion: actions-gateway.github.com/v1alpha1
			kind: ActionsGateway
			metadata:
			  name: released-upgrade-webhook-probe
			  namespace: kube-system
			spec:
			  gitHubURL: https://github.com/example-org
			  gitHubAppRef:
			    name: chart-released-upgrade-check-nonexistent
		EOF
		)"
		[[ "${answer}" == *"reserved namespace"* ]] && break
		sleep 2
	done
	if [[ "${answer}" != *"reserved namespace"* ]]; then
		echo "FAIL: after the released -> HEAD upgrade, the GMC validating webhook never denied" >&2
		echo "  an ActionsGateway in kube-system. Last answer from the apiserver:" >&2
		echo "  ${answer}" >&2
		echo "  A connection/timeout error means the webhook is unreachable (Service endpoints," >&2
		echo "  serving cert, or the manager itself); an admitted create means it is reachable" >&2
		echo "  but no longer validating." >&2
		kc get pods -n "${RELEASE_NS}" -o wide >&2 || true
		return 1
	fi
	echo "OK: the validating webhook is reachable and enforcing"
}

# assert_params_resolve fails unless the PriorityClass guard denies an
# off-allowlist class AND admits a plain write — the same probe pair
# chart-reinstall-check.sh gates on, because it distinguishes a healthy guard
# from both the Q444 no-params outage and a guard that denies everything.
assert_params_resolve() {
	local answer="" plain="" i
	for ((i = 1; i <= 45; i++)); do
		answer="$(kc create -f - 2>&1 <<-EOF || true
			apiVersion: actions-gateway.github.com/v1alpha1
			kind: RunnerGroup
			metadata:
			  name: probe-class-${i}
			  namespace: ${PROBE_NS}
			spec:
			  maxListeners: 1
			  runnerLabels: ["self-hosted"]
			  podTemplate:
			    spec:
			      priorityClassName: system-cluster-critical
			      containers:
			        - name: runner
			          image: runner:test
		EOF
		)"
		case "${answer}" in
		*"not in the platform PriorityClassAllowlist"*) break ;;
		*created*)
			kc delete runnergroup -n "${PROBE_NS}" "probe-class-${i}" \
				--ignore-not-found >/dev/null 2>&1 || true
			;;
		esac
		sleep 2
	done
	if [[ "${answer}" != *"not in the platform PriorityClassAllowlist"* ]]; then
		echo "FAIL: after the released -> HEAD upgrade, the PriorityClass guard never denied an" >&2
		echo "  off-allowlist class. Last answer:" >&2
		echo "  ${answer}" >&2
		echo "  'no params found' persisting for the whole budget is the Q444 signature; an" >&2
		echo "  admitted create means the guard is not enforcing at all." >&2
		return 1
	fi
	plain="$(kc create -f - 2>&1 <<-EOF || true
		apiVersion: actions-gateway.github.com/v1alpha1
		kind: RunnerGroup
		metadata:
		  name: probe-plain
		  namespace: ${PROBE_NS}
		spec:
		  maxListeners: 1
		  runnerLabels: ["self-hosted"]
		  podTemplate:
		    spec:
		      containers:
		        - name: runner
		          image: runner:test
	EOF
	)"
	if [[ "${plain}" != *created* ]]; then
		echo "FAIL: after the released -> HEAD upgrade, a class-free RunnerGroup was not admitted:" >&2
		echo "  ${plain}" >&2
		return 1
	fi
	echo "OK: the PriorityClass guard's params resolve and ordinary writes are admitted"
}

step "target: context=${KUBE_CONTEXT} release=${HELM_RELEASE} ns=${RELEASE_NS}"
kc config current-context >/dev/null

if ! helm status "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" >/dev/null 2>&1; then
	die "release ${HELM_RELEASE} is not installed in ${RELEASE_NS}; install it first."
fi

step "discovering the last released tag"
if [[ -z "${RELEASED_TAG:-}" ]]; then
	# The highest stable vX.Y.Z tag on the remote — prerelease tags (-rc.N etc.)
	# never publish as "the release operators run", so they are excluded. Read
	# from the remote, not the local tag list, which may be stale or unfetched.
	RELEASED_TAG="$(git -C "${REPO_ROOT}" ls-remote --tags --refs origin 'v*' |
		awk -F/ '$NF ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/ { print $NF }' |
		sort -V | tail -1)"
fi
if [[ -z "${RELEASED_TAG}" ]]; then
	echo "SKIP: the origin remote has no stable vX.Y.Z tag, so there is no released chart"
	echo "      to upgrade from (expected on a fresh fork). Nothing was checked."
	exit 0
fi
released_version="${RELEASED_TAG#v}"
echo "     released tag: ${RELEASED_TAG} (chart version ${released_version})"

if [[ -z "${RELEASED_CHART_OCI:-}" ]]; then
	# publish.yml pushes the chart to oci://ghcr.io/<repository owner>/charts;
	# derive the owner from the origin URL so a fork tests its own artifacts.
	origin_url="$(git -C "${REPO_ROOT}" remote get-url origin)"
	origin_url="${origin_url%.git}"
	origin_url="${origin_url%/*}"
	owner="$(tr '[:upper:]' '[:lower:]' <<<"${origin_url##*[:/]}")"
	RELEASED_CHART_OCI="oci://ghcr.io/${owner}/charts"
fi
readonly released_chart_ref="${RELEASED_CHART_OCI}/actions-gateway"

# Q408 Phase 3: helm's OCI client takes neither dockerd's registry-mirror nor a
# rewritten docker ref, so the chart pull is the one client wired by rewriting
# the ref here. The mirror speaks plain HTTP in-cluster, which helm will not do
# unless told to — and --plain-http is added ONLY when a rewrite happened, so a
# direct ghcr.io pull is never downgraded off TLS.
helm_pull_ref="${released_chart_ref}"
helm_pull_scheme_args=()
if [[ -n "$(mirror_for "$(mirror_ref_host "${released_chart_ref#oci://}")")" ]]; then
	helm_pull_ref="oci://$(mirror_rewrite "${released_chart_ref#oci://}")"
	helm_pull_scheme_args=(--plain-http)
	echo "     pulling the released chart via the registry mirror: ${helm_pull_ref}"
fi

step "helm pull ${helm_pull_ref} --version ${released_version}"
# The published artifact, not `git archive` of the tag: a packaging difference
# between the chart source and what operators can actually pull is exactly the
# kind of escape this check exists to catch. Retried: GHCR serves transient
# errors under load, and one blip must not redden the whole e2e run.
pull_ok=""
for attempt in 1 2 3 4 5; do
	if helm pull "${helm_pull_ref}" "${helm_pull_scheme_args[@]}" \
		--version "${released_version}" -d "${work_dir}" 2>&1; then
		pull_ok="yes"
		break
	fi
	echo "     helm pull attempt ${attempt} failed; retrying"
	sleep 5
done
if [[ -z "${pull_ok}" ]]; then
	die "could not pull ${helm_pull_ref} version ${released_version}. If the tag
       ${RELEASED_TAG} exists but its chart was never published, the release is broken for
       operators too — fix the publish (docs/operations/release.md) rather than this check.
       The chart package must also be public (or the environment logged into ghcr.io)."
fi
readonly released_tgz="${work_dir}/actions-gateway-${released_version}.tgz"
[[ -f "${released_tgz}" ]] || die "helm pull succeeded but ${released_tgz} does not exist."

step "capturing the installed release's values (replayed on the upgrade to HEAD)"
helm get values "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
	-o yaml >"${work_dir}/values.yaml"
# `helm get values` prints the literal "null" for a release installed with no
# user-supplied values; -f null is not a valid overrides file.
if [[ "$(tr -d '[:space:]' <"${work_dir}/values.yaml")" == "null" ]]; then
	echo "{}" >"${work_dir}/values.yaml"
fi

# The CRDs HEAD ships via the chart-root crds/ dir. Helm applies that dir on
# `helm install` only and never touches it again — deletes included — so the
# HEAD release installed at the start of the e2e run has left them behind. They
# must go before the released chart is installed, or this cluster would not
# look like one that has only ever run the released chart, and the preflight
# this check exercises (step 2 above) would never fire.
step "resetting chart-root crds/ state to the released chart's"
head_root_crds=()
for f in "${CHART_DIR}"/crds/*.yaml; do
	[[ -e "${f}" ]] || continue
	name="$(awk '/^metadata:$/ { m = 1; next } m && /^  name:/ { print $2; exit }' "${f}")"
	[[ -n "${name}" ]] || die "could not read metadata.name from ${f}; fix the manifest or this parser."
	head_root_crds+=("${name}")
done

step "helm uninstall ${HELM_RELEASE} (replacing the HEAD release with the released chart)"
helm uninstall "${HELM_RELEASE}" --kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" >/dev/null

for crd in ${head_root_crds[@]+"${head_root_crds[@]}"}; do
	echo "     deleting lingering chart-root CRD ${crd} (helm uninstall never removes crds/)"
	kc delete crd "${crd}" --ignore-not-found >/dev/null
done

# Pre-pull the released GMC image on the host and load it onto the kind nodes,
# so kubelet's pull from ghcr.io is a local hit — same de-flake pattern as the
# cert-manager/metrics-server pre-pulls in e2e-reusable.yml. Best-effort: on
# some hosts (macOS Docker Desktop) `kind load` cannot import a registry-pulled
# multi-arch image ("content digest ... not found"), and kubelet's own retried
# pull is a working fallback everywhere — the chart's imagePullPolicy is
# IfNotPresent, so a successful load is still a per-node cache hit.
# awk consumes the whole stream (no early exit): exiting at the first match
# closes the pipe while tar may still be writing, and the resulting EPIPE fails
# the pipeline under pipefail — a timing-dependent flake (Q548, 2026-07-31).
released_gmc_repo="$(tar -xzOf "${released_tgz}" actions-gateway/values.yaml | awk '
	/^gmc:$/ { in_gmc = 1; next }
	in_gmc && /^[^[:space:]]/ { in_gmc = 0 }
	in_gmc && /^  image:$/ { in_img = 1; next }
	in_img && !found && /^    repository:/ { found = 1; print $2 }
')"
if [[ -n "${released_gmc_repo}" ]] && command -v docker >/dev/null && command -v kind >/dev/null &&
	[[ "${KUBE_CONTEXT}" == "kind-${KIND_CLUSTER}" ]]; then
	step "pre-pulling ${released_gmc_repo}:${RELEASED_TAG} onto the kind nodes (best-effort)"
	if ! { "${REPO_ROOT}/scripts/fetch/pull-image-with-retry.sh" "${released_gmc_repo}:${RELEASED_TAG}" &&
		kind load docker-image --name "${KIND_CLUSTER}" "${released_gmc_repo}:${RELEASED_TAG}"; }; then
		echo "     (pre-pull/load failed; kubelet will pull the image itself, with its own backoff)"
	fi
fi

step "helm install ${HELM_RELEASE} <- the released chart ${RELEASED_TAG}"
# allowFloatingImageTags is the chart's documented dev/test opt-out of the
# digest-pin requirement; the release's image digests are not recorded anywhere
# this script can read without another registry round-trip, and the tag IS the
# release here. Everything else runs on the released chart's own defaults.
helm install "${HELM_RELEASE}" "${released_tgz}" \
	--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" --create-namespace \
	--set allowFloatingImageTags=true \
	--set gmc.image.tag="${RELEASED_TAG}" \
	--wait --timeout 10m >/dev/null
released_revision="$(release_revision)"
echo "OK: the released chart is installed and its manager is ready (revision ${released_revision})"

# --- 1. A removed values key must fail at render with the migration message ---
# Anchored on the guard still existing in HEAD's chart: when the removed key's
# render-time guard is eventually retired after its deprecation window, this
# probe retires with it instead of failing the gate.
if grep -rq "priorityClassAllowlist.configMapName" "${CHART_DIR}/templates"; then
	step "a release still setting the removed priorityClassAllowlist.configMapName must fail at render"
	probe_rc=0
	probe_out="$(helm upgrade "${HELM_RELEASE}" "${CHART_DIR}" \
		--kube-context "${KUBE_CONTEXT}" --namespace "${RELEASE_NS}" \
		-f "${work_dir}/values.yaml" \
		--set priorityClassAllowlist.configMapName=released-upgrade-probe \
		--wait --timeout 5m 2>&1)" || probe_rc=$?
	if ((probe_rc == 0)); then
		echo "FAIL: an upgrade carrying the removed priorityClassAllowlist.configMapName key was" >&2
		echo "  ACCEPTED. The render-time guard for it is gone, so an operator's existing value" >&2
		echo "  would silently narrow the PriorityClass security allowlist on upgrade (Q492)." >&2
		exit 1
	fi
	if [[ "${probe_out}" != *"configMapName"* || "${probe_out}" != *"docs/operations/upgrade.md"* ]]; then
		echo "FAIL: the upgrade with the removed configMapName key failed, but not with the" >&2
		echo "  migration message (it must name the key and point at docs/operations/upgrade.md):" >&2
		echo "  ${probe_out}" >&2
		exit 1
	fi
	if [[ "$(release_revision)" != "${released_revision}" ]]; then
		echo "FAIL: the rejected upgrade still advanced the release revision — the failure" >&2
		echo "  happened while applying, not at render, leaving the release half-upgraded." >&2
		exit 1
	fi
	echo "OK: the removed key failed at render with the migration message; the release is untouched"
else
	echo "     (skipping the removed-values probe: HEAD's chart no longer guards configMapName)"
fi

# --- 2. A plain upgrade must succeed, or fail with the documented preflight ---
step "helm upgrade ${HELM_RELEASE} -> HEAD's chart, with no pre-upgrade step"
upgrade_to_head
if ((UPGRADE_RC != 0)); then
	if [[ "$(release_revision)" != "${released_revision}" ]]; then
		echo "FAIL: the upgrade from ${RELEASED_TAG} to HEAD failed AFTER the release revision" >&2
		echo "  advanced — it died midway through applying, leaving an operator's cluster" >&2
		echo "  half-upgraded. Upgrade-blocking checks must fail at render. Helm said:" >&2
		echo "  ${UPGRADE_OUT}" >&2
		exit 1
	fi
	if [[ "${UPGRADE_OUT}" != *"pre-upgrade step"* && "${UPGRADE_OUT}" != *"docs/operations/upgrade.md"* ]]; then
		echo "FAIL: upgrading the released chart (${RELEASED_TAG}) to HEAD fails, and not with a" >&2
		echo "  documented pre-upgrade message. Every existing installation is broken the way" >&2
		echo "  Q492 broke v1.2.0: most likely a CRD referenced by the release moved into (or" >&2
		echo "  was added to) the chart-root crds/ dir — which Helm never applies on upgrade —" >&2
		echo "  without a render-time preflight naming the documented remedy. Either deliver" >&2
		echo "  the CRD via templates/crds/, or add a preflight fail that names the pre-upgrade" >&2
		echo "  step and points at docs/operations/upgrade.md (see" >&2
		echo "  charts/actions-gateway/templates/priorityclass-allowlist.yaml). Helm said:" >&2
		echo "  ${UPGRADE_OUT}" >&2
		exit 1
	fi
	echo "OK: the un-prepared upgrade failed at render with the documented preflight message"

	# --- 3. The documented step, verbatim, then the upgrade must succeed ---
	step "running the documented pre-upgrade step: helm show crds <chart> | kubectl apply -f -"
	crds_manifest="$(helm show crds "${CHART_DIR}")"
	if [[ -z "${crds_manifest}" ]]; then
		die "the preflight demanded the pre-upgrade CRD step, but HEAD's chart ships nothing in
       crds/ — the preflight and the chart layout disagree."
	fi
	kc apply -f - <<<"${crds_manifest}" >/dev/null

	step "helm upgrade ${HELM_RELEASE} -> HEAD's chart, after the documented step"
	upgrade_to_head
	if ((UPGRADE_RC != 0)); then
		echo "FAIL: the upgrade from ${RELEASED_TAG} still fails after the documented pre-upgrade" >&2
		echo "  step. The documented path does not work; Helm said:" >&2
		echo "  ${UPGRADE_OUT}" >&2
		exit 1
	fi
else
	echo "OK: the upgrade needed no pre-upgrade step"
fi

step "the release must now run HEAD's chart"
after_revision="$(release_revision)"
after_version="$(release_chart_version)"
if [[ "${after_revision}" -le "${released_revision}" || "${after_version}" == "${released_version}" ]]; then
	die "the release still reports chart version ${after_version} at revision ${after_revision};
       the upgrade to HEAD's chart did not actually land."
fi
echo "OK: revision ${released_revision} -> ${after_revision}, chart ${released_version} -> ${after_version}"

step "every CRD HEAD ships in crds/ must exist in the cluster"
# An upgrade can SUCCEED while never delivering a crds/ CRD — Helm skips the dir
# on upgrade, and if no rendered object references the kind, nothing fails. The
# operator only finds out when they first create the CR. The documented
# pre-upgrade step applies the whole crds/ dir, so after it every one must exist.
for crd in ${head_root_crds[@]+"${head_root_crds[@]}"}; do
	if ! kc get crd "${crd}" >/dev/null 2>&1; then
		echo "FAIL: ${crd} (from charts/actions-gateway/crds/) does not exist after the upgrade" >&2
		echo "  and its documented pre-upgrade step. Existing installations silently never get" >&2
		echo "  this CRD." >&2
		exit 1
	fi
	echo "     ${crd}: present"
done

# --- 4. A values-set upgrade must reach the schema preflight, then clear it ---
# Everything above replays the release's captured values, which for a stock
# install are the chart's defaults — so allowedInfraPriorityClasses is empty and
# the chart omits it from the rendered CR entirely. That is deliberate (it keeps
# the default upgrade safe against a stale CRD with no cluster read at all), and
# it is exactly why the preflight guarding the SET case had no coverage: Q298's
# lookup guard shipped verified by hand on kind, not by this gate.
#
# The negative half is doubly anchored — on HEAD's chart still carrying the
# guard, and on the cluster's stored CRD actually predating the field. The
# second anchor is not decoration: whether the stored CRD is current depends on
# which branch step 2 took (the documented step in step 3 applies crds/, a
# plain success does not), and on what the last released chart happened to ship.
# Asserting the failure unconditionally would redden the gate the day a release
# ships the field.
step "an upgrade that SETS the PriorityClass allowlists must reach the schema preflight"
before_revision="$(release_revision)"
if ! grep -q "allowedInfraPriorityClasses is set, but the PriorityClassAllowlist CRD" \
	"${CHART_DIR}/templates/priorityclass-allowlist.yaml"; then
	echo "     (skipping the negative half: HEAD's chart no longer guards the field's schema)"
elif [[ -n "$(infra_field_declared)" ]]; then
	echo "     (skipping the negative half: the stored CRD already declares"
	echo "      allowedInfraPriorityClasses, so there is no stale schema to refuse)"
else
	upgrade_with_lists --wait --timeout 5m
	if ((UPGRADE_RC == 0)); then
		echo "FAIL: an upgrade setting allowedInfraPriorityClasses was ACCEPTED while the" >&2
		echo "  cluster's PriorityClassAllowlist CRD still predates that field. Helm skips the" >&2
		echo "  chart-root crds/ dir on upgrade, so the field is not in the stored schema and" >&2
		echo "  server-side apply prunes it or fails MIDWAY — the half-applied shape this check" >&2
		echo "  exists to reject. The render-time guard in" >&2
		echo "  charts/actions-gateway/templates/priorityclass-allowlist.yaml is gone or no" >&2
		echo "  longer fires (Q298)." >&2
		exit 1
	fi
	if [[ "${UPGRADE_OUT}" != *"allowedInfraPriorityClasses"* ||
		"${UPGRADE_OUT}" != *"docs/operations/upgrade.md"* ]]; then
		echo "FAIL: the values-set upgrade failed, but not with the schema preflight message" >&2
		echo "  (it must name allowedInfraPriorityClasses and point at" >&2
		echo "  docs/operations/upgrade.md). Helm said:" >&2
		echo "  ${UPGRADE_OUT}" >&2
		exit 1
	fi
	if [[ "$(release_revision)" != "${before_revision}" ]]; then
		echo "FAIL: the refused values-set upgrade still advanced the release revision — it" >&2
		echo "  failed while applying, not at render, leaving the release half-upgraded." >&2
		exit 1
	fi
	echo "OK: the stale-schema upgrade failed at render with the documented message"

	step "running the documented pre-upgrade step, then retrying the values-set upgrade"
	kc apply -f - <<<"$(helm show crds "${CHART_DIR}")" >/dev/null
fi

# The positive half is unconditional: whichever way the negative half went, an
# upgrade with both lists set must succeed against a current CRD. It is the only
# place any tier renders allowedInfraPriorityClasses into the CR at all.
upgrade_with_lists --wait --timeout 10m
if ((UPGRADE_RC != 0)); then
	echo "FAIL: the values-set upgrade still fails with the CRD schema current. Setting the" >&2
	echo "  PriorityClass allowlists is a supported configuration, and after the documented" >&2
	echo "  pre-upgrade step nothing should refuse it. Helm said:" >&2
	echo "  ${UPGRADE_OUT}" >&2
	exit 1
fi

# The upgrade succeeding is not the same as the values landing: the chart omits
# allowedInfraPriorityClasses when empty, and a stale stored schema prunes it on
# write, so both failure modes look like a clean upgrade from the outside.
allowlist_json="$(kc get priorityclassallowlist \
	-l app.kubernetes.io/part-of=actions-gateway -o json)"
if [[ "$(jq -r '.items | length' <<<"${allowlist_json}")" != "1" ]]; then
	die "expected exactly one chart-owned PriorityClassAllowlist after the values-set upgrade,
       found: $(jq -rc '[.items[].metadata.name]' <<<"${allowlist_json}")"
fi
rendered="$(jq -c '.items[0].spec' <<<"${allowlist_json}")"
for want in "${PROBE_WORKER_CLASS}" "${PROBE_INFRA_CLASS}"; do
	if [[ "${rendered}" != *"${want}"* ]]; then
		echo "FAIL: the values-set upgrade succeeded but the PriorityClassAllowlist CR does not" >&2
		echo "  carry ${want}. Its spec is:" >&2
		echo "  ${rendered}" >&2
		echo "  A field the chart renders but the stored CRD does not declare is pruned on" >&2
		echo "  write with no error — the silent half of the Q298 hazard." >&2
		exit 1
	fi
done
echo "OK: both allowlists round-trip into the CR: ${rendered}"

step "admission must work on the upgraded release"
# A previous run's cleanup deletes the probe namespace with --wait=false, so a
# back-to-back invocation can find it Terminating — which rejects every create
# with "namespace is being terminated" and would read as an admission verdict.
for _ in $(seq 1 60); do
	ns_phase="$(kc get namespace "${PROBE_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
	[[ "${ns_phase}" == "Terminating" ]] || break
	sleep 2
done
if [[ "${ns_phase:-}" == "Terminating" ]]; then
	die "namespace ${PROBE_NS} is still Terminating; rerun once it is gone."
fi
kc create namespace "${PROBE_NS}" --dry-run=client -o yaml | kc apply -f - >/dev/null

assert_webhook_enforces
assert_params_resolve

echo
echo "OK: the last released chart (${RELEASED_TAG}) installed from its published artifact,"
echo "    upgraded to HEAD's chart along the documented path — with the PriorityClass"
echo "    allowlists both defaulted and SET — and came out with a healthy manager, working"
echo "    admission, and every chart-root CRD present."
