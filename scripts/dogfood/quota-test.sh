#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/lib/quota.sh — the global CPU-budget arithmetic
# the release gate reserves the e2e pool's capacity with (Q631).
#
# Why this is tested: every way it can be wrong is silent and expensive. A
# reservation that comes out too large caps CI for no reason; one that comes out
# too small lets the deploy leg hold the budget the e2e leg needs, and the
# autoscaler refuses that scale-up as a bare `FailedScaleUp: GCE quota exceeded`
# that names no quota — the failure mode that cost two v1.3.0-rc.5 runs and
# three wrong diagnoses. The quota read is the other half: an unreadable project
# must never read as an unlimited one, exactly as in lib/nodes.sh and
# lib/workers.sh. gcloud is stubbed, so no network and no project.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/dogfood/lib/quota.sh
source "${REPO_ROOT}/scripts/dogfood/lib/quota.sh"

PROJECT="octo-project"
CLUSTER="octo-cluster"
ZONE="octo-zone-b"

fails=0

check() {
	local name="$1" want="$2" got="$3"
	if [[ "${want}" == "${got}" ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: want '${want}', got '${got}'" >&2
		fails=$((fails + 1))
	fi
}

check_fails() {
	local name="$1"
	shift
	if "$@" >/dev/null 2>&1; then
		echo "FAIL ${name}: expected a non-zero exit" >&2
		fails=$((fails + 1))
	else
		echo "ok   ${name}"
	fi
}

# --- machine_type_vcpu: the shapes this cluster actually runs ---

check "e2-standard-2 is 2 vCPU" 2 "$(machine_type_vcpu e2-standard-2)"
check "e2-standard-4 is 4 vCPU" 4 "$(machine_type_vcpu e2-standard-4)"
check "n2-standard-8 is 8 vCPU" 8 "$(machine_type_vcpu n2-standard-8)"
check "c2d-standard-16 is 16 vCPU" 16 "$(machine_type_vcpu c2d-standard-16)"
check "n2-highmem-4 is 4 vCPU" 4 "$(machine_type_vcpu n2-highmem-4)"

# A shared-core shape carries no count. Failing loudly beats reserving 0 vCPU
# for a pool that is really burning some.
check_fails "e2-small has no count and is refused" machine_type_vcpu e2-small
check_fails "an empty type is refused" machine_type_vcpu ""

# --- reserved_pool_max: the arithmetic the reservation turns on ---

# Today's live shape: 64 vCPU budget, nothing in use, 2x e2-standard-2 system
# (4) + 2x n2-standard-8 e2e (16) reserved, e2-standard-4 workers. 44/4 = 11,
# above the pool's configured 8 — so the cap does not bind, which is the
# intended steady state.
check "today's budget leaves 11 worker nodes" 11 "$(reserved_pool_max 64 0 20 4)"

# The rc.5 shape, at the 32-vCPU limit that killed two runs: the same
# reservation leaves 3 worker nodes, and the e2e pool gets its 16 vCPU.
check "the old 32-vCPU limit leaves 3 worker nodes" 3 "$(reserved_pool_max 32 0 20 4)"

# A benchmark pool left up is the realistic way the budget shrinks under a gate
# that is otherwise unchanged: 4x e2-standard-4 workers-od = 16 vCPU in use.
check "16 vCPU already in use leaves 7" 7 "$(reserved_pool_max 64 16 20 4)"

# Integer division truncates rather than rounding up — a ceiling that rounds up
# is a ceiling that can exceed the budget.
check "a partial node is not counted" 2 "$(reserved_pool_max 32 0 21 4)"

# Nothing left, and over-subscribed, are both 0 and never negative: a negative
# count reads as a valid ceiling everywhere downstream.
check "an exactly-consumed budget leaves 0" 0 "$(reserved_pool_max 20 0 20 4)"
check "an over-subscribed budget floors at 0" 0 "$(reserved_pool_max 20 8 20 4)"
check "usage alone can over-subscribe" 0 "$(reserved_pool_max 64 64 20 4)"

# --- global_cpu_budget: stubbed gcloud ---

# FAKE_QUOTA_ROWS models `gcloud compute project-info describe --flatten` output
# (tab-separated metric/limit/usage, floats as gcloud prints them);
# FAKE_GCLOUD_FAIL models a project that cannot be read.
gcloud() {
	if [[ -n "${FAKE_GCLOUD_FAIL:-}" ]]; then
		return 1
	fi
	printf '%s' "${FAKE_QUOTA_ROWS:-}"
}

FAKE_QUOTA_ROWS=$'NETWORKS\t5.0\t1.0\nCPUS_ALL_REGIONS\t64.0\t0.0\nIMAGES\t100.0\t6.0'
check "reads the limit and usage as ints" "64 0" "$(global_cpu_budget)"

FAKE_QUOTA_ROWS=$'CPUS_ALL_REGIONS\t64.0\t28.0'
check "reads a non-zero usage" "64 28" "$(global_cpu_budget)"

# The metric sits among ~50 others, several of which also end in _CPUS. Matching
# a prefix or a substring would pick up GPUS_ALL_REGIONS or a family quota, and
# the family quotas are exactly what three wrong diagnoses chased.
FAKE_QUOTA_ROWS=$'GPUS_ALL_REGIONS\t0.0\t0.0\nCPUS_ALL_REGIONS\t64.0\t4.0\nCOMMITTED_CPUS\t200.0\t0.0'
check "matches the metric exactly, not a neighbour" "64 4" "$(global_cpu_budget)"

# An absent metric is a failed read, not a 0 budget: the caller refuses to run
# rather than deriving a cap from a number that was never there.
FAKE_QUOTA_ROWS=$'NETWORKS\t5.0\t1.0'
check_fails "an absent metric is a failure" global_cpu_budget

FAKE_QUOTA_ROWS=""
check_fails "an empty response is a failure" global_cpu_budget

FAKE_GCLOUD_FAIL=1
check_fails "an unreadable project is a failure, never an unlimited budget" global_cpu_budget
unset FAKE_GCLOUD_FAIL

# --- pool_autoscaling: autoscaling off is a distinct answer from a failed read ---

# Asserted through the `read` the callers do rather than on the raw string:
# gcloud's `value()` separator is a tab, and what matters is that min and max
# land in separate fields.
read_autoscaling() {
	local min max
	read -r min max <<<"$(pool_autoscaling "$1")"
	echo "${min:-<none>}/${max:-<none>}"
}

FAKE_QUOTA_ROWS=$'0\t8'
check "an autoscaled pool reports min and max" "0/8" "$(read_autoscaling workers)"

# The manually sized pools (default-pool, workers-od) print nothing at all, and
# the caller must be able to tell that from gcloud falling over.
FAKE_QUOTA_ROWS=""
check "a manually sized pool reports nothing" "" "$(pool_autoscaling default-pool)"

FAKE_GCLOUD_FAIL=1
check_fails "an unreadable pool is a failure" pool_autoscaling workers
unset FAKE_GCLOUD_FAIL

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all quota.sh assertions passed"
