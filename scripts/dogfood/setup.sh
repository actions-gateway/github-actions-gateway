#!/usr/bin/env bash
# One-time bootstrap: create the dogfood GKE cluster (system + spot worker
# node pools), then install the v2 CRDs + GAG and provision the gag-dogfood
# tenant on the v2 API (namespace + GitHub App secret + ResourceQuota +
# ActionsGateway + RunnerTemplate + RunnerSet, direct egress).
# See the GKE dogfood plan (indexed in docs/plan/README.md), Parts A3–B8.
#
# Run after the account-level GCP setup (Parts A1–A2: gcloud installed and
# authenticated, project created, billing linked, container + compute APIs
# enabled). This script does NOT create the project or link billing — those
# are one-time, account-scoped, and awkward to script idempotently.
#
# Fully idempotent: every step is guarded or uses an apply/upsert, so it is
# safe to run with some of the work already done (e.g. cluster created but GAG
# not yet installed) and safe to re-run after a partial failure. The gcloud
# cluster/node-pool creates are skipped when the resource already exists;
# helm uses `upgrade --install`; kubectl objects are server-side upserted.
#
# Required env vars (export before running):
#   PROJECT          GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER          GKE cluster name (e.g. gag-dogfood)
#   ZONE             GCP zone (e.g. us-east1-b)
#   REPO             GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#   APP_ID           GitHub App numeric ID (3752347)
#   INSTALLATION_ID  GitHub App installation ID for this repo/org
#
# Optional env vars:
#   ASSUME_YES=1     Skip the interactive "proceed?" confirmation (automation).
#   GAG_IMAGE_TAG    Git ref (SHA, branch, or tag) identifying the GAG control-
#                    plane build to install (default below). It is used TWICE and
#                    must resolve BOTH ways: (a) as a published image tag under
#                    ghcr.io/actions-gateway/{gmc,agc,proxy,wrapper}:<ref>, and
#                    (b) as a git object git-archived for the matching v2 CRD chart
#                    (install_crds). The single ref keeps the image and the CRD
#                    chart on the SAME code, so the v2 alpha schema can never drift
#                    between them. Dogfood deliberately exercises PRE-RELEASE code,
#                    so this is NOT limited to cut `v*` releases: point it at any
#                    post-Q74 ref whose control-plane image has been built + pushed
#                    to GHCR. Build one for a ref with (amd64 GKE nodes):
#                      SHA=<ref>; GIT_SHA="$SHA" docker buildx bake gmc agc proxy wrapper \
#                        --set '*.platform=linux/amd64' \
#                        --set "gmc.tags=ghcr.io/actions-gateway/gmc:$SHA" \
#                        --set "agc.tags=ghcr.io/actions-gateway/agc:$SHA" \
#                        --set "proxy.tags=ghcr.io/actions-gateway/proxy:$SHA" \
#                        --set "wrapper.tags=ghcr.io/actions-gateway/wrapper:$SHA"
#                    See the GKE dogfood plan § "Tracking post-Q74 pre-release
#                    builds".
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/dogfood/lib/gmc.sh
source "${REPO_ROOT}/scripts/dogfood/lib/gmc.sh"

# Default: a post-Q74 main SHA whose control-plane image is published to GHCR and
# whose tree carries the v2beta1+v2alpha1 conversion-webhook CRD chart (Q281). This
# is a plain git ref, not a `v*` release: dogfood tracks pre-release code and must
# not wait for a cut release, and the publish pipeline builds images only on `v*`
# tags — so the image at this ref was built + pushed by hand (see the
# GAG_IMAGE_TAG note above). `latest` is never published, so never float to it.
#
# This default only ever moves forward, and re-running with it must never roll the
# cluster BACK. The script is advertised as safe to re-run, so a default older than
# what is deployed turns an innocuous re-run into a silent downgrade of the whole
# control plane — CRDs included. That is not hypothetical: the default sat at a
# 2026-07-07 ref while dogfood ran 2026-07-24 and then 2026-07-31 code, so a
# defaults run would have withdrawn the capacity-gate rung and re-blocked Q472.
# When you pin a newer ref by hand, update this line in the same change.
GAG_IMAGE_TAG="${GAG_IMAGE_TAG:-2715e7f87e48896b26aaa7c4bf4b8b48425576be}"

# Optional build-capable worker image for the RunnerTemplate (Q239). When set,
# the runner container pins this image instead of staying image-less; the AGC
# then skips its DefaultWorkerImage gap-fill but still injects the Q235 wrapper.
# Build + push one with scripts/dogfood/runner-build.sh. Empty (the default)
# keeps the bare upstream actions-runner, on which this repo's make-based CI
# fails make-command-not-found.
DOGFOOD_RUNNER_IMAGE="${DOGFOOD_RUNNER_IMAGE:-}"

# ---------------------------------------------------------------------------
# Existence guards — make the gcloud creates (which error if the resource
# already exists) idempotent by checking first.
# ---------------------------------------------------------------------------

cluster_exists() {
	gcloud container clusters describe "${CLUSTER}" \
		--project="${PROJECT}" --zone="${ZONE}" >/dev/null 2>&1
}

node_pool_exists() {
	local pool="$1"
	gcloud container node-pools describe "${pool}" \
		--project="${PROJECT}" --cluster="${CLUSTER}" --zone="${ZONE}" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# Part A3 — system node pool (the cluster's default-pool).
# ---------------------------------------------------------------------------

create_cluster() {
	if cluster_exists; then
		echo "Cluster ${CLUSTER} already exists — skipping create."
		return
	fi
	echo "Creating GKE cluster ${CLUSTER} (system node pool)..."
	# Standard zonal cluster — one free per billing account, no cluster fee.
	# --enable-dataplane-v2: Cilium CNI that enforces NetworkPolicy (GAG needs it).
	# --workload-pool: Workload Identity. Control-plane-only here (node pools opt
	# in per-pool via --workload-metadata), but it is a HARD PREREQUISITE of the
	# Part F e2e pool, whose --workload-metadata=GKE_METADATA is rejected with a
	# 400 without it (found live under Q286). Omitting it here is invisible until
	# e2e-setup.sh runs, and can then only be repaired by a separate control-plane
	# update — so create the cluster right the first time. Q380: this flag was
	# missing from the script while the runbook's Part A3 documented it, because
	# every previous run took the "already exists" branch against a cluster that
	# had had Workload Identity retrofitted by hand.
	# No autoscaling on default-pool — it's manually scaled 0/1 to stop/start.
	gcloud container clusters create "${CLUSTER}" \
		--project="${PROJECT}" \
		--zone="${ZONE}" \
		--release-channel=regular \
		--enable-ip-alias \
		--enable-dataplane-v2 \
		--workload-pool="${PROJECT}.svc.id.goog" \
		--machine-type=e2-standard-2 \
		--num-nodes=1 \
		--disk-size=50GB \
		--no-enable-basic-auth \
		--no-issue-client-certificate
}

# ---------------------------------------------------------------------------
# Part A4 — spot worker node pool, tainted so only worker pods land on it.
# ---------------------------------------------------------------------------

create_worker_pool() {
	if node_pool_exists workers; then
		echo "Node pool 'workers' already exists — skipping create."
		return
	fi
	echo "Creating spot worker node pool (autoscaling 0->8)..."
	# Taint keeps GMC/AGC/proxy off worker nodes; worker pods tolerate it.
	# disk-type=pd-standard (HDD), NOT the GKE default pd-balanced (SSD-class):
	# pd-balanced counts against the 500 GB regional SSD_TOTAL_GB quota, so a
	# 100 GB balanced boot disk per worker capped the pool at ~4 nodes (Q248) —
	# a self-inflicted ceiling, not a real quota shortage. pd-standard counts
	# against DISKS_TOTAL_GB (4096 GB) instead, so worker capacity becomes
	# CPU/mem-bound (200 CPU quota), not SSD-bound. The CI job classes are Go
	# build/test/lint/envtest — CPU/mem-bound, not scratch-IOPS-bound — so HDD is
	# fine; the 100 GB size is kept for container-image pull scratch. This lifts
	# max-nodes 4->8 within the existing quota (no quota bump). See the
	# dogfood-runner-rightsizing plan (indexed in docs/plan/README.md).
	gcloud container node-pools create workers \
		--project="${PROJECT}" \
		--cluster="${CLUSTER}" \
		--zone="${ZONE}" \
		--machine-type=e2-standard-4 \
		--spot \
		--num-nodes=0 \
		--min-nodes=0 \
		--max-nodes=8 \
		--enable-autoscaling \
		--node-taints=dedicated=workers:NoSchedule \
		--disk-type=pd-standard \
		--disk-size=100GB
}

# ---------------------------------------------------------------------------
# Part A4b — non-preemptible worker pool for benchmark runs.
# ---------------------------------------------------------------------------

create_worker_od_pool() {
	if node_pool_exists workers-od; then
		echo "Node pool 'workers-od' already exists — skipping create."
		return
	fi
	echo "Creating on-demand worker node pool (fixed size, starts at 0)..."
	# The `workers` pool above is spot, which is the right default for routine CI:
	# cheap, and a preemption just re-runs a job. It is the WRONG shape for a
	# benchmark. Q260 chased a job-starvation signal that turned out to be spot
	# preemption mid-burst (nodes dropping 3->1) rather than anything in GAG; the
	# run only became readable once it was pinned to a non-preemptible pool, where
	# all nodes stayed Ready across 58 monitor samples and the phantom starvation
	# did not recur. Q264's protocol benchmarks used the same pool for the same
	# reason. See the gke-dogfood-turnup-findings and
	# q260-fanout-completion-reconciliation plans (indexed in
	# docs/plan/README.md).
	#
	# Deliberately NOT autoscaled: a benchmark wants a fixed, known node count so
	# the capacity under test is a constant, not something the autoscaler moves
	# mid-run. Size it per run with `ops.sh pool-scale workers-od <n>` and return
	# it to 0 afterwards — at 0 nodes it costs nothing, so it is safe to leave in
	# place between campaigns.
	#
	# pd-standard for the same quota reason as `workers` (Q248): pd-balanced boot
	# disks count against the 500 GB regional SSD quota and capped this pool at ~4
	# nodes. Same taint as `workers` so the identical worker-pod toleration
	# schedules onto either pool.
	gcloud container node-pools create workers-od \
		--project="${PROJECT}" \
		--cluster="${CLUSTER}" \
		--zone="${ZONE}" \
		--machine-type=e2-standard-4 \
		--num-nodes=0 \
		--node-taints=dedicated=workers:NoSchedule \
		--disk-type=pd-standard \
		--disk-size=100GB
}

# ---------------------------------------------------------------------------
# Part A5 — fetch kubeconfig credentials and assert the active kubectl context
# is the cluster we targeted (shared helper). Every kubectl/helm step below
# runs against the current context, so this fails closed before any
# install/secret can land on the wrong cluster.
# ---------------------------------------------------------------------------

get_credentials() {
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"
}

# ---------------------------------------------------------------------------
# Part B1 — preflight the live cluster (CNI enforcement, K8s version,
# metrics-server). Read-only; fails closed before we install onto a cluster
# that can't satisfy GAG's requirements.
# ---------------------------------------------------------------------------

preflight() {
	echo "Running cluster preflight (validate-cluster)..."
	"${REPO_ROOT}/scripts/e2e/validate-cluster.sh"
}

# ---------------------------------------------------------------------------
# Parts B2–B3 (prereq) — the v2 CRDs ship in a separate, opt-in chart
# (actions-gateway-crds-v2) because bundling them would push the main chart's
# release Secret past its 1 MiB limit. This GMC build runs its v2 controllers
# unconditionally, so without the v2 CRDs it error-loops and the IP-range
# reconciler fails to list EgressProxies. Install them alongside the GMC.
#
# CRITICAL: install the CRDs from the SAME ref as the GMC image (GAG_IMAGE_TAG),
# not the local worktree. The v2 alpha API schema drifts between refs (e.g.
# ActionsGateway spec.githubAppRef in v1.1.0-rc.2 became the spec.credentials
# discriminated union in v1.1.0-rc.3); a mismatch makes
# every reconcile fail validation ("unknown field" / "spec.X: Required value"),
# and a stale githubAppRef CRD silently drops the credential so the AGC
# crash-loops on a missing App key. git-archive pins the CRDs to the image's tag.
# ---------------------------------------------------------------------------

install_crds() {
	echo "Rendering + applying v2 CRDs from ${GAG_IMAGE_TAG} (matching the GMC image)..."
	local crd_src
	crd_src="$(mktemp -d)"
	trap 'rm -rf "${crd_src:-}"' EXIT
	git -C "${REPO_ROOT}" archive "${GAG_IMAGE_TAG}" charts/actions-gateway-crds-v2 \
		| tar -x -C "${crd_src}"
	# apply-render, NOT `helm upgrade --install` (Q276/Q277). Since Q74 graduated
	# each v2 CRD to two served versions (v2beta1 + v2alpha1), the rendered chart
	# exceeds Helm's 1 MiB release-Secret limit, so Helm can no longer store the
	# release and `helm install`/`upgrade` fails outright. Render the chart and
	# apply it server-side instead — the supported, deliberate install≡upgrade
	# path (also clears kubectl's 256 KB client-side-apply annotation ceiling that
	# each ~1.16 MB RunnerTemplate CRD blows on its own). Dogfood deliberately uses
	# the from-source render (not the signed release asset): it installs a locally
	# git-archived chart at an arbitrary build tag to exercise pre-release code, so
	# it cannot depend on the release-only `v*` asset. --namespace gmc-system
	# resolves each CRD's conversion-webhook clientConfig to the GMC's
	# webhook-service (identical wiring to what the old helm install baked in via
	# the chart's default values). --force-conflicts takes field ownership from any
	# pre-Q277 helm-installed release of this chart so re-runs stay idempotent.
	helm template actions-gateway-crds-v2 \
		"${crd_src}/charts/actions-gateway-crds-v2" \
		--namespace gmc-system \
		| kubectl apply --server-side --force-conflicts -f -
	rm -rf "${crd_src}"
	trap - EXIT
}

# ---------------------------------------------------------------------------
# Parts B2–B3 — install/upgrade the GAG chart. `upgrade --install` is the
# idempotent form of `helm install`.
# ---------------------------------------------------------------------------

install_gag() {
	local values
	values="$(mktemp)"
	# Use :- so the trap is safe under `set -u` if it fires after the local
	# goes out of scope (e.g. a set -e abort later in the function).
	trap 'rm -f "${values:-}"' EXIT

	# Dogfood/dev mode: pin a released image tag (production pins digests).
	# Self-signed webhook cert (no cert-manager). Keep GMC on default-pool so it
	# goes down when that pool scales to 0; AGC/proxy inherit scheduling via the
	# worker pool's taint. Heredoc is unquoted so ${GAG_IMAGE_TAG} expands.
	cat >"${values}" <<EOF
allowFloatingImageTags: true
# Single GMC replica for dogfood — frees capacity on the small system node for
# the per-tenant AGC pod (production wants the default 2 for HA).
replicaCount: 1
gmc:
  image:
    tag: ${GAG_IMAGE_TAG}
agc:
  image:
    tag: ${GAG_IMAGE_TAG}
proxy:
  image:
    tag: ${GAG_IMAGE_TAG}
# The GMC forwards WRAPPER_IMAGE to every AGC, which injects the wrapper into
# each worker pod so the runner container can be the unmodified upstream
# actions-runner (Q235 injection default). Pin it to the release tag: the chart
# default tag is empty, which renders ghcr.io/.../wrapper:latest — a tag this
# registry never publishes — so injection would ImagePullBackOff without this.
wrapper:
  image:
    tag: ${GAG_IMAGE_TAG}
certManager:
  enabled: false
nodeSelector:
  cloud.google.com/gke-nodepool: default-pool
# No PodDisruptionBudget for dogfood: with a single GMC replica the chart's
# minAvailable: 1 permits zero voluntary disruptions, so the scale-to-0 stop
# (gcloud ... resize --num-nodes=0) can never drain the system node — it lingers
# Ready,SchedulingDisabled and keeps billing (Q236).
podDisruptionBudget:
  enabled: false
EOF

	echo "Installing/upgrading GAG chart..."
	helm upgrade --install gag "${REPO_ROOT}/charts/actions-gateway" \
		--namespace gmc-system --create-namespace \
		--values "${values}"

	# The GMC pod uses priorityClassName: system-cluster-critical (chart default,
	# protects it from eviction). GKE — and any cluster with the restricted
	# PriorityClass admission — only permits that class in a namespace that has a
	# ResourceQuota scoped to it; without one the GMC ReplicaSet fails pod
	# creation ("insufficient quota to match these scopes: [PriorityClass In
	# ...]"). Create the permitting quota before waiting for the rollout.
	echo "Permitting system-critical PriorityClass in gmc-system..."
	kubectl apply -f - <<'QUOTA'
apiVersion: v1
kind: ResourceQuota
metadata:
  name: gmc-system-critical-pods
  namespace: gmc-system
spec:
  hard:
    pods: "10"
  scopeSelector:
    matchExpressions:
      - operator: In
        scopeName: PriorityClass
        values: ["system-node-critical", "system-cluster-critical"]
QUOTA

	# If the GMC isn't Available yet (e.g. a prior run left the ReplicaSet in
	# pod-creation backoff from before the quota existed), restart so it retries
	# immediately instead of waiting out the exponential backoff. Skip when it's
	# already healthy so a re-run doesn't needlessly bounce the control plane.
	if ! gmc_ready; then
		echo "GMC not ready — restarting to clear any pod-creation backoff..."
		kubectl rollout restart "${GMC_DEPLOYMENT}" --namespace "${GMC_NAMESPACE}"
	fi

	wait_for_gmc 3m

	rm -f "${values}"
	trap - EXIT
}

# ---------------------------------------------------------------------------
# Part B3b — wire the GMC's self-signed webhook CA into each v2 CRD's
# spec.conversion.webhook.clientConfig.caBundle (Q279). Since Q74 the v2 kinds
# are stored at v2beta1 and served at v2alpha1 via the GMC-hosted conversion
# webhook; the apiserver calls that webhook over TLS and must trust its serving
# cert via each CRD's caBundle. Dogfood is self-signed (no cert-manager), so the
# CRD chart renders an EMPTY caBundle — without this step every CR apply (and
# conversion read-back) fails the webhook TLS handshake ("x509: certificate
# signed by unknown authority").
#
# Ordering: install_crds applies the CRDs earlier with an empty caBundle (CRD
# registration itself does not need it), because the CA is only mintable AFTER
# install_gag creates the webhook-server-cert Secret — and the GMC must see the
# v2 CRDs at startup to enable its v2 controllers + conversion webhook (Q228
# detection), so the CRDs-before-GMC order must NOT be reversed. We therefore
# patch the caBundle in here, after the GMC is up and before apply_cr, so the
# first CR already round-trips through a TLS-verified conversion webhook.
#
# Secure by default: this RESTORES webhook TLS verification. Never fall back to a
# caBundle-less clientConfig or insecureSkipTLSVerify shortcut.
# ---------------------------------------------------------------------------

patch_crd_cabundle() {
	echo "Wiring GMC webhook CA into the v2 CRD conversion caBundle..."
	# The chart signs the GMC webhook serving cert with a self-signed CA it mints
	# once and stores in webhook-server-cert (reused across renders via `lookup`,
	# so this value is stable on re-runs). The Secret's data["ca.crt"] is already
	# base64(PEM) — exactly the encoding a CRD caBundle wants — so pass it straight
	# through with no decode/re-encode. Read lazily on first need so a pre-Q74
	# image (below) never touches the Secret.
	local ca_bundle=""
	local crd strategy patched=0
	for crd in actionsgateways runnersets runnertemplates egressproxies clusterrunnertemplates; do
		# Only CRDs whose conversion strategy is Webhook have a clientConfig to wire.
		# install_crds pins the CRDs to GAG_IMAGE_TAG; a pre-Q74 image ships
		# single-version CRDs (strategy None, no conversion block), where patching a
		# webhook clientConfig would be rejected ("should not be set when strategy is
		# not Webhook"). Skip those cleanly so setup stays valid across the dogfood
		# image-tag transition — the caBundle only matters once GAG_IMAGE_TAG is a
		# post-Q74 release whose CRDs actually route CR conversion through /convert.
		strategy="$(kubectl get crd "${crd}.actions-gateway.com" \
			-o jsonpath='{.spec.conversion.strategy}' 2>/dev/null || true)"
		if [[ "${strategy}" != "Webhook" ]]; then
			echo "  ${crd}: conversion strategy '${strategy:-None}' (not Webhook) — no caBundle to wire, skipping."
			continue
		fi

		if [[ -z "${ca_bundle}" ]]; then
			ca_bundle="$(kubectl get secret webhook-server-cert -n gmc-system \
				-o jsonpath='{.data.ca\.crt}')"
			if [[ -z "${ca_bundle}" ]]; then
				echo "webhook-server-cert Secret has no ca.crt — cannot wire the CRD caBundle." >&2
				exit 1
			fi
		fi

		# A JSON merge patch sets the caBundle leaf and leaves the chart-rendered
		# clientConfig.service block (name/namespace/path/port) untouched. install_crds
		# never renders caBundle, so a later server-side re-apply of the chart cannot
		# strip this leaf (a different field manager owns it).
		kubectl patch crd "${crd}.actions-gateway.com" --type=merge \
			-p "{\"spec\":{\"conversion\":{\"webhook\":{\"clientConfig\":{\"caBundle\":\"${ca_bundle}\"}}}}}"
		patched=$((patched + 1))
	done
	echo "Wired the GMC webhook CA into ${patched} v2 CRD conversion caBundle(s)."
}

# ---------------------------------------------------------------------------
# Part B4 — tenant namespace with the required GAG label + baseline PSA.
# ---------------------------------------------------------------------------

create_namespace() {
	echo "Creating gag-dogfood tenant namespace..."
	kubectl create namespace gag-dogfood --dry-run=client -o yaml \
		| kubectl apply -f -
	# v2 markers: tenant=managed authorizes the GMC to operate in the namespace;
	# security-profile drives the Pod Security level the GMC stamps. (v1 used
	# actions-gateway.github.com/tenant=true + an inline spec.securityProfile.)
	kubectl label namespace gag-dogfood \
		actions-gateway.com/tenant=managed \
		actions-gateway.com/security-profile=baseline \
		pod-security.kubernetes.io/enforce=baseline \
		--overwrite
}

# ---------------------------------------------------------------------------
# Part B5 — GitHub App secret. The private key lives in the macOS keychain as
# hex; write it to a temp file, create the secret, delete the file. Never
# passes the key through an env var or argv.
# ---------------------------------------------------------------------------

create_secret() {
	local pem_file
	pem_file="$(mktemp)"
	trap 'rm -f "${pem_file:-}"' EXIT

	echo "Retrieving GitHub App private key from keychain..."
	security find-generic-password \
		-a actions-gateway-test -s github-app-private-key -w \
		| xxd -r -p >"${pem_file}"

	# Fail loudly rather than create a Secret with an empty/garbage key, which
	# would surface later as opaque GAG auth failures.
	if [[ ! -s "${pem_file}" ]]; then
		echo "GitHub App private key from keychain is empty — aborting." >&2
		exit 1
	fi

	echo "Creating GitHub App secret in gag-dogfood..."
	kubectl create secret generic github-app-v1 \
		--namespace=gag-dogfood \
		--from-literal=appId="${APP_ID}" \
		--from-literal=installationId="${INSTALLATION_ID}" \
		--from-file=privateKey="${pem_file}" \
		--dry-run=client -o yaml \
		| kubectl apply -f -

	rm -f "${pem_file}"
	trap - EXIT
}

# ---------------------------------------------------------------------------
# Part B6 — namespace ResourceQuota.
# ---------------------------------------------------------------------------

apply_quota() {
	echo "Applying ResourceQuota..."
	kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ResourceQuota
metadata:
  name: dogfood-quota
  namespace: gag-dogfood
spec:
  hard:
    pods: "12"
EOF
}

# ---------------------------------------------------------------------------
# Part B6b — Athens in-cluster Go module proxy (Q244). Athens caches Go
# modules so vendor-check/tidy-check can run on GAG runners without external
# egress to proxy.golang.org. The Athens pod (app=athens) is not labelled
# actions-gateway/component=workload, so it is not covered by the workload
# NetworkPolicy and retains free egress to fetch modules. Worker pods reach
# Athens via an additive NetworkPolicy in deploy/athens/networkpolicy.yaml
# that opens port 3000 from workload pods to Athens pods. Workers are wired
# via GOPROXY/GONOSUMDB env vars in the RunnerTemplate (Part B7 below).
# ---------------------------------------------------------------------------

apply_athens() {
	# Ephemeral cache by default (emptyDir — $0 at rest, but a cold cache on the
	# first vendor-check/tidy-check after each scale-to-zero wake). Set
	# ATHENS_PERSISTENT=1 to render the PVC-backed overlay, which keeps the Go
	# module cache warm across idle cycles at the cost of a continuously-billed
	# disk. See deploy/athens/README.md.
	local overlay="${REPO_ROOT}/deploy/athens"
	if [[ "${ATHENS_PERSISTENT:-0}" == "1" || "${ATHENS_PERSISTENT:-0}" == "true" ]]; then
		overlay="${REPO_ROOT}/deploy/athens/overlays/persistent"
		echo "Applying Athens in-cluster Go module cache (persistent PVC)..."
	else
		echo "Applying Athens in-cluster Go module cache (ephemeral)..."
	fi
	kubectl apply -k "${overlay}"
	echo "  Waiting for Athens to be ready..."
	kubectl rollout status deployment/athens -n gag-dogfood --timeout=120s
}

# Part B7 — the v2 tenant objects. The v2 API decomposes the v1 monolithic
# ActionsGateway into ActionsGateway (gateway + credentials) + RunnerTemplate
# (worker pod shape) + RunnerSet (runner group). Minimal direct-egress form:
# no EgressProxy, so workers egress directly to GitHub — still behind the
# default-deny egress NetworkPolicy (DNS + GitHub CIDR), just without a stable
# per-tenant egress IP. Attach an EgressProxy + spec.defaultProxyRef later to
# add per-tenant IP attribution.
# ---------------------------------------------------------------------------

apply_cr() {
	echo "Applying v2 ActionsGateway + RunnerTemplate + RunnerSet..."
	# Resolve the runner container image (Q239/Q295). Precedence:
	#   1. DOGFOOD_RUNNER_IMAGE set          -> pin it (build-capable image).
	#   2. env unset + existing runner image -> PRESERVE the cluster's current
	#      image, so an idempotent re-run without the env can't silently reset a
	#      toolchain-pinned worker back to the image-less upstream default and make
	#      `make`/Go vanish (the Q295 footgun that cost a full validation cycle).
	#   3. env unset + no existing image     -> stay image-less; the AGC gap-fills
	#      DefaultWorkerImage + injects the Q235 wrapper (today's default).
	local runner_image="${DOGFOOD_RUNNER_IMAGE}"
	if [[ -z "${runner_image}" ]]; then
		# Read back the runner container image from the live RunnerTemplate. Pin the
		# context explicitly (--context) rather than trusting the active one — a
		# parallel session sharing ~/.kube/config could have repointed it since
		# get_credentials ran. This is the same gke_<project>_<zone>_<cluster> name
		# gke_get_credentials_and_verify asserts. Absent RunnerTemplate (first run)
		# -> empty -> falls through to the image-less default.
		local kube_context existing_image
		kube_context="gke_${PROJECT}_${ZONE}_${CLUSTER}"
		existing_image="$(kubectl --context "${kube_context}" \
			get runnertemplate.actions-gateway.com default -n gag-dogfood \
			-o jsonpath='{.spec.podTemplate.spec.containers[?(@.name=="runner")].image}' \
			2>/dev/null || true)"
		if [[ -n "${existing_image}" ]]; then
			echo "  DOGFOOD_RUNNER_IMAGE unset — preserving existing runner image ${existing_image}"
			runner_image="${existing_image}"
		fi
	fi

	# A resolved image (env or preserved) is pinned on the runner container;
	# otherwise the container stays image-less so the AGC gap-fills DefaultWorkerImage.
	local runner_image_field=""
	if [[ -n "${runner_image}" ]]; then
		echo "  runner container pinned to ${runner_image}"
		runner_image_field="          image: ${runner_image}"
	fi
	kubectl apply -f - <<EOF
apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: dogfood
  namespace: gag-dogfood
spec:
  credentials:
    type: GitHubApp
    githubApp:
      name: github-app-v1
  githubURL: https://github.com/${REPO}
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerTemplate
metadata:
  name: default
  namespace: gag-dogfood
spec:
  podTemplate:
    spec:
      tolerations:
        - key: dedicated
          value: workers
          effect: NoSchedule
      containers:
        - name: runner
${runner_image_field}
          # The runner container is image-less by default: that exercises the
          # Q235 injection default, where the AGC gap-fills the resolved worker
          # image on a named image-less container (Q233) — the built-in upstream
          # actions-runner digest (DefaultWorkerImage) — and injects the GAG
          # worker wrapper (WRAPPER_IMAGE) so that unmodified upstream image runs
          # jobs. The bare upstream image has no build toolchain, so this repo's
          # own make-based CI fails make-command-not-found on it; export
          # DOGFOOD_RUNNER_IMAGE (built by scripts/dogfood/runner-build.sh) to pin
          # a build-capable image above instead (Q239). Injection still applies.
          env:
            # Route Go module fetches through Athens (Q244). Workers cannot reach
            # proxy.golang.org directly (egress NetworkPolicy, GKE DPv2 no FQDN NP).
            # Athens fetches from upstream on first request and caches to PVC.
            # GONOSUMDB=* prevents direct sum.golang.org queries from workers;
            # Athens validates checksums when it fetches from proxy.golang.org.
            - name: GOPROXY
              value: "http://go-module-proxy.gag-dogfood.svc.cluster.local:3000,off"
            - name: GONOSUMDB
              value: "*"
          # Right-sized from measured peak (Q248 Phase 1: heavy CI jobs — -race,
          # lint, envtest — peaked ~3.8 vCPU / ~2.1Gi on gag-ci). CPU is
          # compressible: requests-only, NO limit — a CPU limit only throttles
          # bursty Go build/test jobs for no packing gain, while the request still
          # drives packing. request=2 on an e2-standard-4 worker (~3.4 vCPU
          # node-allocatable after GKE system daemonsets) packs exactly ONE heavy
          # pod per node, which the ~3.8 vCPU peak wants — it bursts to the whole
          # node rather than throttling two co-scheduled heavy jobs to ~1.7 vCPU
          # each. Memory is non-compressible → keep a limit: request≈peak (2Gi),
          # limit=peak×~1.4 (3Gi) for OOM headroom (was a 2×/4×-over-provisioned
          # 4Gi/8Gi guess). maxWorkers=8 → ≤8 worker nodes, matching the pool's
          # max-nodes=8. See the dogfood-runner-rightsizing plan.
          resources:
            requests:
              cpu: "2"
              memory: "2Gi"
            limits:
              memory: "3Gi"
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: ci
  namespace: gag-dogfood
spec:
  gatewayRef:
    name: dogfood
  templateRef:
    name: default
  # ScaleSet (the Q264 P5 default), set explicitly rather than by omission so the
  # protocol this tenant runs is readable here. The single runnerLabel is BOTH the
  # scale set's name at GitHub and the workflows' runs-on target, so it must match
  # start.sh's GAG_RUNNER exactly.
  #
  # Migrated off Classic by Q399. Classic acquires a job (AcquireJob flips it to
  # in_progress at GitHub and stamps the runner name) and only then decides whether
  # to provision a worker; every job it declines to provision is orphaned at GitHub
  # with zero steps until the 10-minute lock-lapse / 15-minute unstarted-job timeout
  # kills it. Measured on this tenant 2026-07-25: 85 jobs acquired, 16 worker pods,
  # 69 orphaned (81%). ScaleSet's single-acquirer listener cannot produce that shape
  # (Q264 P4 measured 7/7 vs Classic's 2/7). acquisitionProtocol is IMMUTABLE, so an
  # existing Classic set must be deleted and recreated; see the migration note
  # in the GKE dogfood plan, B7.
  acquisitionProtocol: ScaleSet
  runnerLabels: ["gag-ci-scaleset"]
  # maxWorkers 8: the pd-standard disk right-size (Q248) lifted the worker-node
  # ceiling off the SSD quota, so the ~7-job dogfood CI matrix fits. On the ScaleSet
  # path this is also the capacity advertised to GitHub (X-ScaleSetMaxCapacity), so
  # GitHub never assigns more jobs than the tenant can place. maxListeners is
  # deliberately absent: it is a Classic-only knob (one listener goroutine per
  # runner record) that the ScaleSet path ignores: a scale set has ONE listener.
  maxWorkers: 8
  # Throughput sizing (Q359 Phase 3). This tenant is the ONLY place the profile
  # can be validated live: it needs >=20 sampled jobs per template container
  # (usage.MinSamplesForDrift) before it actuates, and the ~7-job e2e matrix in
  # the release gate cannot reach that in one run. The always-on CI tenant
  # accumulates samples organically — it reached 36 on 2026-07-25.
  #
  # That history does NOT depend on this stanza and does not have to be earned
  # ahead of an RC: the sampler tracks every worker pod regardless of spec.sizing
  # and the aggregate re-seeds from the persisted status.sizingRecommendation, so
  # samples accrue from ordinary CI traffic and survive stop/start. What DOES
  # gate the RC is deployment — a CR edit here reaches the cluster only when
  # setup.sh runs (or via a direct patch); start.sh never applies CRs, so
  # editing this and starting the tenant leaves the profile inert and the
  # release gate reporting an empty sizingProfileState.
  #
  # Throughput rather than Binpack because it is what this template already
  # encodes by hand: requests-only CPU (a limit just throttles bursty Go
  # build/test jobs) with a memory limit above the request for OOM headroom.
  # Binpack is already live-validated on this tenant (2026-07-25); Throughput
  # is not, and both are v1.3 headline features.
  #
  # The clamps are the safety rails, not decoration: this tenant runs the
  # project's real CI, so a derivation from noisy early samples must not be
  # able to starve a heavy job or ask for a pod the node cannot hold.
  #   minRequests.cpu 1    — a heavy -race/lint/envtest job never drops below
  #                          one core's guarantee, whatever the p95 says.
  #   maxRequests.cpu 3    — an e2-standard-4 worker node has ~3.4 vCPU
  #                          allocatable; 3 still schedules, 4 would not.
  #   minRequests.memory   — 1Gi floor under the measured ~2.1Gi peak.
  # Reverting is a one-line delete here plus a re-apply, or an immediate
  # kubectl patch of runnersets.v2alpha1.actions-gateway.com/ci in gag-dogfood
  # removing /spec/sizing (--type=json, op remove).
  sizing:
    profile: Throughput
    minRequests:
      cpu: "1"
      memory: "1Gi"
    maxRequests:
      cpu: "3"
EOF
}

# Show the resolved target and require explicit confirmation before any billable
# create or cluster write (shared helper; ASSUME_YES=1 bypasses it).
confirm_target() {
	confirm_or_exit "$(printf 'About to bootstrap the dogfood environment:\n  Project: %s\n  Cluster: %s  (zone %s)\n  Repo:    %s\nThis creates/updates billable GKE resources and installs GAG into the cluster.' \
		"${PROJECT}" "${CLUSTER}" "${ZONE}" "${REPO}")"
}

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"
	: "${APP_ID:?APP_ID must be set}"
	: "${INSTALLATION_ID:?INSTALLATION_ID must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	# GKE kubeconfigs authenticate via this external plugin; without it every
	# kubectl call fails. Check up front so a first run fails before creating
	# any billable resources rather than after (install: gcloud components
	# install gke-gcloud-auth-plugin).
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"
	require_cmd helm "https://helm.sh/docs/intro/install/"
	require_cmd security "built-in macOS tool — macOS required to read keychain"
	require_cmd xxd "built-in macOS/Linux tool"

	confirm_target

	# Part A — cluster + node pools + credentials.
	create_cluster
	create_worker_pool
	create_worker_od_pool
	get_credentials

	# Part B — install GAG + provision the tenant.
	preflight
	install_crds
	install_gag
	patch_crd_cabundle
	create_namespace
	create_secret
	apply_quota
	apply_athens
	apply_cr

	echo ""
	echo "Bootstrap complete. GAG is installed and the gag-dogfood tenant is up."
	echo ""
	echo "Verify the gateway and that runners registered (~2 min after AGC Ready):"
	echo "  kubectl get actionsgateway,runnerset -n gag-dogfood"
	echo "  kubectl get pods -n gag-dogfood"
	echo "  gh api /repos/${REPO}/actions/runners \\"
	echo "    --jq '.runners[] | {name, status, labels: [.labels[].name]}'"
	echo ""
	echo "Next steps:"
	echo "  1. Land the Part C2 workflow changes (runs-on -> vars.GAG_RUNNER)."
	echo "  2. Route CI to GAG:   scripts/dogfood/start.sh"
	echo "  3. Take it offline:   scripts/dogfood/stop.sh"
	echo "  4. One-time e2e pool: scripts/dogfood/e2e-setup.sh"
	echo ""
	echo "vendor-check and tidy-check are now routed to GAG runners. Athens"
	echo "pre-warms on first request — expect a slower first run per module."
}

[[ -n "${SETUP_LIB_ONLY:-}" ]] || main "$@"
