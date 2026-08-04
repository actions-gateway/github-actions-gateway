# Ownership lease for the dogfood release-validation gate. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/dogfood/lib/lease.sh
#   source "$REPO_ROOT/scripts/dogfood/lib/lease.sh"
#
# validate-release.sh self-cleans through `trap teardown EXIT`, and bash runs
# that trap on TERM, INT and HUP as well as on an ordinary exit (measured on
# bash 5.3). SIGKILL cannot be trapped at all, a killed parent takes the whole
# process group with it, and a teardown that is itself killed part-way through
# stops between the two stop scripts. Every one of those leaves the same state:
# no process, and a cluster still billing (Q640).
#
# The lease is the out-of-process record that makes that state legible. The gate
# holds one for exactly the window in which it owns billable cluster state —
# taken before the first scale-up, released last in teardown, after the stop
# scripts — so a lease whose owning process no longer exists IS an orphaned run,
# and nothing else is.
#
# That distinction is the whole safety argument. A cluster that merely has nodes
# up is never evidence of an orphan: an operator debugging by hand leaves exactly
# that state, and a mechanism that inferred an orphan from cluster state would
# tear their work down. No lease, no reclaim.
#
# One lease per target (project/zone/cluster), so a gate against one cluster
# neither blocks nor reclaims another. Host-local by design: a lease records a
# pid, and a pid only means anything on the host that minted it — a record from
# another host is reported, never acted on.
# shellcheck shell=bash

# RELEASE_LEASE_DIR — where leases live. Host-wide rather than repo-local
# (unlike RELEASE_PROGRESS_FILE, lib/progress.sh): worktrees are per-branch but
# the dogfood cluster is not, so a gate killed in one worktree must be visible
# to a gate started from another. Tests point this at their own scratch dir.
RELEASE_LEASE_DIR="${RELEASE_LEASE_DIR:-${XDG_STATE_HOME:-${HOME}/.local/state}/github-actions-gateway}"

# RELEASE_LEASE_MARKER — a substring the owning process's command line must
# still contain for the lease to count as held. See lease_owner_alive.
RELEASE_LEASE_MARKER="${RELEASE_LEASE_MARKER:-validate-release.sh}"

# lease_host — this host's name. A function so a test can model two hosts.
lease_host() { echo "${HOSTNAME:-$(hostname)}"; }

# lease_target PROJECT ZONE CLUSTER — the canonical target string recorded in a
# lease and re-checked when one is read.
lease_target() { echo "$1/$2/$3"; }

# lease_path PROJECT ZONE CLUSTER — the lease file for one target. The name is
# sanitized to a flat filename; the target inside the file is what a reader
# trusts, so two targets that sanitize alike still read as foreign to each other.
lease_path() {
	local key="$1-$2-$3"
	echo "${RELEASE_LEASE_DIR}/release-gate-${key//[^A-Za-z0-9._-]/-}.lease"
}

# lease_field FILE KEY — print KEY's value from a lease file, empty if absent.
# Splits on the FIRST '=' only, so a value may contain one.
lease_field() {
	[[ -r "$1" ]] || return 0
	awk -v key="$2" -F= '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$1" 2>/dev/null || true
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
#   orphaned  a gate owned it and its process is gone. Reclaimable.
#   foreign   the record was written by another host, or names another target.
#             Its pid means nothing here, so it is reported, never acted on.
lease_state() {
	local lease
	lease="$(lease_path "$1" "$2" "$3")"
	[[ -f "${lease}" ]] || {
		echo free
		return 0
	}
	if [[ "$(lease_field "${lease}" host)" != "$(lease_host)" ]] ||
		[[ "$(lease_field "${lease}" target)" != "$(lease_target "$1" "$2" "$3")" ]]; then
		echo foreign
		return 0
	fi
	if lease_owner_alive "$(lease_field "${lease}" pid)" "$(lease_field "${lease}" marker)"; then
		echo held
	else
		echo orphaned
	fi
}

# lease_acquire PROJECT ZONE CLUSTER [RC] — claim the target for this process.
# Returns 1 when another lease already exists, having changed nothing.
lease_acquire() {
	local lease tmp
	lease="$(lease_path "$1" "$2" "$3")"
	mkdir -p "${RELEASE_LEASE_DIR}" 2>/dev/null || return 1
	tmp="${lease}.$$"
	{
		printf 'pid=%s\n' "$$"
		printf 'host=%s\n' "$(lease_host)"
		printf 'started=%s\n' "$(date +%s)"
		printf 'target=%s\n' "$(lease_target "$1" "$2" "$3")"
		printf 'rc=%s\n' "${4:-}"
		printf 'marker=%s\n' "${RELEASE_LEASE_MARKER}"
	} >"${tmp}" 2>/dev/null || return 1
	# ln is atomic and fails when the target exists, so two gates racing here
	# cannot both believe they hold the lease. Linking a fully written temp file
	# rather than creating and then filling one means a lease that exists is
	# always complete — a reader never has to guess at a half-written record.
	local rc=0
	ln "${tmp}" "${lease}" 2>/dev/null || rc=1
	rm -f "${tmp}" 2>/dev/null
	return "${rc}"
}

# lease_release PROJECT ZONE CLUSTER — drop the lease IF this process owns it.
# The ownership check is what keeps a teardown from deleting the record of a
# gate that is still running (two gates, one cluster: the loser must not clear
# the winner's lease on its way out).
lease_release() {
	local lease
	lease="$(lease_path "$1" "$2" "$3")"
	[[ -f "${lease}" ]] || return 0
	[[ "$(lease_field "${lease}" pid)" == "$$" ]] || return 0
	rm -f "${lease}" 2>/dev/null
	return 0
}

# lease_discard PROJECT ZONE CLUSTER — drop the lease unconditionally. Only for
# a reclaim that has actually torn the orphaned run's cluster back down; a
# failed reclaim must leave the record for the next attempt.
lease_discard() {
	rm -f "$(lease_path "$1" "$2" "$3")" 2>/dev/null
	return 0
}

# lease_describe PROJECT ZONE CLUSTER — one line naming who holds the lease and
# since when, for an operator-facing message.
lease_describe() {
	local lease
	lease="$(lease_path "$1" "$2" "$3")"
	[[ -f "${lease}" ]] || {
		echo "no lease at $(lease_path "$1" "$2" "$3")"
		return 0
	}
	local started="" pid rc host
	pid="$(lease_field "${lease}" pid)"
	rc="$(lease_field "${lease}" rc)"
	host="$(lease_field "${lease}" host)"
	local epoch
	epoch="$(lease_field "${lease}" started)"
	[[ -n "${epoch}" ]] && started="$(lease_age "${epoch}")"
	printf 'pid %s on %s, RC %s, started %s (%s)' \
		"${pid:-?}" "${host:-?}" "${rc:-?}" "${started:-?}" "${lease}"
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
