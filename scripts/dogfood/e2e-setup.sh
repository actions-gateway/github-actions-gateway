#!/usr/bin/env bash
# One-time setup: e2e node pool with nested virtualization, Kata Containers
# runtime, and the gag-dogfood-e2e tenant — namespace + GitHub App Secret +
# ResourceQuota + the v2beta1 tenant CRs (ActionsGateway + RunnerTemplate +
# RunnerSet, ScaleSet single-label). See docs/plan/gke-dogfood.md Part F.
#
# NOTE: this is the Kata isolation path. It is NOT the live/validated e2e path —
# that is the privileged-DinD kustomize overlay (deploy/dogfood-e2e/overlays/dind,
# e2-standard-8 pool), which e2e-start.sh applies on demand. Kata is sized/validated
# under Q226 (the runner's ~5-vCPU peak needs a node bigger than n2-standard-4).
#
# Run once after the main cluster setup (Parts A–B of the runbook).
# Idempotent and safe to re-run: the e2e node-pool create is skipped if the
# pool already exists, and every kubectl object is server-side upserted.
#
# Required env vars (export before running):
#   PROJECT          GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER          GKE cluster name (e.g. gag-dogfood)
#   ZONE             GCP zone (e.g. us-east1-b)
#   REPO             GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
#   APP_ID           GitHub App numeric ID (3752347)
#   INSTALLATION_ID  GitHub App installation ID for this repo
#
# Optional:
#   KATA_VERSION     Kata Containers release tag (default: stable-3.x from GH)
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

# Kata deploy manifests — pin a version; check https://github.com/kata-containers/kata-containers/releases
KATA_VERSION="${KATA_VERSION:-3.14.0}"
KATA_BASE="https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}"

create_node_pool() {
	if gcloud container node-pools describe e2e \
		--project="${PROJECT}" --cluster="${CLUSTER}" --zone="${ZONE}" >/dev/null 2>&1; then
		echo "Node pool 'e2e' already exists — skipping create."
		return
	fi
	echo "Creating e2e node pool with nested virtualization..."
	# n2-standard-4: N2 family is required for nested virtualization on GCP.
	# --enable-nested-virtualization exposes /dev/kvm on the node, which
	# Kata uses to spin up a microVM per pod.
	gcloud container node-pools create e2e \
		--project="${PROJECT}" \
		--cluster="${CLUSTER}" \
		--zone="${ZONE}" \
		--machine-type=n2-standard-4 \
		--enable-nested-virtualization \
		--spot \
		--num-nodes=0 \
		--min-nodes=0 \
		--max-nodes=2 \
		--enable-autoscaling \
		--node-taints=dedicated=e2e:NoSchedule \
		--disk-size=100GB
}

install_kata() {
	local kata_deploy="${KATA_BASE}/kata-deploy-stable.yaml"

	echo "Applying Kata RBAC..."
	kubectl apply -f "${KATA_BASE}/kata-rbac.yaml"

	echo "Applying Kata DaemonSet (version ${KATA_VERSION})..."
	kubectl apply -f "${kata_deploy}"

	# Scope Kata to e2e pool nodes only — the system/workers pools use COS
	# by default which Kata does not support (no /dev/kvm).
	echo "Scoping kata-deploy DaemonSet to e2e pool nodes..."
	kubectl patch daemonset kata-deploy -n kube-system \
		--type=merge \
		-p '{"spec":{"template":{"spec":{"nodeSelector":{"cloud.google.com/gke-nodepool":"e2e"}}}}}'

	echo "Waiting for Kata DaemonSet (will only complete once e2e nodes exist)..."
	echo "  (With 0 e2e nodes running, this may print a warning and continue.)"
	kubectl rollout status daemonset/kata-deploy -n kube-system --timeout=2m || true
}

apply_runtimeclass() {
	echo "Applying kata-qemu RuntimeClass..."
	kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata-qemu
handler: kata-qemu
scheduling:
  nodeClassification:
    nodeSelector:
      katacontainers.io/kata-runtime: "true"
    tolerations:
      - key: dedicated
        value: e2e
        effect: NoSchedule
EOF
	# scheduling.nodeClassification ensures pods using this RuntimeClass only
	# schedule on nodes where the Kata DaemonSet has finished installing
	# (it labels nodes katacontainers.io/kata-runtime=true on completion).
}

create_namespace() {
	echo "Creating gag-dogfood-e2e namespace..."
	kubectl create namespace gag-dogfood-e2e --dry-run=client -o yaml \
		| kubectl apply -f -
	# v2 markers (actions-gateway.com/*): tenant=managed authorizes the GMC to
	# operate in the namespace; security-profile=baseline drives the Pod Security
	# level the GMC stamps. The Kata microVM is the isolation boundary, so the pod
	# stays baseline (no privileged needed). (v1 used the actions-gateway.github.com
	# group with tenant=true + an inline spec.securityProfile.)
	kubectl label namespace gag-dogfood-e2e \
		actions-gateway.com/tenant=managed \
		actions-gateway.com/security-profile=baseline \
		pod-security.kubernetes.io/enforce=baseline \
		--overwrite
}

create_secret() {
	local pem_file
	pem_file="$(mktemp)"
	# :- keeps the trap safe under set -u if it fires after the local goes out
	# of scope (e.g. a set -e abort later in the script).
	trap 'rm -f "${pem_file:-}"' EXIT

	echo "Retrieving GitHub App private key from keychain..."
	security find-generic-password \
		-a actions-gateway-test -s github-app-private-key -w \
		| xxd -r -p > "${pem_file}"

	# Fail loudly rather than create a Secret with an empty/garbage key, which
	# would surface later as opaque GAG auth failures.
	if [[ ! -s "${pem_file}" ]]; then
		echo "GitHub App private key from keychain is empty — aborting." >&2
		exit 1
	fi

	echo "Creating GitHub App secret in gag-dogfood-e2e..."
	kubectl create secret generic github-app-v1 \
		--namespace=gag-dogfood-e2e \
		--from-literal=appId="${APP_ID}" \
		--from-literal=installationId="${INSTALLATION_ID}" \
		--from-file=privateKey="${pem_file}" \
		--dry-run=client -o yaml \
		| kubectl apply -f -

	rm -f "${pem_file}"
	trap - EXIT
}

apply_quota() {
	kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ResourceQuota
metadata:
  name: dogfood-e2e-quota
  namespace: gag-dogfood-e2e
spec:
  hard:
    pods: "6"
EOF
}

apply_cr() {
	echo "Applying v2beta1 ActionsGateway + RunnerTemplate + RunnerSet (Kata)..."
	# Authored directly at v2beta1 (Q231) — the graduated served+storage front-door
	# shape (Q273), deliberately UNLIKE scripts/dogfood/setup.sh (main dogfood) which
	# authors at v2alpha1 to exercise the conversion webhook. The v1 monolith
	# (ActionsGateway.runnerGroups + inline securityProfile/proxy) is decomposed into
	# ActionsGateway (gateway + credentials) + RunnerTemplate (worker pod shape) +
	# RunnerSet (runner group). v2beta1 is ScaleSet-only and strips
	# acquisitionProtocol + maxListeners: the set declares exactly ONE runnerLabel
	# (gag-ci-e2e), which is both its runs-on target and the scale-set name at GitHub,
	# matched by GAG_E2E_RUNNER (e2e-start.sh).
	#
	# ISOLATION: this is the Kata path (baseline PSA; the kata-qemu microVM is the
	# boundary, so the dind sidecar is unprivileged). The live/validated e2e isolation
	# is the privileged-DinD kustomize overlay (deploy/dogfood-e2e/overlays/dind on the
	# e2-standard-8 pool); the Kata path is NOT live-validated yet — the measured runner
	# peak (~5 vCPU) exceeds a whole n2-standard-4, so the node pool must grow (e.g.
	# n2-standard-8) before Kata is sized. Tracked under Q226; the worker resources
	# below are provisional pending that sizing.
	kubectl apply -f - <<EOF
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata:
  name: dogfood-e2e
  namespace: gag-dogfood-e2e
spec:
  credentials:
    type: GitHubApp
    githubApp:
      name: github-app-v1
  githubURL: https://github.com/${REPO}
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerTemplate
metadata:
  name: kata
  namespace: gag-dogfood-e2e
spec:
  podTemplate:
    spec:
      runtimeClassName: kata-qemu
      nodeSelector:
        cloud.google.com/gke-nodepool: e2e
      tolerations:
        - key: dedicated
          value: e2e
          effect: NoSchedule
      # dind as a NATIVE sidecar (restartPolicy: Always init container, K8s >=1.29).
      # Load-bearing: a regular sidecar's dockerd never exits, so the pod never
      # completes, the AGC keeps the session active, and maxWorkers strands (Q249).
      # Unprivileged — the Kata microVM, not privileged:true, is the isolation
      # boundary, so this stays within the baseline PSA profile.
      initContainers:
        - name: dind
          image: docker:27-dind
          restartPolicy: Always
          args: ["--host=tcp://0.0.0.0:2375", "--tls=false"]
          env:
            - name: DOCKER_TLS_CERTDIR
              value: ""
          resources:
            requests: { cpu: "1", memory: "2Gi" }
            limits: { memory: "4Gi" }
      containers:
        - name: runner
          env:
            - name: DOCKER_HOST
              value: tcp://localhost:2375
          resources:
            requests: { cpu: "2", memory: "1Gi" }
            limits: { memory: "3Gi" }
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata:
  name: ci-e2e
  namespace: gag-dogfood-e2e
spec:
  gatewayRef:
    name: dogfood-e2e
  templateRef:
    name: kata
  runnerLabels: ["gag-ci-e2e"]
  maxWorkers: 2
EOF
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
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"
	require_cmd security "built-in macOS tool — macOS required to read keychain"
	require_cmd xxd "built-in macOS/Linux tool"

	confirm_or_exit "About to create a billable nested-virtualization e2e node pool and install Kata + the gag-dogfood-e2e tenant into project ${PROJECT}, cluster ${CLUSTER} (zone ${ZONE})."

	create_node_pool

	# Point kubectl at the dogfood cluster and fail closed if it is not the
	# active context, so Kata + the App Secret never land on another cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	install_kata
	apply_runtimeclass
	create_namespace
	create_secret
	apply_quota
	apply_cr

	echo ""
	echo "Setup complete."
	echo ""
	echo "Next steps:"
	echo "  The e2e-reusable.yml runs-on is already wired to fromJSON(vars.GAG_E2E_RUNNER)"
	echo "  (default ubuntu-latest), so CI is unaffected until you route e2e onto GAG."
	echo "  Enable e2e on GAG (on-demand — spins up the tenant AGC): scripts/dogfood/e2e-start.sh"
	echo "  Disable + tear the AGC back down:                          scripts/dogfood/e2e-stop.sh"
}

main "$@"
