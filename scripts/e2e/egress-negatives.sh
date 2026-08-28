#!/usr/bin/env bash
#
# egress-negatives.sh — prove, from inside the job, that the Kata worker's only
# registry path is the in-cluster mirror (Q408 Phase 4).
#
# Phase 3 wired the clients and proved they ride the mirror, by reading the
# mirror's own access log. That reading says nothing about enforcement: the
# tenant still carried an allow-all `e2e-open-egress` policy, so an upstream a
# client reached anyway was invisible. Phase 4 deletes that policy, and what
# separates the two paths is a probe that must FAIL.
#
# WHY IT RUNS IN THE JOB RATHER THAN FROM A PROBE POD. The Phase 2 battery
# (scripts/dogfood/e2e-mirror-validate.sh) probes from a pod carrying the
# workload label, which rides the same NetworkPolicy pair. That is the right
# instrument for the mirror's own service and the wrong one here: it is an
# ordinary pod, and the claim Phase 4 makes is about a Kata worker, whose
# dockerd runs inside a micro-VM guest and whose inner containers reach the
# network through a bridge NAT inside that guest. Whether policy still binds at
# the end of that path is exactly what a plain pod cannot answer. So the
# runner-container probes and one dockerd-level pair run here, in the job, on
# the worker whose posture is being claimed.
#
# EVERY NEGATIVE IS PAIRED WITH A POSITIVE, and that is the whole design. A
# battery of nothing-is-reachable checks passes identically when the pod has no
# network at all, when DNS is down, or when the step ran somewhere it was never
# meant to — a green verdict from an instrument that could not have failed. So
# `mirror-reachable` and `github-reachable` must pass for the blocked checks to
# mean anything, and `docker-mirror-pull` is what makes `docker-upstream-blocked`
# a statement about the upstream rather than about dockerd.
#
# WHAT A VALUE MEANS. Each probe reports one token:
#
#   200, 405, …   the HTTP status curl received, so the peer was reached
#   -             curl produced no status: connect timed out or was refused,
#                 which is what a dropped egress looks like
#   ok / fail     a `docker pull` exited zero, or did not
#
# A dropped packet has no error to distinguish it from a slow one, so a blocked
# probe is bounded by `--max-time` rather than by the peer. That is why this
# script takes minutes rather than seconds when the posture holds, and why the
# timeouts are named constants: shortening them makes a slow upstream look
# blocked, which is the one way this battery reports a false PASS.
#
# Usage:
#   scripts/e2e/egress-negatives.sh
#
# Environment:
#   REGISTRY_MIRRORS — <upstream>=<mirror> map (scripts/lib/registry-mirror.sh).
#                      Unset or empty means no mirror is wired and no tight
#                      policy is in place — the hosted lane and a developer's
#                      `make e2e` — so the battery reports a skip and exits 0.
#
# Exit: 0 when every check passes, 1 when any fails or reports nothing.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/registry-mirror.sh
source "${REPO_ROOT}/scripts/lib/registry-mirror.sh"

# Seconds a probe waits before calling a silent peer unreachable. Generous on
# purpose: a blocked probe always spends the whole budget, and an upstream that
# is merely slow must not be graded as blocked.
HTTP_TIMEOUT="${EGRESS_NEGATIVES_HTTP_TIMEOUT:-20}"
PULL_TIMEOUT="${EGRESS_NEGATIVES_PULL_TIMEOUT:-120}"

# The non-Hub upstream the two dockerd checks pull the same image from, once
# direct and once through the mirror. gcr.io because `distroless/static` is the
# smallest ref in the measured job-time inventory (plan §2.2) and because it is
# NOT Docker Hub: dockerd's `registry-mirrors` redirects Hub and only Hub, so a
# Hub ref would ride the mirror transparently and could never be the negative.
PROBE_UPSTREAM="gcr.io"
PROBE_REF="gcr.io/distroless/static:nonroot"

# A URL that is neither GitHub nor any registry — the plain-internet negative.
PROBE_INTERNET="https://example.com"

# The cloud metadata server. Reachable on port 53 alone once open egress is
# gone, via the managed policy's link-local DNS rule (`dnsEgressRule`,
# cmd/gmc/internal/controller/shared_networkpolicy.go), and the metadata service
# does not serve DNS. This probes port 80, which nothing admits.
PROBE_METADATA="http://169.254.169.254/computeMetadata/v1/"

# GitHub's API, the positive control for the one upstream that stays reachable:
# the managed workload policy admits the fetched GitHub CIDRs on 443.
PROBE_GITHUB="https://api.github.com/"

# --- pure helpers (unit-tested; no curl, no docker, no cluster) --------------

# expected_value CHECK — print the token CHECK must report to pass, or `-` for
# the blocked checks, whose expectation is the absence of an HTTP status.
expected_value() {
	case "$1" in
	mirror-reachable) printf '200' ;;
	# 405 is what a registry in proxy mode answers an upload attempt, measured
	# both locally (plan §3.6) and on the cluster by the Phase 2 battery. Any
	# 2xx here means the mirror accepted an upload and it is not read-only.
	mirror-readonly) printf '405' ;;
	github-reachable) printf 'http' ;;
	upstream-blocked | internet-blocked | metadata-blocked) printf '-' ;;
	docker-upstream-blocked) printf 'fail' ;;
	docker-mirror-pull) printf 'ok' ;;
	*) return 1 ;;
	esac
}

# The battery, in the order it runs and is graded. Controls first: a run that
# dies partway then reports the checks that make the rest legible.
NEGATIVE_CHECKS=(
	mirror-reachable
	github-reachable
	mirror-readonly
	docker-mirror-pull
	upstream-blocked
	internet-blocked
	metadata-blocked
	docker-upstream-blocked
)

# grade_negatives — read `<check> <value>` lines on stdin and print one
# PASS/FAIL line per DECLARED check, in table order. Exits 1 if any failed or
# did not report.
#
# The expected set is the table rather than the transcript, for the reason every
# battery in this plan gives: a probe that died before its first echo produces
# empty output, and a grader walking what it received finds nothing wrong.
grade_negatives() {
	local -A seen=()
	local check value want failed=0
	while read -r check value; do
		[[ -n "${check}" ]] || continue
		seen["${check}"]="${value}"
	done
	for check in "${NEGATIVE_CHECKS[@]}"; do
		value="${seen["${check}"]:-}"
		want="$(expected_value "${check}")"
		if [[ -z "${value}" ]]; then
			echo "FAIL ${check}: no result reported (the probe did not run)"
			failed=1
			continue
		fi
		case "${want}" in
		# Any HTTP status proves the peer answered; which one is the API's
		# opinion, not the network's.
		http)
			if [[ "${value}" == "-" ]]; then
				echo "FAIL ${check}: no HTTP status — GitHub is unreachable, so every blocked check below is unfalsifiable"
				failed=1
			else
				echo "PASS ${check}: HTTP ${value}"
			fi
			;;
		-)
			if [[ "${value}" == "-" ]]; then
				echo "PASS ${check}: no HTTP status, as required"
			else
				echo "FAIL ${check}: answered HTTP ${value} — this destination is REACHABLE and the posture is not enforced"
				failed=1
			fi
			;;
		*)
			if [[ "${value}" == "${want}" ]]; then
				echo "PASS ${check}"
			else
				echo "FAIL ${check}: got ${value}, want ${want}"
				failed=1
			fi
			;;
		esac
	done
	return "${failed}"
}

# --- probes ------------------------------------------------------------------

# http_probe URL [METHOD] — print the HTTP status curl received, or `-` when it
# received none. Never fails the script: a probe that cannot connect is the
# finding, not an error.
http_probe() {
	local url="$1" method="${2:-GET}" code
	code="$(curl -s -o /dev/null -w '%{http_code}' --max-time "${HTTP_TIMEOUT}" \
		-X "${method}" "${url}" 2>/dev/null || true)"
	# curl prints 000 when it never got a response line, and nothing at all when
	# the invocation itself failed. Both mean the same thing here.
	[[ -n "${code}" && "${code}" != "000" ]] || code='-'
	printf '%s' "${code}"
}

# pull_probe REF — print `ok` when `docker pull REF` succeeds, `fail` otherwise.
# Bounded by `timeout`: a pull whose registry is dropped rather than refused has
# no error to return and would otherwise sit in dockerd's own retry schedule.
pull_probe() {
	local ref="$1"
	if timeout "${PULL_TIMEOUT}" docker pull "${ref}" >/dev/null 2>&1; then
		printf 'ok'
	else
		printf 'fail'
	fi
}

# run_probes HUB_MIRROR MIRRORED_REF — emit one `<check> <value>` line per check.
run_probes() {
	local hub="$1" mirrored="$2"
	printf 'mirror-reachable %s\n' "$(http_probe "http://${hub}/v2/")"
	printf 'github-reachable %s\n' "$(http_probe "${PROBE_GITHUB}")"
	printf 'mirror-readonly %s\n' \
		"$(http_probe "http://${hub}/v2/library/alpine/blobs/uploads/" POST)"
	printf 'docker-mirror-pull %s\n' "$(pull_probe "${mirrored}")"
	printf 'upstream-blocked %s\n' "$(http_probe "https://${PROBE_UPSTREAM}/v2/")"
	printf 'internet-blocked %s\n' "$(http_probe "${PROBE_INTERNET}")"
	printf 'metadata-blocked %s\n' "$(http_probe "${PROBE_METADATA}")"
	printf 'docker-upstream-blocked %s\n' "$(pull_probe "${PROBE_REF}")"
}

main() {
	if [[ -z "${REGISTRY_MIRRORS:-}" ]]; then
		echo "==> Q408 egress negatives: SKIPPED"
		echo "    REGISTRY_MIRRORS is unset, so no in-cluster mirror is wired and"
		echo "    no tight egress policy is in place. Nothing to enforce here."
		return 0
	fi

	local hub mirrored
	hub="$(mirror_for docker.io)"
	[[ -n "${hub}" ]] || {
		echo "ERROR: REGISTRY_MIRRORS names no docker.io mirror, so the reachability" >&2
		echo "       control cannot run and every negative below would be unfalsifiable." >&2
		return 1
	}
	[[ -n "$(mirror_for "${PROBE_UPSTREAM}")" ]] || {
		echo "ERROR: REGISTRY_MIRRORS names no ${PROBE_UPSTREAM} mirror, so the pull pair" >&2
		echo "       has no mirrored half to control the blocked half against." >&2
		return 1
	}
	mirrored="$(mirror_rewrite "${PROBE_REF}")"

	echo "==> Q408 Phase 4 egress negatives"
	echo "    mirror:   ${hub}"
	echo "    pull via: ${mirrored}"
	echo "    A blocked probe spends its whole timeout, so this takes minutes."
	echo

	local failed=0
	grade_negatives < <(run_probes "${hub}" "${mirrored}") || failed=1

	echo
	if ((failed)); then
		echo "Q408 Phase 4 enforcement: FAIL" >&2
		echo "A reachable destination means the worker still has a path that is not the" >&2
		echo "mirror; an unreachable control means this battery proved nothing. Either" >&2
		echo "way, read deploy/dogfood-e2e/overlays/kata/ and the tenant's policies." >&2
		return 1
	fi
	echo "Q408 Phase 4 enforcement: PASS — the mirror and GitHub are reachable,"
	echo "the mirror refuses uploads, and the upstreams, the plain internet and the"
	echo "metadata server are not reachable from this worker."
}

[[ -n "${EGRESS_NEGATIVES_LIB_ONLY:-}" ]] || main "$@"
