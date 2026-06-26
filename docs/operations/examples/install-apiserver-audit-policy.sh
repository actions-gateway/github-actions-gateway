#!/usr/bin/env bash
#
# install-apiserver-audit-policy.sh — install the Actions Gateway API-server
# audit policy on a self-managed kubeadm control plane.
#
# The audit policy is a static file read by kube-apiserver at startup, NOT a
# Kubernetes object — there is no `kubectl apply` for it, and Helm cannot install
# it (it deploys workloads, not control-plane node files). This script automates
# the only feasible self-managed path: copy the policy onto the node and patch
# the kube-apiserver static-pod manifest so the kubelet restarts the API server
# with audit logging enabled.
#
# Run it ONCE PER CONTROL-PLANE NODE, as root, on the node itself (the manifest
# at /etc/kubernetes/manifests/ and the policy path are node-local). It is
# idempotent: a second run detects the flags are already present and exits.
#
# Managed control planes (EKS / GKE / AKS) do not expose --audit-policy-file —
# do NOT use this script there. See
# docs/operations/security-operations.md § Install the audit policy for the
# per-provider managed-audit-log path.
#
# Requires: yq (https://github.com/mikefarah/yq) for the structured manifest
# edit — it cannot corrupt the YAML the way a text substitution could.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# Defaults — override via flags.
POLICY_SRC="${SCRIPT_DIR}/apiserver-audit-policy.yaml"
POLICY_DEST="/etc/kubernetes/audit/policy.yaml"
MANIFEST="/etc/kubernetes/manifests/kube-apiserver.yaml"
LOG_DIR="/var/log/kubernetes/audit"
DRY_RUN=false

usage() {
	cat <<EOF
Usage: $(basename "$0") [options]

Install the Actions Gateway API-server audit policy on this kubeadm
control-plane node and enable audit logging in kube-apiserver.

Options:
  --policy PATH     Source policy file       (default: ${POLICY_SRC})
  --dest PATH       On-node policy path       (default: ${POLICY_DEST})
  --manifest PATH   kube-apiserver manifest   (default: ${MANIFEST})
  --log-dir PATH    Audit log directory       (default: ${LOG_DIR})
  --dry-run         Print what would change, write nothing
  -h, --help        Show this help

Run once per control-plane node, as root. Idempotent.
EOF
}

log() { printf '==> %s\n' "$*"; }
err() { printf 'error: %s\n' "$*" >&2; }

parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
			--policy) POLICY_SRC="$2"; shift 2 ;;
			--dest) POLICY_DEST="$2"; shift 2 ;;
			--manifest) MANIFEST="$2"; shift 2 ;;
			--log-dir) LOG_DIR="$2"; shift 2 ;;
			--dry-run) DRY_RUN=true; shift ;;
			-h|--help) usage; exit 0 ;;
			*) err "unknown argument: $1"; usage >&2; exit 2 ;;
		esac
	done
}

preflight() {
	command -v yq >/dev/null 2>&1 || {
		err "yq not found on PATH — install: https://github.com/mikefarah/yq"
		exit 1
	}
	[[ -f "$POLICY_SRC" ]] || { err "policy file not found: $POLICY_SRC"; exit 1; }
	# Structural sanity-check: the source must be an audit Policy, not some other
	# YAML pointed at by mistake.
	local kind
	kind="$(yq '.kind' "$POLICY_SRC")"
	[[ "$kind" == "Policy" ]] || {
		err "$POLICY_SRC is not an audit Policy (kind=$kind); refusing to install"
		exit 1
	}
	[[ -f "$MANIFEST" ]] || {
		err "kube-apiserver manifest not found: $MANIFEST"
		err "this script must run ON a kubeadm control-plane node"
		exit 1
	}
	if [[ "$DRY_RUN" == false && "$(id -u)" != "0" ]]; then
		err "must run as root to write $POLICY_DEST and $MANIFEST (or use --dry-run)"
		exit 1
	fi
}

install_policy_file() {
	log "install policy: $POLICY_SRC -> $POLICY_DEST"
	if [[ "$DRY_RUN" == true ]]; then return 0; fi
	install -D -m 0644 "$POLICY_SRC" "$POLICY_DEST"
}

# patch_manifest injects the audit flags, volumeMounts, and volumes into the
# kube-apiserver static-pod manifest with yq (a structured edit — it cannot
# corrupt the YAML). It is a no-op if --audit-policy-file is already present.
patch_manifest() {
	if grep -q -- '--audit-policy-file=' "$MANIFEST"; then
		log "audit flags already present in $MANIFEST — nothing to patch"
		return 0
	fi

	local backup
	backup="${MANIFEST}.bak.$(date +%Y%m%d%H%M%S)"
	log "patch manifest: $MANIFEST (backup: $backup)"
	if [[ "$DRY_RUN" == true ]]; then
		log "dry-run: would add --audit-policy-file=$POLICY_DEST and mount $POLICY_DEST + $LOG_DIR"
		return 0
	fi
	cp -p "$MANIFEST" "$backup"

	# Export for yq strenv() so paths reach the expression without quoting hazards.
	export POLICY_DEST LOG_DIR
	local log_path="${LOG_DIR}/audit.log"
	export log_path

	yq -i '
	  .spec.containers[0].command += [
	    "--audit-policy-file=" + strenv(POLICY_DEST),
	    "--audit-log-path=" + strenv(log_path),
	    "--audit-log-maxage=30",
	    "--audit-log-maxbackup=10",
	    "--audit-log-maxsize=100"
	  ]
	  | .spec.containers[0].volumeMounts += [
	    {"name": "audit-policy", "mountPath": strenv(POLICY_DEST), "readOnly": true},
	    {"name": "audit-log", "mountPath": strenv(LOG_DIR)}
	  ]
	  | .spec.volumes += [
	    {"name": "audit-policy", "hostPath": {"path": strenv(POLICY_DEST), "type": "File"}},
	    {"name": "audit-log", "hostPath": {"path": strenv(LOG_DIR), "type": "DirectoryOrCreate"}}
	  ]
	' "$MANIFEST"
}

main() {
	parse_args "$@"
	preflight
	install_policy_file
	patch_manifest
	if [[ "$DRY_RUN" == true ]]; then
		log "dry-run complete — no changes written"
		return 0
	fi
	cat <<EOF
==> done. The kubelet will restart kube-apiserver from the updated manifest
    within ~a minute. Verify with:

      crictl ps | grep kube-apiserver        # new container ID after restart
      tail -f ${LOG_DIR}/audit.log           # audit events appear here

    Repeat this on every other control-plane node. Then forward the log to your
    SIEM and translate the predicates in
    docs/operations/security-operations.md § Audit-log abuse detections into
    alerts. To roll back, restore the timestamped *.bak manifest backup.
EOF
}

main "$@"
