#!/usr/bin/env bash
#
# Q408 Phase 3 validation: did the job's image pulls actually ride the mirror?
#
# Phase 3 wires the clients (dockerd's daemon.json, the rewritten non-Hub refs,
# helm's OCI ref, buildkit's config) while the Kata overlay's allow-all
# `e2e-open-egress` is still in place. That is deliberate — wiring proven before
# enforcement changes — and it is also why a green e2e run proves nothing there:
# a client that ignored its wiring reaches the upstream and the suite passes
# exactly the same. The reading that discriminates is the mirror's own access
# log, which no unmirrored pull can write into.
#
# So: one non-zero hit count per declared instance, or the phase is not done.
#
# Phase 4 deleted that policy, so an unwired client now fails the run outright
# and this script is no longer the only signal. It stays the one that ATTRIBUTES:
# the repositories each instance was asked for name which client rode it, and
# they are the plan's §3.4 audit point. Its negative twin, the half that says a
# path is absent rather than used, is scripts/e2e/egress-negatives.sh.
#
# WHAT COUNTS AS A HIT. Distribution's access log is Combined Log Format, one
# line per request, alongside the structured logrus lines. Measured 2026-08-29 on
# registry:3.1.1 at the pinned digest, behind the catalog-deny proxy, which is
# the shape this reads today:
#
#   127.0.0.1 - - [29/Aug/2026:07:02:22 +0000] "GET /v2/ HTTP/1.1" 200 2 "" "curl/8.7.1"
#   127.0.0.1 - - [29/Aug/2026:07:02:28 +0000] "GET /v2/library/alpine/manifests/3.20 HTTP/1.1" 200 9226 "" "curl/8.7.1"
#
# The 2026-08-28 capture the earlier version of this header quoted was taken
# against an unfronted registry and carried a real client address; the lines
# above are a separate reading, not that one restamped.
#
# THE REMOTE ADDRESS IS THE POD'S OWN LOOPBACK, and reading it as the client is
# the mistake this shape invites. Every pod fronts its registry with the
# catalog-deny proxy (Q1022), so the registry sees 127.0.0.1 for every request
# and the source address survives only in that container's log. Nothing below
# reads it: what discriminates is the request path and the user agent, and the
# proxy forwards both unchanged.
#
# The first line is why a bare request count is the wrong instrument: the
# kubelet probes `/v2/` every 10 seconds on two probes per instance, so an
# instance nothing ever pulled through still accumulates thousands of requests.
# A hit is therefore a request whose path is DEEPER than `/v2/` — a manifest, a
# blob, a tags list — and the verdict needs one that was also SERVED (2xx/3xx),
# since a mirror answering 500 to everything is the §3.6 fsGroup shape and is
# not a mirror that worked.
#
# THE OTHER WRITER OF THIS LOG IS THE PHASE 2 BATTERY, and it is the one that
# would make this reading unfalsifiable. e2e-mirror-validate.sh fetches a real
# manifest and attempts an upload against every instance from a curl pod, so a
# session that runs it first — which the plan's own sequence does, and Phase 4
# does again — leaves all five instances non-zero before the job starts.
# Measured 2026-08-28: the battery alone puts 2 content requests and 1 served on
# each instance, which is a PASS from an instrument that measured nothing.
#
# So a hit must also come from a client that is not that probe, and the test is
# the user agent: real pulls carry docker's, helm's or buildkit's, and the probe
# carries curl's. The exclusion is written to fail SAFE — an unrecognised client
# UA under-counts and reports FAIL, which is investigated, while a probe whose
# curl version bumps stays excluded because the pattern is the client name.
#
# This script only reads: no manifest is applied, no pod is created. Run it
# after a Kata e2e run has finished, against the same tenant, before
# `e2e-stop.sh` scales the mirrors to zero and their logs go with the pods.
#
# Required env vars (export before running):
#   PROJECT   GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-east1-b)
#
# Optional:
#   MIRROR_NAMESPACE  namespace holding the instances (default: gag-registry-mirror)
#
# Exit: 0 when every declared instance served at least one content request, 1
# when any did not — including an instance whose log could not be read at all,
# which is the failure a count over a transcript hides best.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

MIRROR_NAMESPACE="${MIRROR_NAMESPACE:-gag-registry-mirror}"

# The declared instance set, as in e2e-mirror-validate.sh and for the same
# reason: derived from a live listing, four mirrors serving out of five declared
# would grade green.
MIRROR_INSTANCES=(
	mirror-docker-io
	mirror-ghcr-io
	mirror-quay-io
	mirror-registry-k8s-io
	mirror-gcr-io
)

# The container whose access log this reads, named rather than left to kubectl's
# default. Each pod carries a second container, the catalog-deny proxy, and an
# unqualified `kubectl logs` picks by position: the read would move with the
# container order and its warning goes to the stderr these calls discard.
MIRROR_CONTAINER="registry"

# --- pure helpers (unit-tested; no kubectl, no cluster) ----------------------

# hit_repositories — read an access log on stdin, print each distinct repository
# a content request named, sorted. `/v2/<name>/{manifests,blobs,tags}/…`, where
# <name> may itself hold slashes, so the tail is stripped rather than the parts
# counted. Excludes the Phase 2 battery's own probes on the same test as
# count_hits, so the audit listing names what the JOB asked each upstream for.
hit_repositories() {
	awk '
		match($0, /"[A-Z]+ \/v2\/[^ "]+/) {
			ua = $0
			sub(/"[^"]*$/, "", ua)
			if (match(ua, /"[^"]*$/) && substr(ua, RSTART + 1) ~ /^curl\//) next
			match($0, /"[A-Z]+ \/v2\/[^ "]+/)
			uri = substr($0, RSTART, RLENGTH)
			sub(/^"[A-Z]+ \/v2\//, "", uri)
			if (uri !~ /\/(manifests|blobs|tags)\//) next
			sub(/\/(manifests|blobs|tags)\/.*$/, "", uri)
			print uri
		}' | sort -u
}

# count_hits — read an access log on stdin, print "<content> <served> <repos>":
# requests below /v2/, those of them answered 2xx/3xx, and the number of
# distinct repositories named.
#
# The request field is quoted and space-separated ("GET <uri> HTTP/1.1"), so the
# method, URI and status are read by locating that field rather than by column
# position: a client with a space in its user-agent shifts every later column.
count_hits() {
	local log content served repos
	log="$(cat)"
	read -r content served <<<"$(awk '
		match($0, /"[A-Z]+ [^"]+"/) {
			req = substr($0, RSTART + 1, RLENGTH - 2)
			split(req, parts, " ")
			if (parts[2] !~ /^\/v2\/.+/) next
			# Everything after the request field, captured NOW: the user-agent
			# match below resets RSTART/RLENGTH, and reading the status off the
			# stale pair silently reported every request unserved.
			rest = substr($0, RSTART + RLENGTH)
			# The user agent is the last quoted field of the CLF line. A curl
			# there is e2e-mirror-validate.sh probing, not a pull (see header).
			ua = $0
			sub(/"[^"]*$/, "", ua)
			if (match(ua, /"[^"]*$/) && substr(ua, RSTART + 1) ~ /^curl\//) next
			content++
			split(rest, after, " ")
			if (after[1] ~ /^[23][0-9][0-9]$/) served++
		}
		END { printf "%d %d\n", content + 0, served + 0 }' <<<"${log}")"
	# awk rather than `grep -c`: on no repositories grep prints 0 and exits 1,
	# which under pipefail leaves the count empty rather than zero.
	repos="$(hit_repositories <<<"${log}" | awk 'END { print NR }')"
	printf '%s %s %s\n' "${content}" "${served}" "${repos}"
}

# grade_hit_counts — read "<instance> <content> <served> <repos>" lines on
# stdin and print one PASS/FAIL line per DECLARED instance, in table order.
# Exits 1 if any instance failed or did not report.
#
# The expected set is the table, not the transcript, for the reason
# e2e-mirror-validate.sh's grader gives: an instance whose log could not be read
# reports nothing, and a grader walking what it received finds nothing wrong.
grade_hit_counts() {
	local -A content=() served=() repos=()
	local instance c s r failed=0
	while read -r instance c s r; do
		[[ -n "${instance}" ]] || continue
		content["${instance}"]="${c}"
		served["${instance}"]="${s}"
		repos["${instance}"]="${r}"
	done
	for instance in "${MIRROR_INSTANCES[@]}"; do
		c="${content["${instance}"]:-}"
		s="${served["${instance}"]:-}"
		r="${repos["${instance}"]:-0}"
		if [[ -z "${c}" ]]; then
			echo "FAIL ${instance}: no log read (the instance is absent, or its pod is gone)"
			failed=1
		elif ((c == 0)); then
			echo "FAIL ${instance}: 0 content requests — nothing was pulled through this instance"
			failed=1
		elif ((s == 0)); then
			echo "FAIL ${instance}: ${c} content requests, none served (2xx/3xx) — the instance was reached and answered nothing"
			failed=1
		else
			echo "PASS ${instance}: ${s}/${c} content requests served, ${r} distinct repositories"
		fi
	done
	return "${failed}"
}

# --- cluster-side ------------------------------------------------------------

# collect_hit_counts — emit one "<instance> <content> <served> <repos>" line per
# instance whose log can be read, and nothing for one that cannot: the grader
# turns that silence into a named failure rather than a shrunken battery.
collect_hit_counts() {
	local instance logs
	for instance in "${MIRROR_INSTANCES[@]}"; do
		logs="$(kubectl logs "deployment/${instance}" --namespace "${MIRROR_NAMESPACE}" \
			--container "${MIRROR_CONTAINER}" --tail=-1 2>/dev/null || true)"
		[[ -n "${logs}" ]] || continue
		printf '%s %s\n' "${instance}" "$(count_hits <<<"${logs}")"
	done
}

# print_requested_repositories — the §3.4 audit point, made concrete. The
# pull-name side channel is accepted there on the grounds that it is auditable
# at one place; this prints what that audit reads, so the session records what
# the job asked each upstream for rather than only how much.
print_requested_repositories() {
	local instance
	for instance in "${MIRROR_INSTANCES[@]}"; do
		echo "  ${instance}:"
		kubectl logs "deployment/${instance}" --namespace "${MIRROR_NAMESPACE}" \
			--container "${MIRROR_CONTAINER}" --tail=-1 2>/dev/null |
			hit_repositories | sed 's/^/    /' || true
	done
}

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"

	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	if ! kubectl get namespace "${MIRROR_NAMESPACE}" >/dev/null 2>&1; then
		die "namespace '${MIRROR_NAMESPACE}' is absent — the mirrors are not up (scripts/dogfood/e2e-start.sh)"
	fi

	local failed=0

	step "Mirror hit counts (${MIRROR_NAMESPACE})"
	grade_hit_counts < <(collect_hit_counts) || failed=1

	step "Repositories each instance was asked for (plan §3.4's audit point)"
	print_requested_repositories

	echo
	if ((failed)); then
		echo "Q408 Phase 3 validation: FAIL — at least one instance served no pull," >&2
		echo "so a client is still reaching its upstream directly. Read its wiring in" >&2
		echo "deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml." >&2
		return 1
	fi
	echo "Q408: PASS — every declared instance served content."
	echo "This says the pulls rode the mirror, and the repositories above say which"
	echo "client rode which. It does not say the upstreams were unreachable: that is"
	echo "scripts/e2e/egress-negatives.sh, a step of the run itself on the Kata lane."
}

[[ -n "${E2E_MIRROR_HITS_LIB_ONLY:-}" ]] || main "$@"
