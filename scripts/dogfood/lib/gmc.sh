# Shared GMC readiness helpers for the dogfood scripts. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   source "${REPO_ROOT}/scripts/dogfood/lib/gmc.sh"
#
# The GMC's validating webhooks fail closed, so applying a v2 object fails with
# `no endpoints available for service "webhook-service"` while the GMC is down.
# That message names the dataplane rather than the cause, so each of the three
# entrypoints that applies a v2 object waits here first.
#
# At rest the dogfood cluster sits at 0 nodes, which makes that the ordinary
# state rather than an edge one: the pool resize is what gives the GMC somewhere
# to schedule, so every caller waits *after* its resize and *before* its apply.
#
# The same resize gives GKE's konnectivity agents a node. They carry the API
# server's calls to pods, so with none up every webhook call fails with `No agent
# available` while the GMC and its endpoints read healthy (Q1016).

# shellcheck shell=bash

# The Deployment serving the validating webhooks. One spelling, because three
# scripts had it four times between them and a rename would have missed some.
GMC_DEPLOYMENT="deployment/gmc-controller-manager"
GMC_NAMESPACE="gmc-system"

# GKE's own Deployment.
KONNECTIVITY_DEPLOYMENT="deployment/konnectivity-agent"
KONNECTIVITY_NAMESPACE="kube-system"

# gmc_ready [TIMEOUT] — 0 when the rollout is already complete within TIMEOUT
# (default 5s). Silent, and never fails the caller: it answers a question rather
# than asserting an outcome, so the caller decides what not-ready means.
gmc_ready() {
	kubectl rollout status "${GMC_DEPLOYMENT}" \
		--namespace "${GMC_NAMESPACE}" --timeout="${1:-5s}" >/dev/null 2>&1
}

# wait_for_gmc [TIMEOUT] — block until the rollout completes and a konnectivity
# agent is ready, failing the caller when either does not happen. TIMEOUT bounds
# each wait, and defaults to GMC_ROLLOUT_TIMEOUT, then to 5m.
#
# rollout status rather than a pod-label `kubectl wait`, for the reason
# start.sh's wait_agc_rollout records: that selector matches the outgoing
# ReplicaSet's terminating pod during a rollout, which never reaches Ready, so a
# healthy rollout reads as a timeout.
#
# One ready agent rather than the Deployment's rollout, so a replica that cannot
# schedule does not fail a bring-up. The API omits readyReplicas at zero, and a
# jsonpath wait with no value returns once the field is there.
wait_for_gmc() {
	local timeout="${1:-${GMC_ROLLOUT_TIMEOUT:-5m}}"
	echo "Waiting for the GMC rollout (it serves the validating webhooks)..."
	kubectl rollout status "${GMC_DEPLOYMENT}" \
		--namespace "${GMC_NAMESPACE}" --timeout="${timeout}" || return
	echo "Waiting for a konnectivity agent (it carries the API server's webhook calls)..."
	kubectl wait --namespace "${KONNECTIVITY_NAMESPACE}" \
		--for=jsonpath='{.status.readyReplicas}' "${KONNECTIVITY_DEPLOYMENT}" \
		--timeout="${timeout}"
}
