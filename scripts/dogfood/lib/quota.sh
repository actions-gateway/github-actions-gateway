# Global CPU-budget arithmetic for the dogfood cluster. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/dogfood/lib/quota.sh
#   source "$REPO_ROOT/scripts/dogfood/lib/quota.sh"
#
# Why this exists (Q631): the release gate runs two legs that draw on one
# project-wide CPU limit. Its deploy leg routes CI to GAG, whose `workers` pool
# autoscales to 8 e2-standard-4 nodes; its e2e leg then needs 2 n2-standard-8
# nodes the autoscaler cannot get, and reports `FailedScaleUp: GCE quota
# exceeded` without ever naming the quota. Two v1.3.0-rc.5 runs died that way,
# after three wrong diagnoses that chased the family and regional quotas.
#
# Raising the limit moves the collision rather than removing it, so the
# competition is capped here instead: the e2e and system pools' ceilings come
# off the live budget first, and `workers` gets an autoscale max derived from
# what is left.
#
# The budget is read live on purpose. A limit transcribed into a script or a doc
# is a number that ages — CPUS_ALL_REGIONS was 32 until 2026-08-03 and is 64 now
# — and the arithmetic that depends on it ages with it.
#
# Callers must have `set -euo pipefail` active and PROJECT, CLUSTER and ZONE set.
# shellcheck shell=bash

# The quota that actually binds. It is global and project-wide, not regional and
# not per-family: on the dogfood project the regional CPUS and N2_CPUS limits are
# 200 apiece against a 52-vCPU worst case, so neither ever refuses a scale-up.
# Only the resize API's 429 body names this one; the autoscaler's FailedScaleUp
# event does not.
GLOBAL_CPU_QUOTA_METRIC="CPUS_ALL_REGIONS"

# machine_type_vcpu TYPE — print the vCPU count encoded in a GCE machine type
# name (`e2-standard-4` -> 4). Returns 1 on a name that does not carry one (the
# shared-core shapes, `e2-small` and friends) rather than guessing: every caller
# is sizing a billable reservation, and a silent 0 reserves nothing.
machine_type_vcpu() {
	local type="$1"
	[[ "${type}" =~ -([0-9]+)$ ]] || return 1
	echo "${BASH_REMATCH[1]}"
}

# reserved_pool_max BUDGET USED RESERVED PER_NODE — print how many PER_NODE-vCPU
# nodes fit in BUDGET once USED and RESERVED are taken out. Pure (no cluster
# access) so quota-test.sh asserts it directly.
#
# Floors at 0 rather than going negative: an over-subscribed budget is a real
# state (someone left the benchmark pool up) and the caller reports it, but a
# negative node count would read as a valid ceiling everywhere downstream.
reserved_pool_max() {
	local budget="$1" used="$2" reserved="$3" per_node="$4"
	local free=$((budget - used - reserved))
	((free > 0)) || {
		echo 0
		return
	}
	echo $((free / per_node))
}

# global_cpu_budget — print "LIMIT USED" for GLOBAL_CPU_QUOTA_METRIC on PROJECT,
# as whole vCPU. Returns 1 when the project cannot be read or the metric is
# absent from the response.
#
# Never falls back to a default. The reservation this feeds gates an hour-long
# billable run, and an unreadable quota is not an unlimited one — the same
# reason lib/workers.sh reports "unknown" rather than 0.
#
# gcloud reports both figures as floats ("64.0"), so awk truncates them to int.
global_cpu_budget() {
	local rows
	rows="$(gcloud compute project-info describe --project="${PROJECT}" \
		--flatten='quotas[]' \
		--format='value(quotas.metric,quotas.limit,quotas.usage)')" || return 1
	printf '%s\n' "${rows}" | awk -v metric="${GLOBAL_CPU_QUOTA_METRIC}" '
		$1 == metric { printf "%d %d\n", $2, $3; found = 1 }
		END { exit !found }
	'
}

# pool_machine_type POOL — print a node pool's machine type. Returns 1 when the
# pool cannot be read; gcloud's own stderr says why.
pool_machine_type() {
	gcloud container node-pools describe "$1" \
		--cluster="${CLUSTER}" --zone="${ZONE}" --project="${PROJECT}" \
		--format='value(config.machineType)' || return 1
}

# pool_autoscaling POOL — print "MIN MAX" for an autoscaled node pool. Prints
# nothing for a pool with autoscaling off (the manually sized system and
# benchmark pools), which is a distinct answer from a failed read: that returns
# 1.
#
# GKE omits minNodeCount when it holds its 0 default, so the row arrives with a
# leading empty field — ",8", not "0,8" — and the max lands in min unless the
# split preserves it. An absent min is printed as the 0 it stands for. Same
# proto3 omission as currentNodeCount (Q779).
#
# The separator is a comma rather than value()'s default tab because tab is an
# IFS *whitespace* character: `read` drops leading runs of it whatever IFS is
# set to, so no IFS can recover the empty first field from a tab-separated row.
pool_autoscaling() {
	local row min max
	row="$(gcloud container node-pools describe "$1" \
		--cluster="${CLUSTER}" --zone="${ZONE}" --project="${PROJECT}" \
		--format='value[separator=","](autoscaling.minNodeCount,autoscaling.maxNodeCount)')" || return 1
	IFS=',' read -r min max <<<"${row}"
	[[ -n "${max}" ]] || return 0
	echo "${min:-0} ${max}"
}

# set_pool_autoscale_max POOL MIN MAX — move an autoscaled pool's ceiling.
# --project/--zone are pinned so the update never relies on the active gcloud
# config, matching every other mutating call in this group.
set_pool_autoscale_max() {
	local pool="$1" min="$2" max="$3"
	gcloud container clusters update "${CLUSTER}" \
		--project="${PROJECT}" --zone="${ZONE}" \
		--node-pool="${pool}" --enable-autoscaling \
		--min-nodes="${min}" --max-nodes="${max}" --quiet
}
