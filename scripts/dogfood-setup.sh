#!/usr/bin/env bash
# One-time bootstrap: create the dogfood GKE cluster (system + spot worker
# node pools), then install the v2 CRDs + GAG and provision the gag-dogfood
# tenant on the v2 API (namespace + GitHub App secret + ResourceQuota +
# ActionsGateway + RunnerTemplate + RunnerSet, direct egress).
# See docs/plan/gke-dogfood.md Parts A3–B8.
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
#   ZONE             GCP zone (e.g. us-central1-a)
#   REPO             GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#   APP_ID           GitHub App numeric ID (3752347)
#   INSTALLATION_ID  GitHub App installation ID for this repo/org
#
# Optional env vars:
#   ASSUME_YES=1     Skip the interactive "proceed?" confirmation (automation).
#   GAG_IMAGE_TAG    GAG control-plane image tag (default below). The publish
#                    pipeline only builds on v* release tags and never pushes
#                    `latest`, so a real released tag is required — bump this as
#                    new releases land. Tags: https://github.com/actions-gateway/github-actions-gateway/pkgs/container/gmc
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

# Default to the newest published release. `latest` does not exist in this
# project's registry (publish.yml builds only on v* tags), so floating to it
# yields ImagePullBackOff — pin a real tag instead.
GAG_IMAGE_TAG="${GAG_IMAGE_TAG:-v1.1.0-rc.5}"

# Optional build-capable worker image for the RunnerTemplate (Q239). When set,
# the runner container pins this image instead of staying image-less; the AGC
# then skips its DefaultWorkerImage gap-fill but still injects the Q235 wrapper.
# Build + push one with scripts/dogfood-runner-build.sh. Empty (the default)
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
	# No autoscaling on default-pool — it's manually scaled 0/1 to stop/start.
	gcloud container clusters create "${CLUSTER}" \
		--project="${PROJECT}" \
		--zone="${ZONE}" \
		--release-channel=regular \
		--enable-ip-alias \
		--enable-dataplane-v2 \
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
	echo "Creating spot worker node pool (autoscaling 0->4)..."
	# Taint keeps GMC/AGC/proxy off worker nodes; worker pods tolerate it.
	gcloud container node-pools create workers \
		--project="${PROJECT}" \
		--cluster="${CLUSTER}" \
		--zone="${ZONE}" \
		--machine-type=e2-standard-4 \
		--spot \
		--num-nodes=0 \
		--min-nodes=0 \
		--max-nodes=4 \
		--enable-autoscaling \
		--node-taints=dedicated=workers:NoSchedule \
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
	"${REPO_ROOT}/scripts/validate-cluster.sh"
}

# ---------------------------------------------------------------------------
# Parts B2–B3 (prereq) — the v2 CRDs ship in a separate, opt-in chart
# (actions-gateway-crds-v2) because bundling them would push the main chart's
# release Secret past its 1 MiB limit. This GMC build runs its v2 controllers
# unconditionally, so without the v2 CRDs it error-loops and the IP-range
# reconciler fails to list EgressProxies. Install them alongside the GMC.
#
# CRITICAL: install the CRDs from the SAME release as the GMC image
# (GAG_IMAGE_TAG), not the local worktree. The v2 alpha API schema drifts
# between releases (e.g. ActionsGateway spec.githubAppRef in v1.1.0-rc.2 became
# the spec.credentials discriminated union in v1.1.0-rc.3); a mismatch makes
# every reconcile fail validation ("unknown field" / "spec.X: Required value"),
# and a stale githubAppRef CRD silently drops the credential so the AGC
# crash-loops on a missing App key. git-archive pins the CRDs to the image's tag.
# ---------------------------------------------------------------------------

install_crds() {
	echo "Installing/upgrading v2 CRDs from ${GAG_IMAGE_TAG} (matching the GMC image)..."
	local crd_src
	crd_src="$(mktemp -d)"
	trap 'rm -rf "${crd_src:-}"' EXIT
	git -C "${REPO_ROOT}" archive "${GAG_IMAGE_TAG}" charts/actions-gateway-crds-v2 \
		| tar -x -C "${crd_src}"
	helm upgrade --install actions-gateway-crds-v2 \
		"${crd_src}/charts/actions-gateway-crds-v2" \
		--namespace gmc-system --create-namespace
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
	if ! kubectl rollout status deployment/gmc-controller-manager \
		-n gmc-system --timeout=5s >/dev/null 2>&1; then
		echo "GMC not ready — restarting to clear any pod-creation backoff..."
		kubectl rollout restart deployment/gmc-controller-manager -n gmc-system
	fi

	echo "Waiting for GMC to be ready..."
	kubectl rollout status deployment/gmc-controller-manager \
		-n gmc-system --timeout=3m

	rm -f "${values}"
	trap - EXIT
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
	echo "Applying Athens in-cluster Go module cache..."
	kubectl apply -k "${REPO_ROOT}/deploy/athens"
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
	# When DOGFOOD_RUNNER_IMAGE is set, pin it on the runner container; otherwise
	# leave the container image-less so the AGC gap-fills DefaultWorkerImage.
	local runner_image_field=""
	if [[ -n "${DOGFOOD_RUNNER_IMAGE}" ]]; then
		echo "  runner container pinned to ${DOGFOOD_RUNNER_IMAGE}"
		runner_image_field="          image: ${DOGFOOD_RUNNER_IMAGE}"
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
          # DOGFOOD_RUNNER_IMAGE (built by scripts/dogfood-runner-build.sh) to pin
          # a build-capable image above instead (Q239). Injection still applies.
          env:
            # Route Go module fetches through Athens (Q244). Workers cannot reach
            # proxy.golang.org directly (egress NetworkPolicy, GKE DPv2 no FQDN NP).
            # Athens fetches from upstream on first request and caches to PVC.
            # GONOSUMDB=* prevents direct sum.golang.org queries from workers;
            # Athens validates checksums when it fetches from proxy.golang.org.
            - name: GOPROXY
              value: "http://athens.gag-dogfood.svc.cluster.local:3000,off"
            - name: GONOSUMDB
              value: "*"
          resources:
            requests:
              cpu: "2"
              memory: "4Gi"
            limits:
              cpu: "4"
              memory: "8Gi"
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
  runnerLabels: ["self-hosted", "linux", "gag-ci"]
  maxListeners: 8
  maxWorkers: 4
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
	get_credentials

	# Part B — install GAG + provision the tenant.
	preflight
	install_crds
	install_gag
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
	echo "  2. Route CI to GAG:   scripts/dogfood-start.sh"
	echo "  3. Take it offline:   scripts/dogfood-stop.sh"
	echo "  4. One-time e2e pool: scripts/dogfood-e2e-setup.sh"
	echo ""
	echo "vendor-check and tidy-check are now routed to GAG runners. Athens"
	echo "pre-warms on first request — expect a slower first run per module."
}

main "$@"
