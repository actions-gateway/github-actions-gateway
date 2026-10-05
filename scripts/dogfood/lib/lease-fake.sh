# In-memory stand-in for the four Lease API primitives in lib/lease.sh, for the
# dogfood test suites. Source AFTER lib/lease.sh, so these definitions win.
#
# One file per target under RELEASE_LEASE_DIR holds the record as
# holder|renewTime|duration|acquireTime|rc, the shape lease_api_get prints.
# Create fails when the file exists, and patch fails unless the record still has
# the expected holder (and, when given, the expected renewTime), which is the
# compare-and-swap the real API gives. What it cannot model is the API itself:
# that kubectl's create, JSON-patch test op and --ignore-not-found behave as
# lib/lease.sh assumes is unverified here.
#
#   LEASE_FAKE_READ_FAILS=1    every get fails, as an unreachable API does
#   LEASE_FAKE_WRITE_FAILS=1   every create, patch and delete fails
# shellcheck shell=bash

# lease_fake_path PROJECT ZONE CLUSTER — the backing file for one target.
lease_fake_path() {
	local key="$1-$2-$3"
	echo "${RELEASE_LEASE_DIR}/fake-${key//[^A-Za-z0-9._-]/-}.lease"
}

# lease_fake_locked PROJECT ZONE CLUSTER CMD ARGS... — run CMD holding the
# target's lock, so a create, patch or delete is one step as the apiserver's
# are. Without it a background renewer can read a record, lose a race to a test
# overwriting it, and write its renewal back over the overwrite. `command sleep`
# because a suite may stub sleep(). A lock held past 5s belongs to a renewer
# killed mid-write (lease_renew_stop), so it is broken rather than waited on.
lease_fake_locked() {
	local lock rc=0 tries=0
	mkdir -p "${RELEASE_LEASE_DIR}"
	lock="$(lease_fake_path "$1" "$2" "$3").lock"
	until mkdir "${lock}" 2>/dev/null; do
		((++tries < 500)) || rmdir "${lock}" 2>/dev/null || true
		command sleep 0.01
	done
	shift 3
	"$@" || rc=$?
	rmdir "${lock}"
	return "${rc}"
}

# lease_fake_put PROJECT ZONE CLUSTER HOLDER RENEWED DURATION ACQUIRED RC — write
# a record by rename, so a concurrent reader sees the old one or the new one.
lease_fake_put() {
	local f tmp
	f="$(lease_fake_path "$1" "$2" "$3")"
	tmp="${f}.${BASHPID}"
	printf '%s|%s|%s|%s|%s\n' "$4" "$5" "$6" "$7" "$8" >"${tmp}"
	mv "${tmp}" "${f}"
}

# lease_fake_write PROJECT ZONE CLUSTER HOLDER RENEWED DURATION ACQUIRED RC —
# hand-write a record, for the states this process cannot reach by acquiring.
lease_fake_write() {
	lease_fake_locked "$1" "$2" "$3" lease_fake_put "$@"
}

lease_api_get() {
	[[ -z "${LEASE_FAKE_READ_FAILS:-}" ]] || return 1
	local f
	f="$(lease_fake_path "$1" "$2" "$3")"
	[[ -f "${f}" ]] && cat "${f}"
	return 0
}

lease_fake_create() {
	[[ ! -f "$(lease_fake_path "$1" "$2" "$3")" ]] || return 1
	local now
	now="$(lease_now_iso)"
	lease_fake_put "$1" "$2" "$3" "$4" "${now}" "${RELEASE_LEASE_DURATION}" "${now}" "${5:-}"
}

lease_api_create() {
	[[ -z "${LEASE_FAKE_WRITE_FAILS:-}" ]] || return 1
	lease_fake_locked "$1" "$2" "$3" lease_fake_create "$@"
}

lease_fake_patch() {
	local f holder renewed duration acquired rc now
	f="$(lease_fake_path "$1" "$2" "$3")"
	[[ -f "${f}" ]] || return 1
	IFS='|' read -r holder renewed duration acquired rc <"${f}"
	[[ "${holder}" == "$4" ]] || return 1
	[[ -z "${7:-}" || "${renewed}" == "$7" ]] || return 1
	now="$(lease_now_iso)"
	if [[ "$4" != "$5" ]]; then
		acquired="${now}"
		rc="${6:-}"
	fi
	lease_fake_put "$1" "$2" "$3" "$5" "${now}" "${duration}" "${acquired}" "${rc}"
}

lease_api_patch() {
	[[ -z "${LEASE_FAKE_WRITE_FAILS:-}" ]] || return 1
	lease_fake_locked "$1" "$2" "$3" lease_fake_patch "$@"
}

lease_api_delete() {
	[[ -z "${LEASE_FAKE_WRITE_FAILS:-}" ]] || return 1
	lease_fake_locked "$1" "$2" "$3" rm -f "$(lease_fake_path "$1" "$2" "$3")"
}
