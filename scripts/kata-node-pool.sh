#!/usr/bin/env bash
#
# Provision (or print) a GKE Standard node pool with nested virtualization
# enabled on a nested-virt-capable machine family — the node-level prerequisite
# for Kata Containers (see docs/plan/kata-on-gke.md and
# docs/operations/kata-ci-spike-runbook.md). Kata runs each pod inside a
# lightweight VM via QEMU/KVM; GKE nodes are themselves GCE VMs, so KVM must be
# exposed to the node guest. This is a node-pool property — the runner pod that
# later uses `runtimeClassName: kata` needs no privileged context.
#
# This is a LIVE step: it requires authenticated `gcloud` and mutates the target
# GCP project. Set DRY_RUN=1 to print the exact command without executing it
# (used for offline review and by the spike runbook to show the invocation).
#
# Required env (no real values are hardcoded — pass them at call time):
#   PROJECT   GCP project ID                       (e.g. my-ci-project)
#   CLUSTER   existing GKE Standard cluster name   (e.g. gag-kata-ci)
#   REGION    cluster region or zone               (e.g. us-central1)
#
# Optional env (defaults shown):
#   NODE_POOL          kata-pool        node-pool name to create
#   MACHINE_TYPE       n2-standard-4    MUST be a nested-virt-capable family
#                                       (n2/n2d/c2/c2d); a2/a3/g2 GPU families
#                                       do NOT support nested virt on GKE.
#   NUM_NODES          1                nodes in the pool
#   DISK_SIZE          100              boot disk GiB (kind needs headroom)
#   IMAGE_TYPE         UBUNTU_CONTAINERD  Kata's kata-deploy targets containerd;
#                                       Ubuntu nodes carry the KVM module set.
#   LOCATION_FLAG      --region         use --zone for a zonal cluster
#   DRY_RUN            unset            set to 1 to print, not run
#
# NOTE — VERIFY AT SPIKE TIME: the exact nested-virt enablement surface on GKE
# evolves across gcloud releases (node-pool flag vs. node-system-config vs. a
# privileged kvm-device-plugin DaemonSet). Confirming that this invocation
# yields a node with /dev/kvm present is acceptance criterion #1 of the spike;
# cross-check against `gcloud container node-pools create --help` and the GKE
# nested-virtualization docs before the live run. Update this script with the
# confirmed flags as the spike's first recorded result.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

main() {
	: "${PROJECT:?PROJECT must be set (GCP project ID)}"
	: "${CLUSTER:?CLUSTER must be set (existing GKE Standard cluster)}"
	: "${REGION:?REGION must be set (cluster region or zone)}"

	local node_pool="${NODE_POOL:-kata-pool}"
	local machine_type="${MACHINE_TYPE:-n2-standard-4}"
	local num_nodes="${NUM_NODES:-1}"
	local disk_size="${DISK_SIZE:-100}"
	local image_type="${IMAGE_TYPE:-UBUNTU_CONTAINERD}"
	local location_flag="${LOCATION_FLAG:---region}"

	# Fail early on a known-incompatible machine family rather than after a slow
	# node-pool create that silently lacks /dev/kvm.
	case "${machine_type}" in
	n2-* | n2d-* | c2-* | c2d-*) ;;
	*)
		echo "ERROR: MACHINE_TYPE='${machine_type}' is not a nested-virt-capable family." >&2
		echo "       Use an n2/n2d/c2/c2d type; GPU families (a2/a3/g2) do not support nested virt on GKE." >&2
		exit 1
		;;
	esac

	# Built as an array so each flag is a single, correctly-quoted argv element.
	local -a cmd=(
		gcloud container node-pools create "${node_pool}"
		--project "${PROJECT}"
		--cluster "${CLUSTER}"
		"${location_flag}" "${REGION}"
		--machine-type "${machine_type}"
		--num-nodes "${num_nodes}"
		--disk-size "${disk_size}"
		--image-type "${image_type}"
		# Expose nested virtualization (KVM) to the node guest — the prerequisite
		# for Kata's QEMU hypervisor. VERIFY this flag against current gcloud (see
		# header note); it is the crux of spike acceptance criterion #1.
		--enable-nested-virtualization
		# Label the pool so the runner pod / ActionsGateway CR can target it via a
		# nodeSelector and so Kata's kata-deploy DaemonSet can be scoped to it.
		--node-labels "katacontainers.io/kata-runtime=true"
	)

	if [[ -n "${DRY_RUN:-}" ]]; then
		printf '%q ' "${cmd[@]}"
		printf '\n'
		return 0
	fi

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	echo "Creating nested-virt node pool '${node_pool}' on cluster '${CLUSTER}'..."
	"${cmd[@]}"
	echo "Done. Verify /dev/kvm on a node before installing Kata (see the spike runbook)."
}

main "$@"
