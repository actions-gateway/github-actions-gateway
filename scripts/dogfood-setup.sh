#!/usr/bin/env bash
# One-time bootstrap: create the dogfood GKE cluster (system + spot worker
# node pools), then install GAG and provision the gag-dogfood tenant
# (namespace + GitHub App secret + ResourceQuota + ActionsGateway CR).
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
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

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
# Part A5 — fetch kubeconfig credentials (idempotent; rewrites the context),
# then assert the active kubectl context is the cluster we just targeted.
# Every kubectl/helm step below runs against the current context, so this one
# check fails closed before any install/secret can land on the wrong cluster.
# ---------------------------------------------------------------------------

get_credentials() {
	echo "Fetching cluster credentials..."
	gcloud container clusters get-credentials "${CLUSTER}" \
		--project="${PROJECT}" --zone="${ZONE}"

	local expected current
	expected="gke_${PROJECT}_${ZONE}_${CLUSTER}"
	current="$(kubectl config current-context)"
	if [[ "${current}" != "${expected}" ]]; then
		echo "Refusing to continue: kubectl context is '${current}'," >&2
		echo "expected '${expected}'. Aborting before any cluster writes." >&2
		exit 1
	fi
	echo "Active kubectl context: ${current}"
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
# Parts B2–B3 — install/upgrade the GAG chart. `upgrade --install` is the
# idempotent form of `helm install`.
# ---------------------------------------------------------------------------

install_gag() {
	local values
	values="$(mktemp)"
	# Use :- so the trap is safe under `set -u` if it fires after the local
	# goes out of scope (e.g. a set -e abort later in the function).
	trap 'rm -f "${values:-}"' EXIT

	# Dogfood/dev mode: float image tags (production pins digests). Self-signed
	# webhook cert (no cert-manager). Keep GMC on default-pool so it goes down
	# when that pool scales to 0; AGC/proxy inherit scheduling via the worker
	# pool's taint.
	cat >"${values}" <<'EOF'
allowFloatingImageTags: true
gmc:
  image:
    tag: latest
agc:
  image:
    tag: latest
proxy:
  image:
    tag: latest
certManager:
  enabled: false
nodeSelector:
  cloud.google.com/gke-nodepool: default-pool
EOF

	echo "Installing/upgrading GAG chart..."
	helm upgrade --install gag "${REPO_ROOT}/charts/actions-gateway" \
		--namespace gmc-system --create-namespace \
		--values "${values}"

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
	kubectl label namespace gag-dogfood \
		actions-gateway.github.com/tenant=true \
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
# Part B7 — the ActionsGateway CR (the `ci` runner group).
# ---------------------------------------------------------------------------

apply_cr() {
	echo "Applying ActionsGateway CR..."
	kubectl apply -f - <<EOF
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: dogfood-gateway
  namespace: gag-dogfood
spec:
  gitHubAppRef:
    name: github-app-v1
  gitHubURL: https://github.com/${REPO}
  securityProfile: baseline
  proxy:
    minReplicas: 1
    maxReplicas: 4
  runnerGroups:
    - name: ci
      runnerLabels: ["self-hosted", "linux", "gag-ci"]
      maxListeners: 8
      maxWorkers: 4
      podTemplate:
        spec:
          tolerations:
            - key: dedicated
              value: workers
              effect: NoSchedule
          containers:
            - name: runner
              resources:
                requests:
                  cpu: "2"
                  memory: "4Gi"
                limits:
                  cpu: "4"
                  memory: "8Gi"
EOF
}

# Show the resolved target and require explicit confirmation before any
# billable create or cluster write. ASSUME_YES=1 bypasses it for automation.
confirm_target() {
	cat <<MSG
About to bootstrap the dogfood environment:
  Project: ${PROJECT}
  Cluster: ${CLUSTER}  (zone ${ZONE})
  Repo:    ${REPO}
This creates/updates billable GKE resources and installs GAG into the cluster.
MSG
	if [[ "${ASSUME_YES:-}" == "1" ]]; then
		echo "ASSUME_YES=1 set — skipping confirmation."
		return
	fi
	local reply
	read -r -p "Proceed? [y/N] " reply
	if [[ "${reply}" != "y" && "${reply}" != "Y" && "${reply}" != "yes" ]]; then
		echo "Aborted — no changes made."
		exit 1
	fi
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
	install_gag
	create_namespace
	create_secret
	apply_quota
	apply_cr

	echo ""
	echo "Bootstrap complete. GAG is installed and the gag-dogfood tenant is up."
	echo ""
	echo "Verify the gateway and that runners registered (~2 min after AGC Ready):"
	echo "  kubectl get actionsgateway -n gag-dogfood dogfood-gateway"
	echo "  kubectl get pods -n gag-dogfood"
	echo "  gh api /repos/${REPO}/actions/runners \\"
	echo "    --jq '.runners[] | {name, status, labels: [.labels[].name]}'"
	echo ""
	echo "Next steps:"
	echo "  1. Land the Part C2 workflow changes (runs-on -> vars.GAG_RUNNER)."
	echo "  2. Route CI to GAG:   scripts/dogfood-start.sh"
	echo "  3. Take it offline:   scripts/dogfood-stop.sh"
	echo "  4. One-time e2e pool: scripts/dogfood-e2e-setup.sh"
}

main "$@"
