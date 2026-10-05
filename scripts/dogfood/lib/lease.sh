# Ownership lease for the dogfood release-validation gate. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/dogfood/lib/lease.sh
#   source "$REPO_ROOT/scripts/dogfood/lib/lease.sh"
#
# validate-release.sh self-cleans through `trap teardown EXIT`, and bash runs
# that trap on TERM, INT and HUP as well as on an ordinary exit (measured on
# bash 5.3). SIGKILL cannot be trapped at all, a killed parent takes the whole
# process group with it, a teardown that is itself killed part-way through stops
# between the two stop scripts, and a CI runner can vanish mid-run. Every one of
# those leaves the same state: no process, and a cluster still billing (Q640).
#
# The lease is the out-of-process record that makes that state legible. The gate
# holds one for exactly the window in which it owns billable cluster state —
# taken before the first scale-up, released last in teardown, after the stop
# scripts — so a lease whose owner is gone IS an orphaned run, and nothing else
# is. A cluster that merely has nodes up is never evidence of an orphan: an
# operator debugging by hand leaves exactly that state. No lease, no reclaim.
#
# The record is a coordination.k8s.io Lease in the target cluster itself, so a
# maintainer's machine and a CI runner read the same one (Q1158). The control
# plane answers with every node pool at zero, nothing the gate tears down
# deletes a Lease, and deleting the cluster deletes the record with the billing
# it described. Writes are compare-and-swap: create fails when one exists, and
# every update is a JSON patch whose first op tests the holder it expects.
#
# Who owns it is judged two ways, because a pid means nothing off its host:
#
#   same host   the holder's pid, as before: alive and still running the gate
#               is held, anything else is orphaned. Immediate, so the reclaim a
#               CI job runs on its own runner needs no wait.
#   other host  renewal. The owner renews every RELEASE_LEASE_RENEW_INTERVAL;
#               a renewTime older than leaseDurationSeconds is orphaned.
#
# Renewal can misjudge a live owner whose renewer stalled (a laptop asleep), and
# the gate fences against that rather than trusting the clock: an owner that
# finds another holder on its lease stops, and its teardown touches nothing.
# docs/operations/release.md § "Why another host's lease expires" has the trade.
# shellcheck shell=bash

# RELEASE_LEASE_DIR — host-local state: the kubeconfig the lease's kubectl calls
# use, kept apart from the operator's so a lookup never switches their context.
# Tests point this at their own scratch dir.
RELEASE_LEASE_DIR="${RELEASE_LEASE_DIR:-${XDG_STATE_HOME:-${HOME}/.local/state}/github-actions-gateway}"

# RELEASE_LEASE_MARKER — a substring a same-host owner's command line must still
# contain for the lease to count as held. See lease_owner_alive.
RELEASE_LEASE_MARKER="${RELEASE_LEASE_MARKER:-validate-release.sh}"

# Where the Lease lives. `default` because nothing the gate deletes reaches it:
# teardown deletes the e2e tenant, whose namespaces the GMC owns.
RELEASE_LEASE_NAMESPACE="${RELEASE_LEASE_NAMESPACE:-default}"
RELEASE_LEASE_NAME="${RELEASE_LEASE_NAME:-actions-gateway-release-gate}"

# Ten missed renewals before another host may reclaim. Long enough to ride out
# API blips and a short suspend; short against a leak billed by the hour.
RELEASE_LEASE_DURATION="${RELEASE_LEASE_DURATION:-600}"
RELEASE_LEASE_RENEW_INTERVAL="${RELEASE_LEASE_RENEW_INTERVAL:-60}"

LEASE_RC_ANNOTATION="actions-gateway.com/release-gate-rc"

# lease_host — this host's name. A function so a test can model two hosts.
lease_host() { echo "${HOSTNAME:-$(hostname)}"; }

# lease_holder — this process's holder identity, host/pid. A subshell keeps the
# parent's $$, so the renewer renews on the gate's behalf.
lease_holder() { echo "$(lease_host)/$$"; }

# lease_target PROJECT ZONE CLUSTER — the canonical target string.
lease_target() { echo "$1/$2/$3"; }

# lease_now_iso — the current time as a Lease MicroTime.
lease_now_iso() { date -u +%Y-%m-%dT%H:%M:%S.000000Z; }

# lease_epoch TIME — seconds since the epoch for a Lease MicroTime, empty when it
# does not parse. GNU date first (CI), then BSD (macOS).
lease_epoch() {
	local t="${1%%.*}" out
	t="${t%Z}"
	[[ "${t}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}$ ]] || return 0
	out="$(date -u -d "${t}Z" +%s 2>/dev/null)" ||
		out="$(date -j -u -f '%Y-%m-%dT%H:%M:%S' "${t}" +%s 2>/dev/null)" || out=""
	[[ "${out}" =~ ^[0-9]+$ ]] && echo "${out}"
	return 0
}

# lease_json_string VALUE — VALUE as a JSON string literal.
lease_json_string() {
	local s="${1//\\/\\\\}"
	printf '"%s"' "${s//\"/\\\"}"
}

# --- The four API primitives. Tests replace exactly these. ------------------

# lease_kubectl PROJECT ZONE CLUSTER ARGS... — kubectl against the target, by a
# kubeconfig of its own. Credentials are fetched once per gate process; the
# owner file records which process fetched them.
lease_kubectl() {
	local project="$1" zone="$2" cluster="$3"
	shift 3
	local key="${project}-${zone}-${cluster}"
	local cfg="${RELEASE_LEASE_DIR}/lease-${key//[^A-Za-z0-9._-]/-}.kubeconfig"
	if [[ "$(cat "${cfg}.owner" 2>/dev/null)" != "$$" ]]; then
		mkdir -p "${RELEASE_LEASE_DIR}" 2>/dev/null || return 1
		KUBECONFIG="${cfg}" gcloud container clusters get-credentials "${cluster}" \
			--project="${project}" --zone="${zone}" >/dev/null 2>&1 || return 1
		echo "$$" >"${cfg}.owner"
	fi
	kubectl --kubeconfig "${cfg}" --context "gke_${project}_${zone}_${cluster}" \
		-n "${RELEASE_LEASE_NAMESPACE}" "$@"
}

# lease_api_get PROJECT ZONE CLUSTER — print holder|renewTime|duration|acquireTime|rc
# for the Lease, nothing when there is none. Non-zero means the read failed,
# which is never the same answer as no lease.
lease_api_get() {
	lease_kubectl "$1" "$2" "$3" get lease "${RELEASE_LEASE_NAME}" --ignore-not-found \
		-o jsonpath='{.spec.holderIdentity}{"|"}{.spec.renewTime}{"|"}{.spec.leaseDurationSeconds}{"|"}{.spec.acquireTime}{"|"}{.metadata.annotations.actions-gateway\.com/release-gate-rc}'
}

# lease_api_create PROJECT ZONE CLUSTER HOLDER RC — create the Lease. Fails when
# one already exists, which is what decides a two-gate race.
lease_api_create() {
	local now
	now="$(lease_now_iso)"
	lease_kubectl "$1" "$2" "$3" create -f - >/dev/null 2>&1 <<EOF
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: ${RELEASE_LEASE_NAME}
  namespace: ${RELEASE_LEASE_NAMESPACE}
  annotations:
    ${LEASE_RC_ANNOTATION}: $(lease_json_string "${5:-}")
spec:
  holderIdentity: $(lease_json_string "$4")
  leaseDurationSeconds: ${RELEASE_LEASE_DURATION}
  acquireTime: "${now}"
  renewTime: "${now}"
EOF
}

# lease_patch_body EXPECT NEW NOW [RC] [EXPECT_RENEWED] — the JSON patch that
# moves the holder from EXPECT to NEW. The test ops make the whole patch fail
# unless EXPECT still holds it and, given EXPECT_RENEWED, has not renewed since.
# EXPECT == NEW is a renewal; anything else is a takeover, which also stamps
# acquireTime and the RC.
lease_patch_body() {
	local expect="$1" new="$2" now="$3" rc="${4:-}" renewed="${5:-}"
	printf '[{"op":"test","path":"/spec/holderIdentity","value":%s}' "$(lease_json_string "${expect}")"
	[[ -z "${renewed}" ]] ||
		printf ',{"op":"test","path":"/spec/renewTime","value":%s}' "$(lease_json_string "${renewed}")"
	printf ',{"op":"replace","path":"/spec/holderIdentity","value":%s}' "$(lease_json_string "${new}")"
	printf ',{"op":"add","path":"/spec/renewTime","value":%s}' "$(lease_json_string "${now}")"
	if [[ "${expect}" != "${new}" ]]; then
		printf ',{"op":"add","path":"/spec/acquireTime","value":%s}' "$(lease_json_string "${now}")"
		printf ',{"op":"add","path":"/metadata/annotations/%s","value":%s}' \
			"${LEASE_RC_ANNOTATION//\//~1}" "$(lease_json_string "${rc}")"
	fi
	printf ']'
}

# lease_api_patch PROJECT ZONE CLUSTER EXPECT NEW [RC] [EXPECT_RENEWED] —
# compare-and-swap the holder. Fails, changing nothing, when EXPECT no longer
# holds the Lease or, given EXPECT_RENEWED, has renewed it since.
lease_api_patch() {
	lease_kubectl "$1" "$2" "$3" patch lease "${RELEASE_LEASE_NAME}" --type=json \
		-p "$(lease_patch_body "$4" "$5" "$(lease_now_iso)" "${6:-}" "${7:-}")" >/dev/null 2>&1
}

# lease_api_delete PROJECT ZONE CLUSTER — delete the Lease.
lease_api_delete() {
	lease_kubectl "$1" "$2" "$3" delete lease "${RELEASE_LEASE_NAME}" \
		--ignore-not-found >/dev/null 2>&1
}

# --- Reading ----------------------------------------------------------------

# LEASE_HOLDER, LEASE_RENEWED, LEASE_DURATION, LEASE_ACQUIRED, LEASE_RC — set by
# lease_read from the last successful read.
LEASE_HOLDER="" LEASE_RENEWED="" LEASE_DURATION="" LEASE_ACQUIRED="" LEASE_RC=""

# lease_read PROJECT ZONE CLUSTER — load the Lease into the LEASE_* globals.
# Returns 0 with a holder, 1 when there is no Lease, 2 when the read failed.
lease_read() {
	local out
	out="$(lease_api_get "$1" "$2" "$3")" || return 2
	LEASE_HOLDER="" LEASE_RENEWED="" LEASE_DURATION="" LEASE_ACQUIRED="" LEASE_RC=""
	[[ -n "${out}" ]] || return 1
	IFS='|' read -r LEASE_HOLDER LEASE_RENEWED LEASE_DURATION LEASE_ACQUIRED LEASE_RC <<<"${out}"
	return 0
}

# lease_process_command PID — the command line of PID, empty when no such
# process. A function so a test can model a dead, a live, or a recycled pid.
lease_process_command() { ps -o command= -p "$1" 2>/dev/null || true; }

# lease_owner_alive PID MARKER — true when PID is a live process whose command
# line still contains MARKER.
#
# `ps` rather than `kill -0`: kill reports another user's process as EPERM,
# which reads as dead, and bare liveness cannot tell the gate apart from
# whatever recycled its pid after it died. Requiring the marker settles both —
# a recycled pid means the gate IS gone, so failing the match is the right
# answer. Both error directions are safe: a false "alive" refuses to start
# (costs a re-run), never a false teardown.
lease_owner_alive() {
	local pid="$1" marker="$2" cmd
	[[ "${pid}" =~ ^[0-9]+$ ]] || return 1
	cmd="$(lease_process_command "${pid}")"
	[[ -n "${cmd}" ]] || return 1
	[[ -z "${marker}" || "${cmd}" == *"${marker}"* ]]
}

# lease_state PROJECT ZONE CLUSTER — print what this target's lease says:
#
#   free      no lease — no gate owns this cluster, and nothing is reclaimable.
#   held      a live gate owns it. Refuse to start; never touch the cluster.
#   orphaned  a gate owned it and is gone. Reclaimable.
#   foreign   a record this gate cannot judge (no holder, no parseable renewal).
#             Reported, never acted on.
#   unknown   the read failed. Refuse, and touch nothing: an unreadable lease
#             is not a free one.
lease_state() {
	local rc=0
	lease_read "$1" "$2" "$3" || rc=$?
	case "${rc}" in
	1)
		echo free
		return 0
		;;
	2)
		echo unknown
		return 0
		;;
	esac
	lease_judge
}

# lease_judge — held, orphaned or foreign for the record lease_read last loaded.
lease_judge() {
	local host="${LEASE_HOLDER%/*}" pid="${LEASE_HOLDER##*/}"
	if [[ -z "${host}" || "${host}" == "${LEASE_HOLDER}" || ! "${pid}" =~ ^[0-9]+$ ]]; then
		echo foreign
		return 0
	fi
	if [[ "${host}" == "$(lease_host)" ]]; then
		if lease_owner_alive "${pid}" "${RELEASE_LEASE_MARKER}"; then
			echo held
		else
			echo orphaned
		fi
		return 0
	fi
	local renewed now="${RELEASE_LEASE_NOW:-$(date +%s)}"
	renewed="$(lease_epoch "${LEASE_RENEWED}")"
	if [[ -z "${renewed}" || ! "${LEASE_DURATION}" =~ ^[0-9]+$ ]]; then
		echo foreign
	elif ((now - renewed > LEASE_DURATION)); then
		echo orphaned
	else
		echo held
	fi
}

# lease_ownership PROJECT ZONE CLUSTER — whether this process still holds the
# lease: mine, none (no lease at all), lost (another holder is recorded) or
# unknown (the read failed). Only `lost` is positive evidence of a successor.
lease_ownership() {
	local rc=0
	lease_read "$1" "$2" "$3" || rc=$?
	case "${rc}" in
	1) echo none ;;
	2) echo unknown ;;
	*) [[ "${LEASE_HOLDER}" == "$(lease_holder)" ]] && echo mine || echo lost ;;
	esac
}

# --- Writing ----------------------------------------------------------------

# lease_acquire PROJECT ZONE CLUSTER [RC] — claim a free target for this process.
# Returns 1 when a lease already exists (or the API refused), having changed
# nothing.
lease_acquire() {
	lease_api_create "$1" "$2" "$3" "$(lease_holder)" "${4:-}"
}

# lease_takeover PROJECT ZONE CLUSTER — claim an orphaned target before reclaiming
# it, so two reclaimers cannot both tear down and the loser cannot tear down a
# gate that started after the winner. Re-reads and re-judges rather than trusting
# the caller's earlier verdict, which a confirmation prompt can leave minutes
# stale, then swaps on the holder AND renewTime just read: a renewal between the
# read and the write changes renewTime and fails the swap. Returns 1, changing
# nothing, unless the lease is still orphaned and unchanged.
lease_takeover() {
	lease_read "$1" "$2" "$3" || return 1
	[[ "$(lease_judge)" == orphaned ]] || return 1
	lease_api_patch "$1" "$2" "$3" "${LEASE_HOLDER}" "$(lease_holder)" reclaim "${LEASE_RENEWED}"
}

# lease_renew_once PROJECT ZONE CLUSTER — renew this process's lease, printing
# renewed, lost (another holder, or the lease is gone) or error (a failed write
# with this process still recorded, or a failed read: retry next interval).
lease_renew_once() {
	if lease_api_patch "$1" "$2" "$3" "$(lease_holder)" "$(lease_holder)"; then
		echo renewed
		return 0
	fi
	case "$(lease_ownership "$1" "$2" "$3")" in
	mine | unknown) echo error ;;
	*) echo lost ;;
	esac
}

# LEASE_RENEWER_PID — the background renewer lease_renew_start launched.
LEASE_RENEWER_PID=""

# lease_renew_start PROJECT ZONE CLUSTER [stop-owner] — renew in the background
# for as long as this process lives. With stop-owner, finding the lease lost
# TERMs this process, whose teardown then sees `lost` and leaves the cluster to
# its new owner. Without it the renewer only stops renewing: a teardown or a
# reclaim already running its stop scripts gains nothing from a TERM, which
# kills bash mid-trap and leaves the stop script it was running carrying on.
lease_renew_start() {
	local owner="$$" on_lost="${4:-}"
	(
		trap - EXIT
		set +e
		# `command`, so a caller's sleep() stub cannot turn this into a spin.
		# Nothing but the lost message reaches the gate's output: a renewer or
		# its sleep holding a pipe open keeps a CI step waiting on it.
		while command sleep "${RELEASE_LEASE_RENEW_INTERVAL}" 2>/dev/null; do
			kill -0 "${owner}" 2>/dev/null || exit 0
			if [[ "$(lease_renew_once "$1" "$2" "$3")" == lost ]]; then
				[[ "${on_lost}" == stop-owner ]] || exit 0
				echo "error: another holder took the release-gate lease on $(lease_target "$1" "$2" "$3"); stopping this gate." >&2
				kill -TERM "${owner}" 2>/dev/null
				exit 0
			fi
		done
	) >/dev/null &
	LEASE_RENEWER_PID=$!
}

# lease_renew_stop — stop the background renewer, if one is running.
lease_renew_stop() {
	[[ -n "${LEASE_RENEWER_PID}" ]] || return 0
	kill "${LEASE_RENEWER_PID}" 2>/dev/null || true
	wait "${LEASE_RENEWER_PID}" 2>/dev/null || true
	LEASE_RENEWER_PID=""
}

# lease_release PROJECT ZONE CLUSTER — drop the lease IF this process holds it.
# The check is what keeps a gate that lost its lease from deleting its
# successor's. A read and a delete, not one atomic call: the gap is a successor
# taking over a lease this process renewed under a minute ago, which needs the
# holder's renewal to have lapsed while it was still running.
lease_release() {
	[[ "$(lease_ownership "$1" "$2" "$3")" == mine ]] || return 0
	lease_api_delete "$1" "$2" "$3"
	return 0
}

# --- Reporting --------------------------------------------------------------

# lease_describe PROJECT ZONE CLUSTER — one line naming who holds the lease, since
# when, and how to inspect it, for an operator-facing message.
lease_describe() {
	local rc=0 where
	where="Lease ${RELEASE_LEASE_NAMESPACE}/${RELEASE_LEASE_NAME} in $(lease_target "$1" "$2" "$3")"
	lease_read "$1" "$2" "$3" || rc=$?
	case "${rc}" in
	1)
		echo "no ${where}"
		return 0
		;;
	2)
		echo "could not read the ${where}"
		return 0
		;;
	esac
	local acquired="?" renewed="?" epoch
	epoch="$(lease_epoch "${LEASE_ACQUIRED}")"
	[[ -n "${epoch}" ]] && acquired="$(lease_age "${epoch}")"
	epoch="$(lease_epoch "${LEASE_RENEWED}")"
	[[ -n "${epoch}" ]] && renewed="$(lease_age "${epoch}")"
	printf 'held by %s (host/pid), RC %s, acquired %s, renewed %s (%s; kubectl -n %s get lease %s)' \
		"${LEASE_HOLDER:-?}" "${LEASE_RC:-?}" "${acquired}" "${renewed}" "${where}" \
		"${RELEASE_LEASE_NAMESPACE}" "${RELEASE_LEASE_NAME}"
}

# lease_age EPOCH — a human elapsed time since EPOCH. RELEASE_LEASE_NOW pins
# "now" for tests.
lease_age() {
	local now="${RELEASE_LEASE_NOW:-$(date +%s)}" then="$1"
	[[ "${then}" =~ ^[0-9]+$ ]] || {
		echo "?"
		return 0
	}
	local secs=$((now - then))
	((secs < 0)) && secs=0
	printf '%dh%02dm ago' $((secs / 3600)) $(((secs % 3600) / 60))
}
