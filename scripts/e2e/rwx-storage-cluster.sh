#!/usr/bin/env bash
# rwx-storage-cluster.sh — stand up a kind cluster with a real ReadWriteMany
# storage class, so the shared worker storage reference architecture is validated
# rather than asserted (Q719). Idempotent: safe to re-run.
#
# Why this exists: GAG's workers are storage-less, and a shared RWX volume is
# entirely a tenant podTemplate concern — the provisioner never provisions one. So
# nothing in the fast tiers can observe whether such a pod actually mounts the
# volume, whether two workers on two nodes really see one filesystem, or whether
# the worker's gap-filled UID can write to it. Those belong to a kubelet, a CSI
# driver and two nodes. This cluster supplies all three.
#
# The RWX backend is csi-driver-nfs in front of an in-cluster NFS server, which is
# what lets the harness run on a laptop with no cloud filesystem. The driver, the
# server and the mount are genuine; only the storage appliance is disposable.
#
# Environment:
#   RWX_STORAGE_CLUSTER — kind cluster name (default: gag-rwx)
#   KIND_NODE_IMAGE     — pin the node image, e.g. kindest/node:vX.Y.Z@sha256:...
#                         (optional; leave UNSET here — riding kind's release
#                         default is what keeps this harness on the Kubernetes
#                         minor kind ships. The e2e tier pins its node image DOWN;
#                         this harness must not copy that.)
#   CSI_DRIVER_NFS_VERSION — csi-driver-nfs release (default below). Bumping this
#                         is the point: a driver whose fsGroupPolicy or provisioning
#                         behaviour changed fails `make test-rwx-storage`.
#
# After this script runs, `make test-rwx-storage` drives workers through it.
# Tear down with `make rwx-storage-cluster-delete`.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

RWX_STORAGE_CLUSTER=${RWX_STORAGE_CLUSTER:-gag-rwx}
# Keep the assignment on one line in this exact shape — updatecli matches it by
# regex the way it does cluster-autoscaler's.
CSI_DRIVER_NFS_VERSION=${CSI_DRIVER_NFS_VERSION:-v4.13.4}

KUBE_CONTEXT="kind-${RWX_STORAGE_CLUSTER}"
CSI_BASE="https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/${CSI_DRIVER_NFS_VERSION}/deploy/${CSI_DRIVER_NFS_VERSION}"

# kubectl_ctx pins every call to this script's own cluster. Parallel sessions
# share the ambient kubectl context, and an unpinned apply of a CSI driver with
# cluster-wide volume rights is not a mistake worth leaving reachable.
kubectl_ctx() {
	kubectl --context "${KUBE_CONTEXT}" "$@"
}

create_cluster() {
	if kind get clusters 2>/dev/null | grep -qx "${RWX_STORAGE_CLUSTER}"; then
		echo "==> kind cluster ${RWX_STORAGE_CLUSTER} already exists"
		return 0
	fi
	local create_args=(create cluster --name "${RWX_STORAGE_CLUSTER}"
		--config test/rwx-storage/kind-config.yaml --wait 180s)
	if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
		echo "==> using pinned node image ${KIND_NODE_IMAGE}"
		create_args+=(--image "${KIND_NODE_IMAGE}")
	fi
	echo "==> creating kind cluster ${RWX_STORAGE_CLUSTER}"
	kind "${create_args[@]}"
}

install_csi_driver() {
	echo "==> installing csi-driver-nfs ${CSI_DRIVER_NFS_VERSION}"
	# The four manifests install-driver.sh applies for the driver proper. The
	# snapshot controller and its CRDs are deliberately left out: nothing here
	# snapshots, and installing a cluster-wide snapshot controller into a harness
	# cluster is surface with no assertion behind it.
	local f
	for f in rbac-csi-nfs.yaml csi-nfs-driverinfo.yaml csi-nfs-controller.yaml csi-nfs-node.yaml; do
		kubectl_ctx apply -f "${CSI_BASE}/${f}"
	done
	kubectl_ctx -n kube-system rollout status deploy/csi-nfs-controller --timeout=300s
	kubectl_ctx -n kube-system rollout status ds/csi-nfs-node --timeout=300s
}

install_nfs_server() {
	echo "==> installing the in-cluster NFS server"
	kubectl_ctx apply -f test/rwx-storage/nfs-server.yaml
	kubectl_ctx -n gag-rwx-storage rollout status deploy/nfs-server --timeout=300s
	kubectl_ctx apply -f test/rwx-storage/storageclass.yaml
}

# verify_rwx binds one throwaway claim before declaring the cluster ready. A
# StorageClass that applies cleanly has proven nothing: the mount happens at
# provisioning time, and a wrong share path fails there rather than at apply.
# Without this the first symptom is a test timing out on a Pending pod.
verify_rwx() {
	echo "==> verifying the class actually provisions ReadWriteMany"
	kubectl_ctx apply -f test/rwx-storage/verify-claim.yaml
	if ! kubectl_ctx -n gag-rwx-storage wait --for=jsonpath='{.status.phase}'=Bound \
		pvc/rwx-preflight --timeout=180s; then
		echo "error: the preflight claim never bound; dumping diagnostics" >&2
		kubectl_ctx -n gag-rwx-storage describe pvc rwx-preflight >&2 || true
		kubectl_ctx -n kube-system logs deploy/csi-nfs-controller -c nfs --tail=40 >&2 || true
		return 1
	fi
	local modes
	modes=$(kubectl_ctx -n gag-rwx-storage get pvc rwx-preflight -o jsonpath='{.status.accessModes[*]}')
	if [[ "${modes}" != *ReadWriteMany* ]]; then
		echo "error: the class bound the preflight claim as '${modes}', not ReadWriteMany" >&2
		return 1
	fi
	kubectl_ctx -n gag-rwx-storage delete pvc rwx-preflight --wait=false >/dev/null
}

main() {
	create_cluster
	install_csi_driver
	install_nfs_server
	verify_rwx
	echo
	echo "==> ready. Run the shared-storage validation with:"
	echo "      make test-rwx-storage"
}

main "$@"
