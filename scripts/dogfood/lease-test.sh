#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/lib/lease.sh: the ownership lease that lets the
# release gate tell an orphaned run from a cluster somebody is using (Q640), on
# any host that can reach the cluster (Q1158).
#
# Why it is tested: the lease is the sole trigger for a destructive teardown of a
# prod-classified GKE cluster, so every state it can report is a decision about
# whether to delete somebody's environment. Both directions are silent and both
# are expensive. Reading a live gate — or a hand-run debugging session — as
# orphaned tears down work in progress, which is strictly worse than the leak
# this mechanism exists to stop. Reading a dead gate as live leaves the leak in
# place, unfixed and now believed fixed.
#
# The hard cases are pid reuse, another host, and an unreadable record: a
# recycled pid is a live process that is NOT the gate (reclaiming is correct),
# another host's pid means nothing here so only its renewal can be judged, and a
# failed read must never read as free. Each is asserted below, along with the
# compare-and-swap that decides a two-gate race and a two-reclaimer one.
#
# No cluster: lib/lease-fake.sh replaces the four API primitives, and
# `lease_process_command` is stubbed, so a pid is whatever the test says it is.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# Set before sourcing: the default resolves under $HOME at source time, and no
# test may write there.
RELEASE_LEASE_DIR="${WORKDIR}/lease"
export RELEASE_LEASE_DIR
# shellcheck source=scripts/dogfood/lib/lease.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease.sh"
# shellcheck source=scripts/dogfood/lib/lease-fake.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease-fake.sh"

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
GATE_CMD="bash scripts/dogfood/validate-release.sh v1.3.0-rc.4"

reset_leases() {
	rm -rf "${RELEASE_LEASE_DIR}"
	LIVE_PIDS=()
	HOSTNAME="test-host"
	unset LEASE_FAKE_READ_FAILS LEASE_FAKE_WRITE_FAILS RELEASE_LEASE_NOW
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

check_ok() {
	local name="$1"
	shift
	if "$@"; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}" >&2
		fails=$((fails + 1))
	fi
}

check_fails() {
	local name="$1"
	shift
	if "$@"; then
		echo "FAIL ${name}" >&2
		fails=$((fails + 1))
	else
		echo "ok   ${name}"
	fi
}

state() { lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}"; }
ownership() { lease_ownership "${PROJECT}" "${ZONE}" "${CLUSTER}"; }
record() { cat "$(lease_fake_path "${PROJECT}" "${ZONE}" "${CLUSTER}")" 2>/dev/null || true; }
holder() { record | cut -d'|' -f1; }
field_rc() { record | cut -d'|' -f5; }

# iso_ago SECONDS — a Lease MicroTime SECONDS before RELEASE_LEASE_NOW (or now).
iso_ago() {
	local t=$((${RELEASE_LEASE_NOW:-$(date +%s)} - $1))
	date -u -r "${t}" +%Y-%m-%dT%H:%M:%S.000000Z 2>/dev/null ||
		date -u -d "@${t}" +%Y-%m-%dT%H:%M:%S.000000Z
}

# remote_lease RENEWED_AGO [HOLDER] — a record another host renewed that long ago.
remote_lease() {
	lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "${2:-ci-runner-7/4242}" \
		"$(iso_ago "$1")" 600 "$(iso_ago 5400)" v9.9.9-rc.1
}

echo "scripts/dogfood/lease-test.sh"

# --- an unclaimed target is free, and free costs nothing ---------------------

reset_leases
check "no lease reads free" "free" "$(state)"
check "no lease is not owned" "none" "$(ownership)"

# --- an unreadable lease is never free ---------------------------------------

# The case a host-local file never had: the record is in the cluster, and an API
# that does not answer must not read as "nobody holds it", or a CI run with bad
# credentials would start over a live local gate.
reset_leases
remote_lease 10
LEASE_FAKE_READ_FAILS=1
check "a failed read reads unknown, not free" "unknown" "$(state)"
check "a failed read is unknown ownership" "unknown" "$(ownership)"
check_contains "a failed read is described as unreadable" "could not read" \
	"$(lease_describe "${PROJECT}" "${ZONE}" "${CLUSTER}")"
unset LEASE_FAKE_READ_FAILS

# --- acquire, hold, release --------------------------------------------------

reset_leases
LIVE_PIDS[$$]="${GATE_CMD}"
check_ok "an unclaimed target can be acquired" \
	lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "v1.3.0-rc.4"
check "a live owner reads held" "held" "$(state)"
check "the lease records this host and process" "test-host/$$" "$(holder)"
check "the lease records the RC" "v1.3.0-rc.4" "$(field_rc)"
check "the lease is this process's" "mine" "$(ownership)"

lease_release "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "releasing an owned lease frees the target" "free" "$(state)"

# --- the two-gate race: create decides, and the loser changes nothing --------

reset_leases
LIVE_PIDS[$$]="${GATE_CMD}"
lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "first"
check_fails "a second acquire fails while the first is held" \
	lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "second"
# The load-bearing half: the loser must not have overwritten the winner's
# record, or the winner's own release would find someone else's lease.
check "the loser leaves the winner's record intact" "first" "$(field_rc)"

# --- same host: the orphan is judged by the pid, at once ---------------------

# The Q640 state exactly — the gate was killed, so nothing released the lease.
# No wait for expiry: this is what CI's same-runner reclaim step relies on.
reset_leases
LIVE_PIDS[$$]="${GATE_CMD}"
lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" "v1.3.0-rc.4"
unset "LIVE_PIDS[$$]"
check "a same-host owner that no longer exists reads orphaned" "orphaned" "$(state)"

# A recycled pid is a live process that is not the gate. Liveness alone would
# read it as held and leave the nodes billing forever; the marker is what
# separates them.
reset_leases
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "test-host/4242" \
	"$(iso_ago 5)" 600 "$(iso_ago 5)" v9.9.9-rc.1
LIVE_PIDS[4242]="/usr/sbin/cupsd -l"
check "a recycled pid reads orphaned, not held" "orphaned" "$(state)"
LIVE_PIDS[4242]="${GATE_CMD}"
check "the same pid still running the gate reads held" "held" "$(state)"

# --- another host: judged by renewal, never by the pid -----------------------

reset_leases
remote_lease 30
check "another host's freshly renewed lease reads held" "held" "$(state)"
check "another host's lease is not this process's" "lost" "$(ownership)"
# The pid in another host's record must never be looked up here: a live local
# process that happens to share the number says nothing about that host.
LIVE_PIDS[4242]="${GATE_CMD}"
remote_lease 601
check "another host's lapsed lease reads orphaned whatever runs locally at its pid" \
	"orphaned" "$(state)"
LIVE_PIDS=()
# One clock reading for both sides: a second ticking over between them is 601.
RELEASE_LEASE_NOW="$(date +%s)"
remote_lease 600
check "a lease renewed exactly one duration ago still reads held" "held" "$(state)"
unset RELEASE_LEASE_NOW

# The window that matters is the lost runner: Q880's own reclaim step never ran,
# and this is the only way anyone can see the run is gone.
reset_leases
remote_lease 3600 "runnervm-gone/1999"
check "a vanished runner's lease reads orphaned once it lapses" "orphaned" "$(state)"

# --- an unattributable record is reported, never acted on --------------------

reset_leases
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "" "$(iso_ago 9999)" 600 "" ""
check "a lease with no holder reads foreign" "foreign" "$(state)"
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "kube-controller-manager" "$(iso_ago 9999)" 600 "" ""
check "a holder that is not host/pid reads foreign" "foreign" "$(state)"
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "ci-runner-7/4242" "yesterday" 600 "" ""
check "another host's unparseable renewal reads foreign" "foreign" "$(state)"
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "ci-runner-7/4242" "$(iso_ago 9999)" "" "" ""
check "another host's lease with no duration reads foreign" "foreign" "$(state)"

# --- release never clears a lease this process does not hold -----------------

reset_leases
remote_lease 30
lease_release "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "release leaves another holder's lease alone" "ci-runner-7/4242" "$(holder)"

# --- takeover: the reclaim claims the orphan before tearing anything down ----

reset_leases
remote_lease 3600
check_ok "an orphaned lease can be taken over" lease_takeover "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "the takeover records this process" "test-host/$$" "$(holder)"
check "the takeover marks the record as a reclaim" "reclaim" "$(field_rc)"
check "a taken-over lease is this process's" "mine" "$(ownership)"
lease_release "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "a reclaimer releases what it took over" "free" "$(state)"

# Two reclaimers read the same orphan; the second swap must fail, or both tear
# down and the loser could tear down a gate that started after the winner.
reset_leases
remote_lease 3600 "runnervm-gone/1999"
# shellcheck disable=SC2329 # invoked by lease_takeover
lease_read() {
	LEASE_HOLDER="runnervm-gone/1999"
	return 0
}
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "other-reclaimer/77" "$(iso_ago 1)" 600 "$(iso_ago 1)" reclaim
check_fails "a takeover from a holder that has since changed fails" \
	lease_takeover "${PROJECT}" "${ZONE}" "${CLUSTER}"
unset -f lease_read
# shellcheck source=scripts/dogfood/lib/lease.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease.sh"
# shellcheck source=scripts/dogfood/lib/lease-fake.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease-fake.sh"
lease_process_command() { echo "${LIVE_PIDS[$1]:-}"; }
check "the losing takeover leaves the winner's record intact" "other-reclaimer/77" "$(holder)"

# A reclaim judges the lease orphaned, then waits at a confirmation prompt for
# as long as the operator takes. An owner that wakes and renews meanwhile keeps
# its holder, so a swap on the holder alone would still take a live run over.
reset_leases
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "laptop/4242" "$(iso_ago 3600)" 600 "$(iso_ago 5400)" v9.9.9-rc.1
check "the sleeping laptop's lease reads orphaned" "orphaned" "$(state)"
lease_api_patch "${PROJECT}" "${ZONE}" "${CLUSTER}" "laptop/4242" "laptop/4242"
check_fails "a takeover after the owner renewed fails" \
	lease_takeover "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "the renewed owner keeps its lease" "laptop/4242" "$(holder)"

# The same renewal landing between the takeover's own read and its write: the
# re-judge saw a lapsed record, so only the renewTime test can catch it.
reset_leases
stale="$(iso_ago 3600)"
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "laptop/4242" "${stale}" 600 "$(iso_ago 5400)" v9.9.9-rc.1
# shellcheck disable=SC2329 # invoked by lease_takeover
lease_read() {
	LEASE_HOLDER="laptop/4242" LEASE_RENEWED="${stale}" LEASE_DURATION=600
	lease_api_patch "${PROJECT}" "${ZONE}" "${CLUSTER}" "laptop/4242" "laptop/4242"
	return 0
}
check_fails "a renewal between the takeover's read and write fails the swap" \
	lease_takeover "${PROJECT}" "${ZONE}" "${CLUSTER}"
unset -f lease_read
# shellcheck source=scripts/dogfood/lib/lease.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease.sh"
# shellcheck source=scripts/dogfood/lib/lease-fake.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease-fake.sh"
lease_process_command() { echo "${LIVE_PIDS[$1]:-}"; }
check "the racing owner keeps its lease" "laptop/4242" "$(holder)"

# --- renewal ----------------------------------------------------------------

reset_leases
LIVE_PIDS[$$]="${GATE_CMD}"
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" "test-host/$$" "$(iso_ago 300)" 600 "$(iso_ago 300)" v1.3.0-rc.4
check "renewing a held lease reports renewed" "renewed" "$(lease_renew_once "${PROJECT}" "${ZONE}" "${CLUSTER}")"
renewed="$(lease_epoch "$(record | cut -d'|' -f2)")"
check "renewal moves renewTime to now" "1" "$(($(date +%s) - renewed < 5 ? 1 : 0))"
check "renewal keeps the RC" "v1.3.0-rc.4" "$(field_rc)"

remote_lease 1 "ci-runner-7/4242"
check "renewing a lease another holder took reports lost" "lost" \
	"$(lease_renew_once "${PROJECT}" "${ZONE}" "${CLUSTER}")"
check "a lost renewal leaves the new holder's record" "ci-runner-7/4242" "$(holder)"

rm -f "$(lease_fake_path "${PROJECT}" "${ZONE}" "${CLUSTER}")"
check "renewing a deleted lease reports lost" "lost" \
	"$(lease_renew_once "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# A failed write while this process is still recorded is an API blip, and a
# failed read cannot tell: both retry rather than stopping a healthy gate.
lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" v1.3.0-rc.4
LEASE_FAKE_WRITE_FAILS=1
check "a failed write with this process recorded reports error" "error" \
	"$(lease_renew_once "${PROJECT}" "${ZONE}" "${CLUSTER}")"
LEASE_FAKE_READ_FAILS=1
check "a failed write and read report error, not lost" "error" \
	"$(lease_renew_once "${PROJECT}" "${ZONE}" "${CLUSTER}")"
unset LEASE_FAKE_WRITE_FAILS LEASE_FAKE_READ_FAILS

# --- the background renewer --------------------------------------------------
#
# Driven in a child bash that plays the gate, since the renewer signals its
# owner and this suite must not be that owner.

# run_owner SCRIPT — run SCRIPT as a gate process with the lease libs loaded.
run_owner() {
	RELEASE_LEASE_RENEW_INTERVAL=0.2 HOSTNAME=owner-host \
		bash -c "set -euo pipefail
		source '${REPO_ROOT}/scripts/dogfood/lib/lease.sh'
		source '${REPO_ROOT}/scripts/dogfood/lib/lease-fake.sh'
		$1"
}

# A renewer that finds another holder stops its gate, which is what keeps a gate
# whose renewals lapsed from carrying on over its successor's cluster.
reset_leases
run_owner "lease_acquire '${PROJECT}' '${ZONE}' '${CLUSTER}' rc
	lease_renew_start '${PROJECT}' '${ZONE}' '${CLUSTER}' stop-owner
	sleep 10
	echo survived" >"${WORKDIR}/owner.out" 2>&1 &
owner=$!
for _ in $(seq 50); do
	[[ "$(holder)" == owner-host/* ]] && break
	sleep 0.1
done
remote_lease 1 "ci-runner-7/4242"
owner_rc=0
wait "${owner}" || owner_rc=$?
check "a renewer that finds another holder TERMs its gate" "143" "${owner_rc}"
check_contains "the renewer says why it stopped the gate" "another holder took" \
	"$(cat "${WORKDIR}/owner.out")"
check "the stopped gate leaves the new holder's record" "ci-runner-7/4242" "$(holder)"

# A renewer started for a teardown or a reclaim must not signal: its owner is
# already running stop scripts, and a TERM there kills bash mid-trap while the
# stop script carries on. It stops renewing and leaves its owner alone.
reset_leases
run_owner "lease_acquire '${PROJECT}' '${ZONE}' '${CLUSTER}' rc
	lease_renew_start '${PROJECT}' '${ZONE}' '${CLUSTER}'
	echo \"\${LEASE_RENEWER_PID}\" >'${WORKDIR}/quiet.pid'
	sleep 2
	echo survived" >"${WORKDIR}/quiet.out" 2>&1 &
owner=$!
for _ in $(seq 50); do
	[[ "$(holder)" == owner-host/* ]] && break
	sleep 0.1
done
remote_lease 1 "ci-runner-7/4242"
owner_rc=0
wait "${owner}" || owner_rc=$?
check "a renewer without stop-owner leaves its owner running" "0" "${owner_rc}"
check_contains "the owner ran to completion" "survived" "$(cat "${WORKDIR}/quiet.out")"
quiet="$(cat "${WORKDIR}/quiet.pid")"
kill -0 "${quiet}" 2>/dev/null && quiet_alive=1 || quiet_alive=0
check "a renewer that lost its lease stops renewing" "0" "${quiet_alive}"
((quiet_alive == 0)) || kill "${quiet}" 2>/dev/null || true

# A renewer outliving a SIGKILLed gate would keep a dead run's lease fresh
# forever, and no other host could ever reclaim it.
# The pid goes through a file, so a renewer that does outlive its gate fails
# the check below instead of holding a captured stdout open forever.
reset_leases
run_owner "lease_acquire '${PROJECT}' '${ZONE}' '${CLUSTER}' rc
	lease_renew_start '${PROJECT}' '${ZONE}' '${CLUSTER}'
	echo \"\${LEASE_RENEWER_PID}\" >'${WORKDIR}/renewer.pid'" >/dev/null 2>&1
renewer="$(cat "${WORKDIR}/renewer.pid")"
gone=0
for _ in $(seq 30); do
	kill -0 "${renewer}" 2>/dev/null || {
		gone=1
		break
	}
	sleep 0.1
done
check "the renewer exits once its gate is gone" "1" "${gone}"
((gone)) || kill "${renewer}" 2>/dev/null || true

# --- the patch body ----------------------------------------------------------

body="$(lease_patch_body "a/1" "a/1" "2026-10-04T20:00:00.000000Z")"
check "a renewal patch is valid JSON" "3" "$(jq length <<<"${body}")"
check "every patch tests the holder first" "test /spec/holderIdentity a/1" \
	"$(jq -r '.[0] | "\(.op) \(.path) \(.value)"' <<<"${body}")"
body="$(lease_patch_body "a/1" "b/2" "2026-10-04T20:00:00.000000Z" 'rc"x')"
check "a takeover patch stamps acquireTime and the RC" "5" "$(jq length <<<"${body}")"
check "the RC annotation key is pointer-escaped" "/metadata/annotations/actions-gateway.com~1release-gate-rc" \
	"$(jq -r '.[4].path' <<<"${body}")"
check "a quote in a value survives escaping" 'rc"x' "$(jq -r '.[4].value' <<<"${body}")"
body="$(lease_patch_body "a/1" "b/2" "2026-10-04T20:00:00.000000Z" rc "2026-10-04T19:00:00.000000Z")"
check "a takeover tests the renewTime it read, second" "test /spec/renewTime 2026-10-04T19:00:00.000000Z" \
	"$(jq -r '.[1] | "\(.op) \(.path) \(.value)"' <<<"${body}")"

# --- time parsing ------------------------------------------------------------

check "a Lease MicroTime parses to its epoch" "1791144000" "$(lease_epoch 2026-10-04T20:00:00.000000Z)"
check "a MicroTime without a fraction parses too" "1791144000" "$(lease_epoch 2026-10-04T20:00:00Z)"
check "an unparseable time is empty" "" "$(lease_epoch yesterday)"

# --- one lease per target ----------------------------------------------------

reset_leases
LIVE_PIDS[$$]="${GATE_CMD}"
lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}"
check "a lease on one cluster leaves another free" "free" \
	"$(lease_state other-proj "${ZONE}" other-cluster)"
check_ok "a second cluster can be acquired independently" \
	lease_acquire other-proj "${ZONE}" other-cluster

# --- the operator-facing description ----------------------------------------

reset_leases
remote_lease 120
desc="$(lease_describe "${PROJECT}" "${ZONE}" "${CLUSTER}")"
check_contains "the description names the holder" "ci-runner-7/4242" "${desc}"
check_contains "the description names the RC" "v9.9.9-rc.1" "${desc}"
# How long it has been leaking is the number that decides how urgent this is.
check_contains "the description ages the lease" "acquired 1h30m ago" "${desc}"
check_contains "the description ages the last renewal" "renewed 0h02m ago" "${desc}"
check_contains "the description names the command to inspect it" \
	"kubectl -n default get lease actions-gateway-release-gate" "${desc}"

if ((fails > 0)); then
	echo "lease-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "lease-test: ok"
