#!/usr/bin/env bash
# Recurring dogfood/e2e maintenance + debug ops, folded into one reviewed
# dispatcher so they aren't hand-typed ad hoc with per-command
# PROD_GUARD_OVERRIDEs each time (Q342). Each subcommand pins --project/--zone
# on every gcloud call and verifies the resolved kubectl context (via
# gke_get_credentials_and_verify) before any mutating kubectl call, exactly like
# the setup/start/stop lifecycle scripts.
#
# Why there is no PROD_GUARD_OVERRIDE inside: the prod-guard hook parses ad-hoc
# Bash tool calls, not the kubectl/gcloud commands *inside* a `bash scripts/...`
# invocation (docs/development/kind-iteration.md § Verify the resolved target),
# so scripting an op is what makes it hook-exempt — reviewed once here instead
# of overridden per command. The target pinning below is the real safety layer.
#
# Idempotent + safe to re-run: a pool resize to the current node count is a
# no-op, `helm upgrade --install` and `kubectl apply` converge, `rollout
# restart` just rolls again, and the debug pods clean up after themselves.
#
# Subcommands:
#   pool-scale <pool> <nodes>   Resize a node pool (default-pool|workers|e2e).
#   kata-install                (Re)install kata-deploy + the `kata` RuntimeClass.
#   agc-bounce [ci|e2e]         Roll-restart the CI (default) or e2e AGC + wait.
#   debug-pod [--kata]          Interactive throwaway shell pod on the e2e pool.
#   kvm-check [<node>]          Verify /dev/kvm on the e2e node(s).
#
# Required env vars (export before running):
#   PROJECT   GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-east1-b)
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/dogfood/lib/kata.sh
source "${REPO_ROOT}/scripts/dogfood/lib/kata.sh"

# Tenant coordinates. The AGC Deployment the GMC provisions for a gateway is
# `<gateway-name>-agc` in the gateway's namespace (agcResourceSuffix in
# cmd/gmc/internal/controller/actionsgateway_v2_builder.go).
CI_NAMESPACE="gag-dogfood"
CI_AGC_DEPLOYMENT="dogfood-agc"
E2E_NAMESPACE="gag-dogfood-e2e"
E2E_AGC_DEPLOYMENT="dogfood-e2e-agc"
# GKE stamps the node pool onto every node as this label; the e2e pool is `e2e`
# (created by e2e-setup.sh).
NODEPOOL_LABEL_KEY="cloud.google.com/gke-nodepool"
E2E_POOL="e2e"

usage() {
	cat <<'EOF'
Recurring dogfood/e2e maintenance + debug ops (Q342). Requires PROJECT, CLUSTER,
and ZONE exported (see docs/plan/gke-dogfood.md § Variables).

Usage: scripts/dogfood/ops.sh <subcommand> [args]

  pool-scale <pool> <nodes>   Resize a node pool (default-pool|workers|e2e).
  kata-install                (Re)install kata-deploy + the `kata` RuntimeClass.
  agc-bounce [ci|e2e]         Roll-restart the CI (default) or e2e AGC + wait.
  debug-pod [--kata]          Interactive throwaway shell pod on the e2e pool.
  kvm-check [<node>]          Verify /dev/kvm on the e2e node(s).
EOF
}

# op_pool_scale POOL NODES — resize a node pool, pinning --project/--zone so the
# resize never relies on the active gcloud config.
op_pool_scale() {
	local pool="${1:-}" nodes="${2:-}"
	if [[ -z "$pool" || -z "$nodes" ]]; then
		echo "usage: ops.sh pool-scale <pool> <nodes>" >&2
		exit 1
	fi
	if [[ ! "$nodes" =~ ^[0-9]+$ ]]; then
		echo "error: <nodes> must be a non-negative integer (got '${nodes}')" >&2
		exit 1
	fi
	echo "Resizing node pool '${pool}' to ${nodes} node(s) on ${CLUSTER} (${PROJECT}/${ZONE})..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool="${pool}" --num-nodes="${nodes}" --zone="${ZONE}" --quiet
	echo "Done."
}

# op_kata_install — (re)install the kata-deploy DaemonSet + `kata` RuntimeClass
# alias without re-running the full billable e2e-setup.sh. Reuses the shared lib.
op_kata_install() {
	# Point kubectl at the dogfood cluster and fail closed if it is not the
	# active context, so the Kata install never lands on another cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"
	kata_install
	kata_apply_runtimeclass
	echo "Done."
}

# op_agc_bounce [ci|e2e] — roll-restart an AGC Deployment and wait for it to
# come back. Use after a GMC/AGC config change or when an AGC is wedged.
#
# Restarts the control plane only: it deletes no worker pod, so it never clears
# a stalled stop.sh drain (which counts worker pods). Needs a GMC carrying the
# Q552 fix — older ones revert the restart annotation and the rollout reports
# success while nothing rolls.
op_agc_bounce() {
	local which="${1:-ci}" ns dep
	case "$which" in
		ci) ns="${CI_NAMESPACE}"; dep="${CI_AGC_DEPLOYMENT}" ;;
		e2e) ns="${E2E_NAMESPACE}"; dep="${E2E_AGC_DEPLOYMENT}" ;;
		*)
			echo "error: agc-bounce target must be 'ci' or 'e2e' (got '${which}')" >&2
			exit 1
			;;
	esac
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"
	echo "Bouncing AGC deployment/${dep} in ${ns}..."
	kubectl rollout restart "deployment/${dep}" --namespace "${ns}"
	kubectl rollout status "deployment/${dep}" --namespace "${ns}" --timeout=3m
	echo "Done."
}

# debug_pod_overrides RUNTIME_CLASS — emit the pod-spec JSON that pins a debug
# pod to the e2e pool (nodeSelector + taint toleration), optionally under a
# RuntimeClass. Compact JSON (no heredoc) so it passes cleanly as --overrides.
debug_pod_overrides() {
	local runtime_class="$1" rc_field=""
	[[ -n "$runtime_class" ]] && rc_field="\"runtimeClassName\":\"${runtime_class}\","
	printf '{"spec":{%s"nodeSelector":{"%s":"%s"},"tolerations":[{"key":"dedicated","value":"e2e","effect":"NoSchedule","operator":"Equal"}]}}' \
		"$rc_field" "$NODEPOOL_LABEL_KEY" "$E2E_POOL"
}

# op_debug_pod [--kata] — launch an interactive throwaway busybox pod on the
# e2e pool for bisecting worker shape (`--kata` runs it under the kata
# RuntimeClass, matching the isolation the worker gets). --rm auto-deletes on
# exit; a stale same-named pod from a crashed prior run is removed first so the
# op is safe to re-run.
op_debug_pod() {
	local runtime_class=""
	case "${1:-}" in
		--kata) runtime_class="kata" ;;
		"") ;;
		*)
			echo "error: debug-pod takes at most '--kata' (got '${1}')" >&2
			exit 1
			;;
	esac
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"
	local name="dogfood-debug" overrides
	overrides="$(debug_pod_overrides "$runtime_class")"
	kubectl delete pod "${name}" --namespace "${E2E_NAMESPACE}" --ignore-not-found
	echo "Launching interactive debug pod '${name}' on the ${E2E_POOL} pool${runtime_class:+ (runtimeClass ${runtime_class})}..."
	echo "  (The e2e pool autoscales from 0, so scheduling can take a minute.)"
	kubectl run "${name}" --namespace "${E2E_NAMESPACE}" \
		--image=busybox --restart=Never --rm -it \
		--overrides="${overrides}" -- sh
}

# prune_node_debuggers — delete the node-debugger-* pods `kubectl debug node/`
# leaves behind in the default namespace, so repeated kvm-checks don't
# accumulate detached pods.
prune_node_debuggers() {
	local pods pod
	pods="$(kubectl get pods -n default -o name 2>/dev/null || true)"
	# shellcheck disable=SC2086  # one pod/name per line, no spaces — split intentionally
	for pod in $pods; do
		case "$pod" in
			pod/node-debugger-*) kubectl delete "$pod" -n default --ignore-not-found ;;
		esac
	done
}

# op_kvm_check [NODE] — verify /dev/kvm is exposed on the e2e node(s) (the
# nested-virtualization prerequisite Kata needs). With no arg it checks every
# up e2e-pool node; the pool autoscales from 0, so there may be none.
op_kvm_check() {
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"
	local nodes node
	if [[ -n "${1:-}" ]]; then
		nodes="node/${1}"
	else
		nodes="$(kubectl get nodes -l "${NODEPOOL_LABEL_KEY}=${E2E_POOL}" -o name)"
	fi
	if [[ -z "$nodes" ]]; then
		echo "No ${E2E_POOL}-pool nodes are up. Scale one up (ops.sh pool-scale ${E2E_POOL} 1)"
		echo "or dispatch an e2e run so the pool autoscales, then re-check."
		return 0
	fi
	while IFS= read -r node; do
		node="${node#node/}"
		echo "==> /dev/kvm on ${node} (expect: crw-rw---- ... 10, 232):"
		kubectl debug "node/${node}" -it --image=busybox --profile=general \
			-- ls -l /host/dev/kvm || true
	done <<<"$nodes"
	prune_node_debuggers
}

main() {
	local cmd="${1:-}"

	# Help + arg-error paths short-circuit before the env/tool checks, so an
	# operator can read the usage without exporting the Variables block first.
	case "$cmd" in
		-h|--help|help) usage; return 0 ;;
		"")
			echo "error: no subcommand given" >&2
			usage >&2
			exit 1
			;;
		pool-scale|kata-install|agc-bounce|debug-pod|kvm-check) ;;
		*)
			echo "error: unknown subcommand '${cmd}'" >&2
			usage >&2
			exit 1
			;;
	esac
	shift

	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"

	case "$cmd" in
		pool-scale) op_pool_scale "$@" ;;
		kata-install) op_kata_install "$@" ;;
		agc-bounce) op_agc_bounce "$@" ;;
		debug-pod) op_debug_pod "$@" ;;
		kvm-check) op_kvm_check "$@" ;;
	esac
}

main "$@"
