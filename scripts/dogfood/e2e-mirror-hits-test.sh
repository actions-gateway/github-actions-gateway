#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/e2e-mirror-hits.sh: the Q408 Phase 3 reading
# that says whether the job's pulls actually rode the mirror.
#
# Why it is tested. Phase 3 runs with the Kata overlay's allow-all
# `e2e-open-egress` still in place, so the e2e suite is green whether the wiring
# worked or not — this counter is the ONLY thing that separates the two, on a
# booked dogfood session that costs a pool resize to repeat. Every way it could
# lie is silent:
#
#   1. A query that never matches counts zero. Here zero is a FAIL, so a broken
#      parser wastes the session rather than passing it — which is the safe
#      direction and still the wrong outcome. The log fixture below is a VERBATIM
#      capture from registry:3.1.1 in proxy mode (2026-08-28, the digest the
#      manifests pin), so the parser is asserted against the format it will meet.
#   2. The kubelet probes `/v2/` twice per instance every 10-20 seconds, so a
#      request count over an instance nothing pulled through is in the thousands.
#      The `/v2/` root must not count, and that is asserted directly.
#   3. An instance whose log cannot be read reports nothing, and a grader walking
#      what it received finds nothing wrong with it. The expected set is the
#      declared instance table, so an absent line is a FAIL with its own reason.
#   4. A mirror answering 500 to every pull is the §3.6 fsGroup shape. It is
#      reached, so it accumulates content requests; none are served. Content
#      alone would grade it green.
#
# The script is sourced with E2E_MIRROR_HITS_LIB_ONLY=1 so main() does not run;
# nothing here touches kubectl or a cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
E2E_MIRROR_HITS_LIB_ONLY=1
export E2E_MIRROR_HITS_LIB_ONLY
# shellcheck source=scripts/dogfood/e2e-mirror-hits.sh
source "${REPO_ROOT}/scripts/dogfood/e2e-mirror-hits.sh"

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
		fails=$((fails + 1))
	fi
}

# --- the fixture -------------------------------------------------------------
#
# Captured from `docker logs` of registry:3.1.1 run in proxy mode with the
# Deployment's own env (REGISTRY_PROXY_REMOTEURL, the empty debug addr, access
# log enabled), probed with curl. Both line shapes are here — the structured
# logrus lines and the Combined Log Format access lines — because the parser has
# to pick the second out of the first, and a fixture holding only the shape it
# wants would not test that.
measured_log() {
	cat <<'LOG'
time="2026-08-28T15:22:58.1Z" level=info msg="using inmemory blob descriptor cache" environment=development go.version=go1.25.9 instance.id=01a048f7 service=registry version=3.1.1
192.168.65.1 - - [28/Aug/2026:15:23:00 +0000] "GET /v2/ HTTP/1.1" 200 2 "" "curl/8.7.1"
time="2026-08-28T15:23:00.6Z" level=info msg="Challenge established with upstream: https://registry-1.docker.io/v2/" environment=development http.request.method=GET http.request.uri=/v2/library/alpine/manifests/latest service=registry version=3.1.1
192.168.65.1 - - [28/Aug/2026:15:23:00 +0000] "GET /v2/library/alpine/manifests/latest HTTP/1.1" 200 9218 "" "curl/8.7.1"
192.168.65.1 - - [28/Aug/2026:15:23:01 +0000] "GET /v2/library/alpine/blobs/sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b HTTP/1.1" 200 3401 "" "docker/28.0.0 go/go1.23.0 kernel/6.18.35"
192.168.65.1 - - [28/Aug/2026:15:23:01 +0000] "POST /v2/library/alpine/blobs/uploads/ HTTP/1.1" 405 78 "" "curl/8.7.1"
LOG
}

# --- count_hits --------------------------------------------------------------

check 'the measured log yields 3 content requests, 2 served, 1 repository' \
	'3 2 1' "$(measured_log | count_hits)"

check 'a log of nothing but /v2/ probes counts no content' \
	'0 0 0' "$(count_hits <<'LOG'
10.0.0.1 - - [28/Aug/2026:15:23:00 +0000] "GET /v2/ HTTP/1.1" 200 2 "" "kube-probe/1.35"
10.0.0.1 - - [28/Aug/2026:15:23:10 +0000] "GET /v2/ HTTP/1.1" 200 2 "" "kube-probe/1.35"
LOG
)"

# The §3.6 fsGroup shape: reached, and serving nothing.
check 'an instance answering 500 to every pull is content-but-unserved' \
	'2 0 1' "$(count_hits <<'LOG'
10.0.0.1 - - [28/Aug/2026:15:23:00 +0000] "GET /v2/ HTTP/1.1" 200 2 "" "kube-probe/1.35"
10.0.0.1 - - [28/Aug/2026:15:23:01 +0000] "GET /v2/library/alpine/manifests/latest HTTP/1.1" 500 78 "" "docker/28.0.0"
10.0.0.1 - - [28/Aug/2026:15:23:02 +0000] "GET /v2/library/alpine/manifests/3.20 HTTP/1.1" 500 78 "" "docker/28.0.0"
LOG
)"

# A user-agent holding spaces is the ordinary case, not an exotic one — docker's
# own carries three. A parser reading the status by column position reports the
# wrong field on every real line.
check 'a multi-word user agent does not shift the status read' \
	'1 1 1' "$(count_hits <<'LOG'
10.0.0.1 - - [28/Aug/2026:15:23:01 +0000] "GET /v2/actions-gateway/gmc/manifests/v1.2.0 HTTP/1.1" 200 1 "" "docker/28.0.0 go/go1.23.0 kernel/6.18.35"
LOG
)"

# --- hit_repositories --------------------------------------------------------

check 'a multi-segment repository survives the tail strip' \
	'actions-gateway/charts/actions-gateway' "$(hit_repositories <<'LOG'
10.0.0.1 - - [28/Aug/2026:15:23:01 +0000] "GET /v2/actions-gateway/charts/actions-gateway/manifests/1.2.0 HTTP/1.1" 200 1 "" "Helm/3.16"
LOG
)"

check 'the /v2/ root names no repository' '' "$(hit_repositories <<'LOG'
10.0.0.1 - - [28/Aug/2026:15:23:00 +0000] "GET /v2/ HTTP/1.1" 200 2 "" "kube-probe/1.35"
LOG
)"

# --- grade_hit_counts --------------------------------------------------------

all_serving() {
	local instance
	for instance in "${MIRROR_INSTANCES[@]}"; do
		printf '%s 12 12 3\n' "${instance}"
	done
}

out="$(all_serving | grade_hit_counts)"
rc=0
all_serving | grade_hit_counts >/dev/null || rc=$?
check 'every instance serving grades 0' 0 "${rc}"
check 'five instances produce five lines' 5 "$(printf '%s\n' "${out}" | awk 'END { print NR }')"
check_contains 'a passing line carries the served/total counts' \
	'PASS mirror-docker-io: 12/12 content requests served, 3 distinct repositories' "${out}"

# The failure this instrument exists for: a client that ignored its wiring and
# reached its upstream instead, leaving its mirror with probes only.
rc=0
out="$(all_serving | awk '$1 == "mirror-gcr-io" { $2 = 0; $3 = 0; $4 = 0 } { print }' |
	grade_hit_counts)" || rc=$?
check 'one instance with no content grades 1' 1 "${rc}"
check_contains 'and names it as pulled through by nothing' \
	'FAIL mirror-gcr-io: 0 content requests' "${out}"
check_contains 'while its siblings still pass' 'PASS mirror-ghcr-io' "${out}"

# An instance that was reached and answered nothing is a different fault with a
# different remedy, so it gets its own reason line.
rc=0
out="$(all_serving | awk '$1 == "mirror-quay-io" { $3 = 0 } { print }' | grade_hit_counts)" || rc=$?
check 'an unserved instance grades 1' 1 "${rc}"
check_contains 'and says it answered nothing rather than that it was unused' \
	'FAIL mirror-quay-io: 12 content requests, none served' "${out}"

# The silent-instrument case: a log that could not be read reports no line at
# all, and the battery must not shrink to the four that did.
rc=0
out="$(all_serving | grep -v mirror-registry-k8s-io | grade_hit_counts)" || rc=$?
check 'an unreported instance grades 1' 1 "${rc}"
check_contains 'and is named as unread rather than as unused' \
	'FAIL mirror-registry-k8s-io: no log read' "${out}"
check 'the battery is still five lines' 5 "$(printf '%s\n' "${out}" | awk 'END { print NR }')"

rc=0
out="$(grade_hit_counts </dev/null)" || rc=$?
check 'an empty transcript grades 1' 1 "${rc}"
check 'an empty transcript still grades five instances' 5 \
	"$(printf '%s\n' "${out}" | grep -c '^FAIL' || true)"

if ((fails)); then
	echo "${fails} assertion(s) failed" >&2
	exit 1
fi
echo
echo "all e2e-mirror-hits.sh assertions passed"
