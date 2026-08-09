#!/usr/bin/env bash
#
# Provision (or print) a GKE Standard node pool with nested virtualization
# enabled on a nested-virt-capable machine family — the node-level prerequisite
# for Kata Containers (see docs/operations/kata-ci-spike-runbook.md). Kata runs
# each pod inside a lightweight VM via QEMU/KVM; GKE nodes are themselves GCE
# VMs, so KVM must be
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
#   MACHINE_TYPE       c2-standard-4    MUST be a nested-virt-capable family
#                                       (n2/n2d/c2/c2d); a2/a3/g2 GPU families
#                                       do NOT support nested virt on GKE.
#   NUM_NODES          1                nodes in the pool
#   DISK_SIZE          100              boot disk GiB (kind needs headroom)
#   IMAGE_TYPE         UBUNTU_CONTAINERD  Kata's kata-deploy targets containerd;
#                                       Ubuntu nodes carry the KVM module set.
#                                       COS_CONTAINERD works at 1.28.4-gke.1083000+.
#   POOL_LABEL         gag.dev/kata-ci=true  label used to SCOPE the kata-deploy
#                                       installer. Deliberately NOT
#                                       katacontainers.io/kata-runtime -- see below.
#   LOCATION_FLAG      --region         use --zone for a zonal cluster
#   DRY_RUN            unset            set to 1 to print, not run
#
# VALIDATED (Q226, 2026-07): --enable-nested-virtualization is a GA gcloud flag on
# `container node-pools create`, and the resulting node exposes /dev/kvm
# (crw-rw---- 10, 232) with the CPU `vmx` flag. Confirmed on GKE
# 1.35.5-gke.1241004 / Ubuntu 24.04 / c2-standard-4.
#
# CAPACITY: a nested-virt pool draws from the ordinary machine-family capacity
# pool (nested virt does not narrow it). n2/n2d were ZONE_RESOURCE_POOL_EXHAUSTED
# in us-central1-a during validation while quota sat at 0/200 -- a stockout, not a
# quota error. c2/c2d worked. Check the PER-FAMILY regional quota too (C2_CPUS
# defaults to 8 on a fresh project).
#
# PREFER `gcloud container clusters create --enable-nested-virtualization` where
# you can: it accepts the same flag, and a stockout during a *separate*
# `node-pools create` holds a cluster-level lock that blocks every other mutation
# -- including deleting the stuck pool -- for tens of minutes.
#
# SECURITY: this script does not enable Workload Identity. Kata isolates the
# kernel, NOT the pod network: the GKE metadata server stays reachable from inside
# the micro-VM and will serve the node's service-account token. Pass
# --workload-metadata=GKE_METADATA on the pool (and --workload-pool on the cluster)
# before running untrusted code. See docs/operations/kata-dind-workloads.md.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

main() {
	: "${PROJECT:?PROJECT must be set (GCP project ID)}"
	: "${CLUSTER:?CLUSTER must be set (existing GKE Standard cluster)}"
	: "${REGION:?REGION must be set (cluster region or zone)}"

	local node_pool="${NODE_POOL:-kata-pool}"
	local machine_type="${MACHINE_TYPE:-c2-standard-4}"
	local num_nodes="${NUM_NODES:-1}"
	local disk_size="${DISK_SIZE:-100}"
	local image_type="${IMAGE_TYPE:-UBUNTU_CONTAINERD}"
	local pool_label="${POOL_LABEL:-gag.dev/kata-ci=true}"
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
		# for Kata's QEMU hypervisor. GA flag; validated to yield /dev/kvm.
		--enable-nested-virtualization
		# Scope the kata-deploy installer to this pool. Deliberately NOT
		# katacontainers.io/kata-runtime: kata-deploy SETS that label itself once
		# the runtime is installed, and the RuntimeClass schedules pods on it.
		# Pre-applying it here would let a Kata pod land on a node before the
		# runtime exists. This pool is fixed-size, so the separation is free; a
		# pool that AUTOSCALES FROM ZERO must additionally bake the runtime label
		# into --node-labels or the autoscaler can never simulate a match and
		# Kata pods stay Pending forever (found live under Q286 — see
		# scripts/dogfood/e2e-setup.sh and docs/operations/kata-dind-workloads.md).
		--node-labels "${pool_label}"
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
