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
# The script is sourced with E2E_MIRROR_CLIENTS_LIB_ONLY=1 so main() does not
# run; nothing here touches kubectl or a cluster.
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

echo
if ((fails)); then
	echo "${fails} check(s) failed" >&2
	exit 1
fi
echo "all e2e-mirror-clients.sh assertions passed"
