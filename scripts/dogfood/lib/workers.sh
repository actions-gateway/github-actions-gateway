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

# WORKER_POD_NAMESPACE — optional namespace scope for the pod probes below.
# Empty (the default) reads all namespaces, which is what the whole-cluster
# teardown (stop.sh) wants. e2e-stop.sh sets it to the e2e tenant namespace so
# its pre-delete drain does not wait on the always-on CI tenant's workers, which
# keep flowing after the e2e window closes.
WORKER_POD_NAMESPACE="${WORKER_POD_NAMESPACE:-}"

# worker_pod_scope — print the kubectl namespace argument the probes use:
# --all-namespaces, or --namespace=<ns> when WORKER_POD_NAMESPACE scopes them.
worker_pod_scope() {
	if [[ -n "${WORKER_POD_NAMESPACE}" ]]; then
		echo "--namespace=${WORKER_POD_NAMESPACE}"
	else
		echo "--all-namespaces"
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

# WORKER_DRAIN_INITIAL_PODS / WORKER_DRAIN_FINAL_PODS — the `namespace/name` set
# in flight at the drain's first and last successful readings. wait_for_worker_drain
# sets both; drain_progress_summary turns them into the one fact a timed-out
# operator needs, which pod counts cannot supply (see list_inflight_workers).
WORKER_DRAIN_INITIAL_PODS=""
WORKER_DRAIN_FINAL_PODS=""

# list_inflight_workers — print one `namespace/name` line per worker pod in a
# non-terminal phase. Exits 1 when the cluster cannot be read, so a caller can
# tell an empty cluster from an unreadable one.
#
# Non-terminal, not just Running: a Pending worker pod means a job was already
# acquired and will start the moment its node is up, so scaling down on it
# strands exactly as much as scaling down on a Running one.
#
# Identities, not just a count, because the count alone cannot tell a slow drain
# from a wedged one: a tenant sitting at its concurrency ceiling admits a new pod
# for every one it reaps, holding the count flat while work still completes.
# Columns are named explicitly rather than parsed out of the default `get pods`
# output, whose leading namespace column is present under --all-namespaces and
# absent under --namespace.
list_inflight_workers() {
	local out
	out="$(worker_kubectl get pods "$(worker_pod_scope)" \
		--selector="${WORKER_POD_SELECTOR}" \
		--field-selector='status.phase!=Succeeded,status.phase!=Failed' \
		-o custom-columns='NS:.metadata.namespace,POD:.metadata.name' \
		--no-headers 2>/dev/null)" || return 1
	# NF skips the blank line an empty result leaves.
	printf '%s\n' "${out}" | awk 'NF {print $1 "/" $2}'
}

# count_inflight_workers — print how many worker pods are in a non-terminal
# phase, or "unknown" when the cluster cannot be read.
#
# A read failure is reported as "unknown" and never as 0. Callers gate a
# billable scale-down on this, and an unreachable cluster must not read as an
# idle one.
count_inflight_workers() {
	local out
	out="$(list_inflight_workers)" || {
		echo "unknown"
		return
	}
	# awk, not `grep -c`, because grep exits 1 on no match and would abort the
	# caller under `set -e`.
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

	local i empty_streak=0 seeded=0 pods inflight
	WORKER_DRAIN_INITIAL_PODS=""
	WORKER_DRAIN_FINAL_PODS=""
	for ((i = 1; i <= attempts; i++)); do
		if pods="$(list_inflight_workers)"; then
			inflight="$(printf '%s\n' "${pods}" | awk 'NF {c++} END {print c+0}')"
			WORKER_DRAIN_FINAL_PODS="${pods}"
			if ((seeded == 0)); then
				WORKER_DRAIN_INITIAL_PODS="${pods}"
				seeded=1
			fi
		else
			inflight="unknown"
		fi
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

# describe_inflight_workers — print one table row per in-flight worker pod: the
# diagnostic an operator needs when a drain times out, naming the pod to chase
# and the node it is pinning. Best-effort — prints nothing when the cluster
# cannot be read, since the caller has already reported that.
#
# SCHED and WAIT carry the two ways a worker pod stalls without ever leaving
# Pending: SCHED=Unschedulable when no node can take it, and a WAIT reason when
# it is on a node but its container will not start. Phase alone reads "Pending"
# for both, and for a pod that is merely waiting on a node coming up.
describe_inflight_workers() {
	worker_kubectl get pods "$(worker_pod_scope)" \
		--selector="${WORKER_POD_SELECTOR}" \
		--field-selector='status.phase!=Succeeded,status.phase!=Failed' \
		-o custom-columns='NS:.metadata.namespace,POD:.metadata.name,PHASE:.status.phase,SCHED:.status.conditions[?(@.type=="PodScheduled")].reason,WAIT:.status.containerStatuses[*].state.waiting.reason,NODE:.spec.nodeName' \
		2>/dev/null || true
}

# explain_inflight_workers — print the latest warning event for each in-flight
# worker pod, one `namespace/name  REASON: message` line apiece.
#
# The columns above say a pod is stuck; only its events say why, and the two
# stalls seen on the dogfood cluster are indistinguishable without them:
# FailedScheduling names the resource the pod cannot get, and FailedMount names
# the `job-payload` secret that does not exist — which reads as a bare
# ContainerCreating in every other view. Runs once, on the timeout path, so a
# lookup per pod is cheap. Best-effort throughout: a pod whose events cannot be
# read still gets a line.
explain_inflight_workers() {
	local ref ns pod why
	while IFS= read -r ref; do
		[[ -n "${ref}" ]] || continue
		ns="${ref%%/*}"
		pod="${ref#*/}"
		# `|| true` is what makes "best-effort" true: under pipefail a failed
		# read fails the assignment, and errexit then aborts the whole
		# diagnostic at the first unreadable pod — dropping every pod after it
		# and, in stop.sh, the warning about not scaling the pool down (Q733).
		why="$(worker_kubectl get events --namespace="${ns}" \
			--field-selector="involvedObject.name=${pod},type=Warning" \
			-o 'jsonpath={range .items[*]}{.reason}{": "}{.message}{"\n"}{end}' \
			2>/dev/null | awk 'NF' | tail -n 1 || true)"
		printf '  %s  %s\n' "${ref}" "${why:-no warning events}"
	done < <(list_inflight_workers)
}

# drain_progress_summary — say whether a timed-out drain was converging, from
# the pod sets wait_for_worker_drain recorded. Call only after it returns 1.
#
# Turnover is the signal, not the count: a tenant at its concurrency ceiling
# admits a pod for every one it reaps, so a drain that is working its way
# through a backlog and one that is wedged both hold the count flat. Pods
# present at both the first and last reading are the ones that never moved.
drain_progress_summary() {
	local final held
	final="$(printf '%s\n' "${WORKER_DRAIN_FINAL_PODS}" | awk 'NF {c++} END {print c+0}')"
	held="$(comm -12 \
		<(printf '%s\n' "${WORKER_DRAIN_INITIAL_PODS}" | awk 'NF' | sort) \
		<(printf '%s\n' "${WORKER_DRAIN_FINAL_PODS}" | awk 'NF' | sort) |
		awk 'NF {c++} END {print c+0}')"

	if ((final == 0)); then
		echo "The cluster could not be read for the whole wait — occupancy is unknown."
	elif ((held == final)); then
		echo "None of the ${final} in-flight pod(s) turned over during the wait: every pod in"
		echo "flight now was in flight when the drain started. The drain is NOT converging,"
		echo "and waiting longer will not clear it."
	else
		echo "${held} of the ${final} in-flight pod(s) have been in flight since the drain started;"
		echo "the rest turned over, so work is completing. The drain is converging — it needs"
		echo "a longer budget (DRAIN_TIMEOUT), not intervention."
	fi
}

# count_queued_scale_set_jobs LABEL — print how many queued GitHub Actions jobs
# in REPO target LABEL as a runs-on label, or "unknown" when GitHub cannot be
# read. Worker pods only exist for jobs an AGC has already acquired; a job still
# queued at GitHub has no pod, so the pod probes above cannot see it — yet
# deleting the AGC strands it just as surely (nothing is left to acquire it, and
# its run wedges the workflow's concurrency group; 2026-07-31 incident). Reads
# REPO from the environment like the callers' other gh touchpoints.
#
# A read failure is "unknown", never 0: callers gate an AGC delete on this, and
# an unreachable GitHub must not read as an empty queue.
count_queued_scale_set_jobs() {
	local label="$1"
	local runs
	runs="$(gh run list --repo "${REPO}" --status queued -L 50 \
		--json databaseId --jq '.[].databaseId' 2>/dev/null)" || {
		echo "unknown"
		return
	}
	local run n count=0
	while IFS= read -r run; do
		[[ -z "${run}" ]] && continue
		n="$(gh api "repos/${REPO}/actions/runs/${run}/jobs" \
			--jq "[.jobs[] | select(.status == \"queued\" and (.labels | index(\"${label}\")))] | length" \
			2>/dev/null)" || {
			echo "unknown"
			return
		}
		count=$((count + n))
	done <<<"${runs}"
	echo "${count}"
}

# wait_for_scale_set_idle LABEL [TIMEOUT_SECONDS] [INTERVAL_SECONDS] — block
# until no queued job targets LABEL, polling every INTERVAL. Returns 0 once the
# queue is empty, 1 if TIMEOUT elapses first or GitHub stays unreadable. A
# single clean reading suffices (no settle window): routing is reset before the
# callers drain, so nothing new targets the label, and a job acquired between
# polls becomes a worker pod the pod drain that follows still sees.
wait_for_scale_set_idle() {
	local label="$1"
	local timeout="${2:-${WORKER_DRAIN_TIMEOUT_DEFAULT}}"
	local interval="${3:-${WORKER_DRAIN_INTERVAL_DEFAULT}}"

	local attempts=$((timeout / interval + 1))
	local i queued
	for ((i = 1; i <= attempts; i++)); do
		queued="$(count_queued_scale_set_jobs "${label}")"
		if [[ "${queued}" == "0" ]]; then
			return 0
		fi
		if [[ "${queued}" == "unknown" ]]; then
			echo "  cannot read the job queue — an unreachable GitHub is not an empty queue" >&2
		else
			echo "  ${queued} queued job(s) still target '${label}'; waiting (up to ${timeout}s)..."
		fi
		if ((i < attempts)); then
			sleep "${interval}"
		fi
	done
	return 1
}
