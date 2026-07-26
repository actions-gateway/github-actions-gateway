# Shared worker-pod occupancy probes for the dogfood lifecycle scripts. Source,
# don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/dogfood/lib/workers.sh
#   source "$REPO_ROOT/scripts/dogfood/lib/workers.sh"
#
# Why this exists (Q434): worker pods are reaped by their tenant AGC and by
# nothing else. Take the system pool to 0 while a CI job is in flight and the
# AGC is evicted with nowhere to reschedule, so its workers keep running, their
# `workers`-pool nodes stay pinned by pods no controller will ever delete, and
# the cluster bills spot nodes until a human notices. One incident stranded 82
# spot node-hours that way. Every script that scales the system pool down or
# deletes the cluster therefore reads worker occupancy first, and reads it the
# same way from here.
#
# Callers must have `set -euo pipefail` active and point kubectl at the dogfood
# cluster first (these read the active context — gate them with
# gke_get_credentials_and_verify), or set WORKER_KUBECTL_CONTEXT to pin one
# without touching the active context.
# shellcheck shell=bash

# WORKER_KUBECTL_CONTEXT — optional kubectl context for the probes below. Empty
# (the default) uses the active context, which is what the lifecycle scripts
# want after gke_get_credentials_and_verify has asserted it. delete.sh sets it
# instead: it reads occupancy to decide whether to prompt, and pinning the
# context per call keeps that read from depending on kubeconfig state it never
# established.
WORKER_KUBECTL_CONTEXT="${WORKER_KUBECTL_CONTEXT:-}"

# worker_kubectl ARGS... — kubectl with the pinned context applied when one is
# set. Not exported as an alias: the probes call it directly so a stubbed
# `kubectl` in the unit tests still intercepts.
worker_kubectl() {
	if [[ -n "${WORKER_KUBECTL_CONTEXT}" ]]; then
		kubectl --context="${WORKER_KUBECTL_CONTEXT}" "$@"
	else
		kubectl "$@"
	fi
}

# WORKER_POD_SELECTOR — the label selector matching every AGC-provisioned worker
# pod on both tiers. The AGC stamps the recommended app.kubernetes.io/* set on
# every worker pod it builds (provisioner's buildPod, via apilabels.Recommended
# with workerAppName/workerComponent), so this matches classic and scale-set
# workers in every tenant namespace without naming either tier's owner label
# (actions-gateway/runner-group vs actions-gateway.com/runner-set). Matching on
# labels rather than the `runner-` name prefix means a tenant pod that merely
# happens to be named that way is not miscounted as in-flight CI.
WORKER_POD_SELECTOR="app.kubernetes.io/name=actions-runner,app.kubernetes.io/component=runner"

# Drain-wait defaults, overridable per call. The timeout covers the longest job
# the dogfood dispatches can put on a worker (`timeout-minutes: 20` in
# unit-test.yml) plus reap and settle time; the interval is a compromise between
# a responsive drain and hammering the API server for up to 25 minutes.
WORKER_DRAIN_TIMEOUT_DEFAULT=1500
WORKER_DRAIN_INTERVAL_DEFAULT=15

# WORKER_DRAIN_SETTLE_READINGS — how many consecutive empty readings count as
# drained. More than one because "no worker pods right now" is not the same as
# "no work left": between one job's pod being reaped and the listener spawning
# the next queued job's pod there is a gap, and a single reading taken inside it
# would scale the pool down mid-batch — the exact incident this guards. Two
# readings an interval apart do not close the gap entirely; the bounded timeout
# is what makes the remaining risk safe, since it fails the stop rather than
# stranding nodes.
WORKER_DRAIN_SETTLE_READINGS=2

# count_inflight_workers — print how many worker pods are in a non-terminal
# phase cluster-wide, or "unknown" when the cluster cannot be read.
#
# Non-terminal, not just Running: a Pending worker pod means a job was already
# acquired and will start the moment its node is up, so scaling down on it
# strands exactly as much as scaling down on a Running one.
#
# A read failure is reported as "unknown" and never as 0. Callers gate a
# billable scale-down on this, and an unreachable cluster must not read as an
# idle one.
count_inflight_workers() {
	local out
	out="$(worker_kubectl get pods --all-namespaces \
		--selector="${WORKER_POD_SELECTOR}" \
		--field-selector='status.phase!=Succeeded,status.phase!=Failed' \
		--no-headers 2>/dev/null)" || {
		echo "unknown"
		return
	}
	# awk, not `grep -c`, because grep exits 1 on no match and would abort the
	# caller under `set -e`. NF skips the blank line an empty result leaves.
	printf '%s\n' "${out}" | awk 'NF {c++} END {print c+0}'
}

# wait_for_worker_drain [TIMEOUT_SECONDS] [INTERVAL_SECONDS] — block until no
# worker pod is in flight, polling every INTERVAL. Returns 0 once drained,
# 1 if TIMEOUT elapses first or the cluster stays unreadable.
#
# Progress goes to stdout so an operator watching a long drain can see which
# jobs are holding it up; the caller decides what a 1 means (stop.sh refuses to
# scale down, which leaves the AGC alive to finish reaping).
wait_for_worker_drain() {
	local timeout="${1:-${WORKER_DRAIN_TIMEOUT_DEFAULT}}"
	local interval="${2:-${WORKER_DRAIN_INTERVAL_DEFAULT}}"

	# Attempt count rather than a wall-clock deadline so the settle readings
	# always get to run: a caller passing a timeout shorter than the settle
	# window wants a fast drain check, not a guaranteed failure.
	local attempts=$((timeout / interval + 1))
	if ((attempts < WORKER_DRAIN_SETTLE_READINGS)); then
		attempts="${WORKER_DRAIN_SETTLE_READINGS}"
	fi

	local i empty_streak=0 inflight
	for ((i = 1; i <= attempts; i++)); do
		inflight="$(count_inflight_workers)"
		if [[ "${inflight}" == "unknown" ]]; then
			echo "  cannot read worker pods — an unreachable cluster is not an idle one" >&2
			empty_streak=0
		elif ((inflight == 0)); then
			empty_streak=$((empty_streak + 1))
			if ((empty_streak >= WORKER_DRAIN_SETTLE_READINGS)); then
				return 0
			fi
			echo "  no worker pods in flight — confirming (${empty_streak}/${WORKER_DRAIN_SETTLE_READINGS})..."
		else
			empty_streak=0
			echo "  ${inflight} worker pod(s) still in flight; waiting (up to ${timeout}s)..."
		fi
		if ((i < attempts)); then
			sleep "${interval}"
		fi
	done
	return 1
}

# describe_inflight_workers — print one `namespace/name  phase  node` line per
# in-flight worker pod: the diagnostic an operator needs when a drain times out,
# naming both the pod to chase and the node it is pinning. Best-effort — prints
# nothing when the cluster cannot be read, since the caller has already reported
# that.
describe_inflight_workers() {
	worker_kubectl get pods --all-namespaces \
		--selector="${WORKER_POD_SELECTOR}" \
		--field-selector='status.phase!=Succeeded,status.phase!=Failed' \
		-o custom-columns='POD:.metadata.namespace/.metadata.name,PHASE:.status.phase,NODE:.spec.nodeName' \
		2>/dev/null || true
}
