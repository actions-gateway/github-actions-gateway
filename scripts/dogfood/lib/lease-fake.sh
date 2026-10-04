# In-memory stand-in for the four Lease API primitives in lib/lease.sh, for the
# dogfood test suites. Source AFTER lib/lease.sh, so these definitions win.
#
# One file per target under RELEASE_LEASE_DIR holds the record as
# holder|renewTime|duration|acquireTime|rc, the shape lease_api_get prints.
# Create fails when the file exists and patch fails unless the expected holder
# still holds it, which is the compare-and-swap the real API gives. What it
# cannot model is the API itself: that kubectl's create, JSON-patch test op and
# --ignore-not-found behave as lib/lease.sh assumes is unverified here.
#
#   LEASE_FAKE_READ_FAILS=1    every get fails, as an unreachable API does
#   LEASE_FAKE_WRITE_FAILS=1   every create, patch and delete fails
# shellcheck shell=bash

# lease_fake_path PROJECT ZONE CLUSTER — the backing file for one target.
lease_fake_path() {
	local key="$1-$2-$3"
	echo "${RELEASE_LEASE_DIR}/fake-${key//[^A-Za-z0-9._-]/-}.lease"
}

# lease_fake_write PROJECT ZONE CLUSTER HOLDER RENEWED DURATION ACQUIRED RC —
# hand-write a record, for the states this process cannot reach by acquiring.
lease_fake_write() {
	mkdir -p "${RELEASE_LEASE_DIR}"
	printf '%s|%s|%s|%s|%s\n' "$4" "$5" "$6" "$7" "$8" >"$(lease_fake_path "$1" "$2" "$3")"
}

lease_api_get() {
	[[ -z "${LEASE_FAKE_READ_FAILS:-}" ]] || return 1
	local f
	f="$(lease_fake_path "$1" "$2" "$3")"
	[[ -f "${f}" ]] && cat "${f}"
	return 0
}

lease_api_create() {
	[[ -z "${LEASE_FAKE_WRITE_FAILS:-}" ]] || return 1
	[[ ! -f "$(lease_fake_path "$1" "$2" "$3")" ]] || return 1
	local now
	now="$(lease_now_iso)"
	lease_fake_write "$1" "$2" "$3" "$4" "${now}" "${RELEASE_LEASE_DURATION}" "${now}" "${5:-}"
}

lease_api_patch() {
	[[ -z "${LEASE_FAKE_WRITE_FAILS:-}" ]] || return 1
	local f holder duration acquired rc now
	f="$(lease_fake_path "$1" "$2" "$3")"
	[[ -f "${f}" ]] || return 1
	IFS='|' read -r holder _ duration acquired rc <"${f}"
	[[ "${holder}" == "$4" ]] || return 1
	now="$(lease_now_iso)"
	if [[ "$4" != "$5" ]]; then
		acquired="${now}"
		rc="${6:-}"
	fi
	lease_fake_write "$1" "$2" "$3" "$5" "${now}" "${duration}" "${acquired}" "${rc}"
}

lease_api_delete() {
	[[ -z "${LEASE_FAKE_WRITE_FAILS:-}" ]] || return 1
	rm -f "$(lease_fake_path "$1" "$2" "$3")"
}
