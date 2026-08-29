#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/e2e-mirror-clients.sh — the Q1026 reading that
# says who actually reached the mirrors and whether every one of them carries the
# workload label.
#
# Why it is tested. The narrowing this reading gates is fail-closed and total: a
# client that loses the path cannot pull at all. So the two ways this script can
# lie both matter, and they are opposite:
#
#   1. Grading an ungradeable window green. A worker pod is deleted at the end of
#      its job, so its address resolves to nothing afterwards — the ORDINARY case
#      for a finished window. A script that skipped what it could not resolve
#      would report safety over exactly the clients it failed to check, and a
#      window in which nothing connected at all would read the same way. Both are
#      refusals here, and both are asserted below.
#   2. Missing an unlabelled client. That is the finding the whole reading is
#      for, and it must outrank a refusal sitting beside it, or one deleted pod
#      would bury it.
#
# The proxy log lines are HAProxy's, taken from a local run of the pinned image
# against the config the manifests render (2026-08-29), not invented: the client
# field is `<address>:<port>` and the proxy's own startup NOTICE lines share the
# stream.
#
#   3. Grading a PARTIAL read as a whole one. Measured on the first version: with
#      one instance's log unreadable, the output was byte-identical to a
#      five-of-five read, because all five were pooled through one `sort -u`.
#      That satisfied this script's own draft-exit condition. Every instance is
#      now graded by name, and the case is asserted below in both directions.
#
# The distinction that makes the per-instance grade usable is `read` vs `clients
# seen`: an instance that was read and saw nobody is legitimate, since not every
# mirror gets traffic in every window, while one that could not be read is a
# hole. Keying on an empty address list would conflate them and refuse a
# perfectly good window; keying on `kubectl logs`' exit status does not.
#
# The script is sourced with E2E_MIRROR_CLIENTS_LIB_ONLY=1 so main() does not
# run; `kubectl` is stubbed for the collector's cases, so no cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
E2E_MIRROR_CLIENTS_LIB_ONLY=1
export E2E_MIRROR_CLIENTS_LIB_ONLY
# shellcheck source=scripts/dogfood/e2e-mirror-clients.sh
source "${REPO_ROOT}/scripts/dogfood/e2e-mirror-clients.sh"

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

check_contains() {
	local name="$1" needle="$2" haystack="$3"
	if [[ "${haystack}" == *"${needle}"* ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: '${needle}' not in output" >&2
		echo "     output: ${haystack}" >&2
		fails=$((fails + 1))
	fi
}

grade() {
	set +e
	GRADE_OUT="$(grade_clients <<<"$1")"
	GRADE_RC=$?
	set -e
}

echo "scripts/dogfood/e2e-mirror-clients-test.sh"

# --- the address parse -------------------------------------------------------

A_PROXY_LOG='[NOTICE]   (1) : Initializing new worker (8)
[NOTICE]   (1) : Loading success.
10.4.2.17:51234 [29/Aug/2026:07:02:22.008] mirror registry/local 0/0/0/2/2 200 155 - - ---- 1/1/0/0/0 0/0 "GET /v2/ HTTP/1.1"
10.4.2.17:51240 [29/Aug/2026:07:02:28.113] mirror registry/local 0/0/1/9/10 200 9226 - - ---- 1/1/0/0/0 0/0 "GET /v2/library/alpine/manifests/3.20 HTTP/1.1"
10.128.0.31:38112 [29/Aug/2026:07:02:29.359] mirror registry/local 0/0/0/1/1 200 2 - - ---- 1/1/0/0/0 0/0 "GET /v2/ HTTP/1.1"
[2001:db8::5]:44001 [29/Aug/2026:07:02:31.002] mirror mirror/<NOSRV> 0/-1/-1/-1/0 403 175 - - LR-- 1/1/0/0/0 0/0 "GET /v2/_catalog HTTP/1.1"'

check "each address is reported once, however many requests it made" \
	"10.128.0.31
10.4.2.17
2001:db8::5" "$(client_addresses <<<"${A_PROXY_LOG}")"

# The proxy's own startup lines share the stream. A parser that took the first
# field of every line would enter "[NOTICE]" into the client set, and an address
# that is not an address resolves to nothing — a refusal manufactured by the
# parser rather than found in the window.
check "the proxy's own log lines are not clients" "" \
	"$(client_addresses <<<'[NOTICE]   (1) : Loading success.')"

# --- the verdict, in both directions ----------------------------------------

grade "10.4.2.17 pod-workload gag-dogfood-e2e/runner-abc
10.128.0.31 node gke-gag-dogfood-default-pool-1"
check "a labelled client and the kubelet pass" 0 "${GRADE_RC}"
check_contains "the kubelet is named as exempt" "EXEMPT  10.128.0.31 node/" "${GRADE_OUT}"

# The finding the reading exists for.
grade "10.4.2.17 pod-workload gag-dogfood-e2e/runner-abc
10.4.3.9 pod-unlabelled tenant-b/helper-job-xyz"
check "an unlabelled client fails" 1 "${GRADE_RC}"
check_contains "it says what narrowing would do to it" \
	"cuts this client off entirely" "${GRADE_OUT}"
check_contains "and names the pod" "tenant-b/helper-job-xyz" "${GRADE_OUT}"

# --- a reading that was not taken is never a pass ----------------------------
#
# The ordinary shape after a finished window: the worker that pulled has been
# deleted, so its address resolves to nothing. Skipping it would report safety
# over the one client that mattered.

grade "10.4.2.17 pod-workload gag-dogfood-e2e/runner-abc
10.4.9.44 unresolved -"
check "an unresolved client refuses rather than passing" 2 "${GRADE_RC}"
check_contains "the refusal says why the pod is likely gone" \
	"already been deleted" "${GRADE_OUT}"

grade ""
check "a window with no client at all refuses" 2 "${GRADE_RC}"
check_contains "and says nothing was graded" "nothing was graded" "${GRADE_OUT}"

# A finding outranks a refusal beside it: one deleted pod must not bury the
# unlabelled client this whole reading is for.
grade "10.4.9.44 unresolved -
10.4.3.9 pod-unlabelled tenant-b/helper-job-xyz"
check "an unlabelled client outranks an unresolved one" 1 "${GRADE_RC}"
check_contains "both are still reported" "REFUSE  10.4.9.44" "${GRADE_OUT}"

# --- every declared instance is graded by name ------------------------------
#
# The HOLD this fix answers. Pooling the five instances made an unread log
# invisible; the reading now names each one.

reads() { grade_instance_reads <<<"$1"; }

ALL_OK="$(for i in "${MIRROR_INSTANCES[@]}"; do printf 'read %s ok\n' "$i"; done)"

set +e
OUT="$(reads "${ALL_OK}")"; RC=$?
set -e
check "five readable instances pass" 0 "${RC}"
check "each is named" "${#MIRROR_INSTANCES[@]}" "$(grep -c '^READ ' <<<"${OUT}")"

# One instance unreadable. Under the old collector this was indistinguishable
# from a clean read; it must now refuse and name the instance.
set +e
OUT="$(reads "$(grep -v 'mirror-gcr-io' <<<"${ALL_OK}"; echo 'read mirror-gcr-io failed')")"; RC=$?
set -e
check "one unreadable instance refuses" 2 "${RC}"
check_contains "and is named" "REFUSE  mirror-gcr-io: its catalog-deny log could not be read" "${OUT}"
check "the other four still read" 4 "$(grep -c '^READ ' <<<"${OUT}")"

# An instance that reported nothing at all is the same hole as one that reported
# `failed`. Walking the transcript rather than the table would find neither.
set +e
OUT="$(reads "$(grep -v 'mirror-quay-io' <<<"${ALL_OK}")")"; RC=$?
set -e
check "an instance with no record refuses" 2 "${RC}"
check_contains "and says no read was reported" "no read was attempted or reported at all" "${OUT}"

# --- read, but idle, is NOT a refusal ----------------------------------------
#
# The distinction the union could not make. A mirror nobody pulled through in a
# given window is ordinary; refusing on it would make the reading unusable in
# any window where traffic did not touch all five.
set +e
OUT="$(reads "${ALL_OK}")"; RC=$?
set -e
check "five read, zero clients seen, still passes the read grade" 0 "${RC}"

# --- the collector distinguishes them, keyed on exit status ------------------

LINE='10.4.2.17:51234 [29/Aug/2026:07:02:22.008] mirror registry/local 0/0/0/2/2 200 155 - - ---- 1/1/0/0/0 0/0 "GET /v2/ HTTP/1.1"'
UNREADABLE=""
kubectl() {
	if [[ -n "${UNREADABLE}" && "$*" == *"${UNREADABLE}"* ]]; then
		return 1
	fi
	[[ -n "${SILENT_OK:-}" ]] || printf '%s\n' "${LINE}"
}

UNREADABLE="" SILENT_OK="" records="$(collect_client_addresses)"
check "a full read reports five ok" 5 "$(grep -c '^read .* ok$' <<<"${records}")"
check "and one client record per instance" 5 "$(grep -c '^client ' <<<"${records}")"

UNREADABLE="mirror-gcr-io" SILENT_OK="" records="$(collect_client_addresses)"
check "an unreadable instance is recorded as failed" 1 "$(grep -c '^read mirror-gcr-io failed$' <<<"${records}")"
check "and contributes no client record" 0 "$(grep -c '^client mirror-gcr-io ' <<<"${records}")"

# The case that must NOT refuse: every log read, none of them holding a client.
UNREADABLE="" SILENT_OK=1 records="$(collect_client_addresses)"
check "an idle-but-readable fleet reports five ok" 5 "$(grep -c '^read .* ok$' <<<"${records}")"
check "and no failed record" 0 "$(grep -c 'failed$' <<<"${records}")"

# --- a hostNetwork pod sharing a node address is not silently resolved -------
#
# Both lookups match, and neither answer is right: as a pod it is graded on a
# label no selector usefully applies to it, as a node it is waved through.
grade "10.128.0.31 ambiguous kube-system/anetd-abc+node/gke-pool-1"
check "an ambiguous address refuses" 2 "${GRADE_RC}"
check_contains "naming both sides" "kube-system/anetd-abc+node/gke-pool-1" "${GRADE_OUT}"
check_contains "and saying why it is not decidable" "not decidable from here" "${GRADE_OUT}"

echo
if ((fails)); then
	echo "${fails} check(s) failed" >&2
	exit 1
fi
echo "all e2e-mirror-clients.sh assertions passed"
