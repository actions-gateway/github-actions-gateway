#!/usr/bin/env bash
# One-time setup for the dogfood e2e tenant's CLUSTER INFRA — the pieces the
# kustomize overlays can't express: the e2e node pool (nested virtualization +
# Workload Identity), the Kata Containers runtime + `kata` RuntimeClass alias,
# the gag-dogfood-e2e namespace, and the GitHub App Secret.
# See the GKE dogfood plan (indexed in docs/plan/README.md), Part F.
#
# The tenant OBJECTS (ResourceQuota + ActionsGateway + ClusterRunnerTemplate +
# RunnerSet + egress policy, and the namespace's security-profile gates) are
# owned by the worker-isolation overlays under deploy/dogfood-e2e/overlays/
# (dind = privileged DinD, kata = unprivileged kind-in-Kata) and applied
# on demand by e2e-start.sh (E2E_VARIANT selects the overlay) — not here.
#
# Q226 validated unprivileged dockerd + kind inside a Kata microVM on GKE and
# corrected this script's Kata install (the old release-asset URLs 404; the
# RuntimeClass used an invalid scheduling.nodeClassification field; the pool did
# not pin --image-type so it got COS). Q286 wires GAG's e2e suite through it —
# see the kata-on-gke plan (indexed in docs/plan/README.md).
#
# Run once after the main cluster setup (Parts A–B of the runbook).
# Idempotent and safe to re-run: the e2e node-pool create is skipped if the
# pool already exists, and every kubectl object is server-side upserted.
#
# Required env vars (export before running):
#   PROJECT          GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER          GKE cluster name (e.g. gag-dogfood)
#   ZONE             GCP zone (e.g. us-east1-b)
#   APP_ID           GitHub App numeric ID (3752347)
#   INSTALLATION_ID  GitHub App installation ID for this repo
#
# Optional:
#   KATA_VERSION     Kata Containers chart/appVersion (default below)
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# kata_install / kata_apply_runtimeclass + the KATA_* pool labels live in the
# shared lib so the kata-install op (scripts/dogfood/ops.sh kata-install) reuses
# exactly this logic instead of duplicating it.
# shellcheck source=scripts/dogfood/lib/kata.sh
source "${REPO_ROOT}/scripts/dogfood/lib/kata.sh"

# The window where a Kata pod binds before kata-deploy finishes installing
# resolves itself: sandbox creation fails and the kubelet retries until the
# handler appears (see the KATA_RUNTIME_LABEL_KEY note in lib/kata.sh).
create_node_pool() {
	if gcloud container node-pools describe e2e \
		--project="${PROJECT}" --cluster="${CLUSTER}" --zone="${ZONE}" >/dev/null 2>&1; then
		echo "Node pool 'e2e' already exists — skipping create."
		return
	fi
	echo "Creating e2e node pool with nested virtualization..."
	# --enable-nested-virtualization exposes /dev/kvm on the node, which Kata uses
	# to spin up a microVM per pod. GCP rejects the create outright for a family
	# that cannot do it, naming the ones that can:
	#
	#   A2, A3, C2, C3, C4, C4D, C4N, G2, H3, H4D, N1, N2, N4, N4D, Z3, M4
	#
	# Intel-only in practice — the AMD families (C2D, N2D) are NOT on it, whatever
	# older notes here said. It also needs --image-type=UBUNTU_CONTAINERD (or
	# COS_CONTAINERD at 1.28.4-gke.1083000+); without the explicit image type GKE
	# defaults to COS and kata-deploy cannot install onto it. Verified under Q226.
	#
	# --workload-metadata=GKE_METADATA is a SECURITY PREREQUISITE, not a nicety:
	# Kata isolates the kernel, not the pod network, so without Workload Identity
	# the runner can still mint the node's service-account token from the metadata
	# server. Requires --workload-pool on the cluster.
	#
	# n2 rather than c2 for quota headroom, not performance (Q627). The regional
	# C2_CPUS default is 8 — one node of the 8-vCPU shape this pool needs, so a
	# refused scale-up has nowhere to retry — and a request to raise it was denied
	# 2026-07-31. N2_CPUS defaults to 200, and n2 is on the nested-virt list above
	# at the same 8 vCPU/32 GB. It is also this pool's original family, so it is
	# already proven here.
	gcloud container node-pools create e2e \
		--project="${PROJECT}" \
		--cluster="${CLUSTER}" \
		--zone="${ZONE}" \
		--machine-type=n2-standard-8 \
		--image-type=UBUNTU_CONTAINERD \
		--enable-nested-virtualization \
		--workload-metadata=GKE_METADATA \
		--spot \
		--num-nodes=0 \
		--min-nodes=0 \
		--max-nodes=2 \
		--enable-autoscaling \
		--node-labels="${KATA_POOL_LABEL_KEY}=true,${KATA_RUNTIME_LABEL_KEY}=true" \
		--node-taints=dedicated=e2e:NoSchedule \
		--disk-size=100GB
}

create_namespace() {
	echo "Creating gag-dogfood-e2e namespace..."
	kubectl create namespace gag-dogfood-e2e --dry-run=client -o yaml \
		| kubectl apply -f -
	# Only the v2 tenant marker here (authorizes the GMC to operate in the
	# namespace). The security-profile / PSA gates are isolation-specific and
	# owned by the overlay e2e-start.sh applies. Both current variants need the
	# privileged profile — dind because its sidecar IS privileged, kata because
	# PSS baseline forbids the guest-scoped capability adds its UNPRIVILEGED
	# dockerd needs (see deploy/dogfood-e2e/overlays/kata/resources.yaml).
	kubectl label namespace gag-dogfood-e2e \
		actions-gateway.com/tenant=managed \
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

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
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

	kata_install
	kata_apply_runtimeclass
	create_namespace
	create_secret

	echo ""
	echo "Setup complete."
	echo ""
	echo "Next steps:"
	echo "  The e2e-reusable.yml runs-on is already wired to fromJSON(vars.GAG_E2E_RUNNER)"
	echo "  (default ubuntu-latest), so CI is unaffected until you route e2e onto GAG."
	echo "  Enable e2e on GAG (on-demand — applies the E2E_VARIANT overlay [dind|kata]"
	echo "  and spins up the tenant AGC):     scripts/dogfood/e2e-start.sh"
	echo "  Disable + tear the AGC back down: scripts/dogfood/e2e-stop.sh"
}

[[ -n "${E2E_SETUP_LIB_ONLY:-}" ]] || main "$@"
