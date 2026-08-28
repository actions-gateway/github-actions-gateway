# Shared GMC readiness helpers for the dogfood scripts. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   source "${REPO_ROOT}/scripts/dogfood/lib/gmc.sh"
#
# Every v2 CRD sets `conversion.strategy: Webhook` against the GMC's
# webhook-service, so applying any v2 object is routed through /convert and
# fails with `no endpoints available for service "webhook-service"` while the
# GMC is down. That message names the dataplane rather than the cause, so each
# of the three entrypoints that applies a v2 object waits here first.
#
# At rest the dogfood cluster sits at 0 nodes, which makes that the ordinary
# state rather than an edge one: the pool resize is what gives the GMC somewhere
# to schedule, so every caller waits *after* its resize and *before* its apply.

# shellcheck shell=bash

# The Deployment serving the conversion webhook. One spelling, because three
# scripts had it four times between them and a rename would have missed some.
GMC_DEPLOYMENT="deployment/gmc-controller-manager"
GMC_NAMESPACE="gmc-system"

# gmc_ready [TIMEOUT] — 0 when the rollout is already complete within TIMEOUT
# (default 5s). Silent, and never fails the caller: it answers a question rather
# than asserting an outcome, so the caller decides what not-ready means.
gmc_ready() {
	kubectl rollout status "${GMC_DEPLOYMENT}" \
		--namespace "${GMC_NAMESPACE}" --timeout="${1:-5s}" >/dev/null 2>&1
}

# wait_for_gmc [TIMEOUT] — block until the rollout completes, failing the caller
# when it does not. TIMEOUT defaults to GMC_ROLLOUT_TIMEOUT, then to 5m.
#
# rollout status rather than a pod-label `kubectl wait`, for the reason
# start.sh's wait_agc_rollout records: that selector matches the outgoing
# ReplicaSet's terminating pod during a rollout, which never reaches Ready, so a
# healthy rollout reads as a timeout.
wait_for_gmc() {
	echo "Waiting for the GMC rollout (it serves the v2beta1 conversion webhook)..."
	kubectl rollout status "${GMC_DEPLOYMENT}" \
		--namespace "${GMC_NAMESPACE}" --timeout="${1:-${GMC_ROLLOUT_TIMEOUT:-5m}}"
}
