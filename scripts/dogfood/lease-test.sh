#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/lib/lease.sh: the ownership lease that lets the
# release gate tell an orphaned run from a cluster somebody is using (Q640).
#
# Why it is tested: the lease is the sole trigger for a destructive teardown of a
# prod-classified GKE cluster, so every state it can report is a decision about
# whether to delete somebody's environment. Both directions are silent and both
# are expensive. Reading a live gate — or a hand-run debugging session — as
# orphaned tears down work in progress, which is strictly worse than the leak
# this mechanism exists to stop. Reading a dead gate as live leaves the leak in
# place, unfixed and now believed fixed.
#
# The two hard cases are pid reuse and another host: a recycled pid is a live
# process that is NOT the gate (reclaiming is correct), and a pid from another
# host means nothing here at all (acting on it is never correct). Both are
# asserted below, along with the atomicity that decides a two-gate race.
#
# No cluster and no processes: `lease_process_command` is stubbed, so a pid is
# whatever the test says it is, and RELEASE_LEASE_DIR points at scratch.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# Set before sourcing: the default resolves under $HOME at source time, and no
# test may write there.
RELEASE_LEASE_DIR="${WORKDIR}/lease"
export RELEASE_LEASE_DIR
# shellcheck source=scripts/dogfood/lib/lease.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease.sh"

fails=0

PROJECT=dogfood-proj
ZONE=us-east1-b
CLUSTER=gag-dogfood

# --- Stubs ------------------------------------------------------------------

# LIVE_PIDS maps a pid to the command line `ps` would report for it. A pid that
# is not a key is a dead process (empty output, which is what `ps -o command=`
# gives for a pid that no longer exists).
declare -A LIVE_PIDS=()
lease_process_command() { echo "${LIVE_PIDS[$1]:-}"; }

# HOSTNAME is what lease_host reads, so a test can mint a record as another host.
HOSTNAME="test-host"

reset_leases() {
	rm -rf "${RELEASE_LEASE_DIR}"
	LIVE_PIDS=()
	HOSTNAME="test-host"
}

# --- Assertions -------------------------------------------------------------

check() {
	local name="$1" want="$2" got="$3"
	if [[ "${want}" == "${got}" ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: want '${want}', got '${got}'" >&2
		fails=$((fails + 1))
	fi
}

check_contains() {
	local name="$1" needle="$2" haystack="$3"
	if [[ "${haystack}" == *"${needle}"* ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: '${needle}' not in '${haystack}'" >&2
		fails=$((fails + 1))
	fi
}

state() { lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}"; }
lease_file() { lease_path "${PROJECT}" "${ZONE}" "${CLUSTER}"; }

# write_lease PID HOST TARGET [MARKER] — hand-write a record, for the states a
# real acquire cannot produce from this process (another host, another pid).
write_lease() {
	mkdir -p "${RELEASE_LEASE_DIR}"
	{
		printf 'pid=%s\n' "$1"
		printf 'host=%s\n' "$2"
		printf 'started=%s\n' "$(date +%s)"
		printf 'target=%s\n' "$3"
		printf 'rc=v9.9.9-rc.1\n'
		printf 'marker=%s\n' "${4-validate-release.sh}"
	} >"$(lease_file)"
}

echo "scripts/dogfood/lease-test.sh"

# --- an unclaimed target is free, and free costs nothing ---------------------

reset_leases
check "no lease reads free" "free" "$(state)"

# --- acquire, hold, release --------------------------------------------------

reset_leases
LIVE_PIDS[$$]="bash scripts/dogfood/validate-release.sh v1.3.0-rc.4"
if lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "v1.3.0-rc.4"; then
	echo "ok   an unclaimed target can be acquired"
else
	echo "FAIL an unclaimed target must be acquirable" >&2
	fails=$((fails + 1))
fi
check "a live owner reads held" "held" "$(state)"
check "the lease records this process" "$$" "$(lease_field "$(lease_file)" pid)"
check "the lease records the RC" "v1.3.0-rc.4" "$(lease_field "$(lease_file)" rc)"
check "the lease records the target" "${PROJECT}/${ZONE}/${CLUSTER}" \
	"$(lease_field "$(lease_file)" target)"

lease_release "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "releasing an owned lease frees the target" "free" "$(state)"

# --- the two-gate race: ln decides, and the loser changes nothing ------------

reset_leases
LIVE_PIDS[$$]="bash scripts/dogfood/validate-release.sh v1.3.0-rc.4"
lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "first"
if lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "second"; then
	echo "FAIL a second acquire must fail while the first is held" >&2
	fails=$((fails + 1))
else
	echo "ok   a second acquire fails while the first is held"
fi
# The load-bearing half: the loser must not have overwritten the winner's
# record, or the winner's own release would find someone else's lease.
check "the loser leaves the winner's record intact" "first" \
	"$(lease_field "$(lease_file)" rc)"
check "no temp record is left behind" "1" \
	"$(find "${RELEASE_LEASE_DIR}" -type f | wc -l | tr -d ' ')"

# --- the orphan: the owning process is gone ----------------------------------

# The Q640 state exactly — the gate was killed, so nothing released the lease.
reset_leases
LIVE_PIDS[$$]="bash scripts/dogfood/validate-release.sh v1.3.0-rc.4"
lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "v1.3.0-rc.4"
unset "LIVE_PIDS[$$]"
check "an owner that no longer exists reads orphaned" "orphaned" "$(state)"

# A recycled pid is a live process that is not the gate. Liveness alone would
# read it as held and leave the nodes billing forever; the marker is what
# separates them.
reset_leases
write_lease 4242 "test-host" "${PROJECT}/${ZONE}/${CLUSTER}"
LIVE_PIDS[4242]="/usr/sbin/cupsd -l"
check "a recycled pid reads orphaned, not held" "orphaned" "$(state)"
LIVE_PIDS[4242]="bash scripts/dogfood/validate-release.sh v1.3.0-rc.4"
check "the same pid still running the gate reads held" "held" "$(state)"

# --- another host is reported, never reclaimed -------------------------------

reset_leases
write_lease 4242 "someone-elses-mac" "${PROJECT}/${ZONE}/${CLUSTER}"
check "a record from another host reads foreign" "foreign" "$(state)"

# A record whose target is not the one being asked about is equally unjudgeable
# — sanitizing two targets to one filename must not let one speak for the other.
reset_leases
write_lease 4242 "test-host" "other-proj/${ZONE}/${CLUSTER}"
check "a record for another target reads foreign" "foreign" "$(state)"

# A truncated record has no host, so it cannot be attributed — report, never
# act. (`ln` of a fully written file means this should be unreachable; the
# fail-safe direction is asserted anyway because the cost of the other one is a
# wrongly deleted cluster.)
reset_leases
mkdir -p "${RELEASE_LEASE_DIR}"
: >"$(lease_file)"
check "an unattributable record reads foreign" "foreign" "$(state)"

# --- release never clears a lease this process does not own ------------------

reset_leases
write_lease 4242 "test-host" "${PROJECT}/${ZONE}/${CLUSTER}"
LIVE_PIDS[4242]="bash scripts/dogfood/validate-release.sh v1.3.0-rc.4"
lease_release "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "release leaves another process's lease alone" "held" "$(state)"
# discard is the reclaim path's tool and is unconditional by design — it runs
# only after that owner's cluster has actually been torn down.
lease_discard "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "discard clears a lease regardless of owner" "free" "$(state)"

# --- one lease per target ----------------------------------------------------

reset_leases
LIVE_PIDS[$$]="bash scripts/dogfood/validate-release.sh v1.3.0-rc.4"
lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "a lease on one cluster leaves another free" "free" \
	"$(lease_state other-proj "${ZONE}" other-cluster)"
if lease_acquire other-proj "${ZONE}" other-cluster; then
	echo "ok   a second cluster can be acquired independently"
else
	echo "FAIL a lease on one cluster must not block another" >&2
	fails=$((fails + 1))
fi

# --- the operator-facing description ----------------------------------------

reset_leases
write_lease 4242 "test-host" "${PROJECT}/${ZONE}/${CLUSTER}"
RELEASE_LEASE_NOW="$(($(date +%s) + 5400))"
desc="$(lease_describe "${PROJECT}" "${ZONE}" "${CLUSTER}")"
unset RELEASE_LEASE_NOW
check_contains "the description names the owning pid" "pid 4242" "${desc}"
check_contains "the description names the RC" "v9.9.9-rc.1" "${desc}"
# How long it has been leaking is the number that decides how urgent this is.
check_contains "the description ages the lease" "1h30m ago" "${desc}"
check_contains "the description names the file to delete" "$(lease_file)" "${desc}"

# --- lease_field parses a value containing its own separator -----------------

reset_leases
mkdir -p "${RELEASE_LEASE_DIR}"
printf 'pid=7\nmarker=bash a=b.sh\n' >"$(lease_file)"
check "a value may contain the separator" "bash a=b.sh" \
	"$(lease_field "$(lease_file)" marker)"
check "an absent key reads empty" "" "$(lease_field "$(lease_file)" nope)"

if ((fails > 0)); then
	echo "lease-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "lease-test: ok"
