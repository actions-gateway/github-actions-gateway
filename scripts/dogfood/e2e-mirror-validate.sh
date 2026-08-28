#!/usr/bin/env bash
#
# Q408 Phase 2 validation: does the in-cluster registry pull-through cache
# actually serve? Applies to a cluster `e2e-start.sh` has already brought up —
# this script only reads and probes, it never applies a manifest.
#
# Why a script rather than a hand-typed curl session: Q408 Phase 3 re-runs the
# same battery once the clients are wired, and Phase 4 re-runs it again under the
# tight policy. A booked dogfood session is the scarce resource in all three, so
# the battery is a command rather than a transcript.
#
# WHAT THIS COVERS AND WHAT IT DOES NOT. The plan's §3.6 build notes were all
# taken against the image locally with `docker`, so nothing cluster-side —
# scheduling, probes, the policies, volume permissions — was covered. This closes
# that half for the mirror's own service:
#
#   available   every declared instance's Deployment reports Available.
#   v2          GET /v2/ answers 200 through the Service.
#   manifest    a real upstream manifest fetch answers 200. THIS is the check
#               that matters: a mirror whose storage root is unwritable answers
#               200 on /v2/ and 500 on every pull (measured, §3.6 — the fsGroup
#               finding), so the `v2` check alone grades a broken mirror green.
#   push        POST /v2/<name>/blobs/uploads/ is refused. §3.1's read-only
#               property, measured on the cluster rather than argued.
#   debug       the bundled config's :5001 pprof + /metrics listener is unbound.
#
# It does NOT prove enforcement. The probe pod runs in the tenant namespace
# carrying the workload label, so it rides the same NetworkPolicy pair a worker
# does — which exercises the mirror-side `registry-mirror-worker-access` ingress
# rule for real — but the Kata overlay's allow-all `e2e-open-egress` is still in
# place until Q408 Phase 4, so reachability here does not distinguish the mirror
# path from the open one. Enforcement is Phase 4's negatives.
#
# Required env vars (export before running):
#   PROJECT   GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-east1-b)
#
# Optional:
#   MIRROR_NAMESPACE  namespace holding the instances (default: gag-registry-mirror)
#   TENANT_NAMESPACE  namespace the probe pod runs in, which must be the one
#                     `registry-mirror-worker-access` admits (default:
#                     gag-dogfood-e2e)
#   PROBE_TIMEOUT     seconds to wait for the probe pod to finish (default: 180)
#
# Exit: 0 when every check passes, 1 on any failure — including a check that
# produced no result at all, which is the failure mode a battery like this hides
# best (see grade_probe_output).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

MIRROR_NAMESPACE="${MIRROR_NAMESPACE:-gag-registry-mirror}"
TENANT_NAMESPACE="${TENANT_NAMESPACE:-gag-dogfood-e2e}"
PROBE_TIMEOUT="${PROBE_TIMEOUT:-180}"
PROBE_POD="registry-mirror-probe"
PROBE_IMAGE="curlimages/curl:8.10.1"

# --- the instance table ------------------------------------------------------
#
# One row per upstream: <instance> <repository> <reference>. The instance set is
# the five of the measured e2e job-time inventory (plan §2.2 as corrected by
# §2.5); the refs are one real pull per upstream, taken from that same inventory
# so each probe exercises a path the suite actually walks.
#
# Kept as data rather than derived from the live Deployments on purpose: derived,
# a missing instance would shrink the battery instead of failing it, and every
# check would report green over four mirrors when the manifests declare five.
MIRROR_INSTANCES=(
	"mirror-docker-io library/alpine latest"
	"mirror-ghcr-io actions-gateway/gmc v1.2.0"
	"mirror-quay-io jetstack/cert-manager-controller v1.20.2"
	"mirror-registry-k8s-io metrics-server/metrics-server v0.8.1"
	"mirror-gcr-io distroless/static nonroot"
)

# The four checks each instance is graded on. `available` is read from the
# cluster by this script; the other four come back from the probe pod.
PROBE_CHECKS=(v2 manifest push debug)

# --- pure helpers (unit-tested; no kubectl, no cluster) ----------------------

# mirror_probe_script NAMESPACE — emit the shell program the probe pod runs,
# reading the instance table from MIRROR_INSTANCES. It prints one
# `<instance> <check> <value>` line per probe and nothing else, so the grader
# parses a fixed shape rather than curl's or the registry's prose.
#
# Every curl carries `|| true`: a connection refused by NetworkPolicy is a
# finding to report, not a reason for the pod to exit before the later probes
# run. The value carried is what discriminates — an HTTP status for the three
# HTTP probes, curl's own exit code for `debug`, where the pass condition is a
# connection that never establishes.
mirror_probe_script() {
	local ns="$1" row instance repo ref
	local accept='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
	echo 'set -u'
	# The `$(...)` below belong to the emitted program, not to this one, so the
	# single quotes are what keeps them unexpanded here.
	# shellcheck disable=SC2016
	for row in "${MIRROR_INSTANCES[@]}"; do
		read -r instance repo ref <<<"${row}"
		local host="${instance}.${ns}.svc.cluster.local:5000"
		printf 'echo "%s v2 $(curl -s -o /dev/null -w "%%{http_code}" --max-time 20 http://%s/v2/ || true)"\n' \
			"${instance}" "${host}"
		printf 'echo "%s manifest $(curl -s -o /dev/null -w "%%{http_code}" --max-time 60 -H "Accept: %s" http://%s/v2/%s/manifests/%s || true)"\n' \
			"${instance}" "${accept}" "${host}" "${repo}" "${ref}"
		printf 'echo "%s push $(curl -s -o /dev/null -w "%%{http_code}" --max-time 20 -X POST http://%s/v2/%s/blobs/uploads/ || true)"\n' \
			"${instance}" "${host}" "${repo}"
		# No -w here: what is being read is whether the connection establishes at
		# all, which is curl's exit status (7 == could not connect), not a status
		# line. A listener that answers anything makes this non-7 and fails.
		printf 'curl -s -o /dev/null --max-time 10 http://%s.%s.svc.cluster.local:5001/metrics; echo "%s debug $?"\n' \
			"${instance}" "${ns}" "${instance}"
	done
}

# grade_probe_output — read the probe pod's stdout on stdin and print one
# `PASS`/`FAIL <reason>` line per expected check, in table order. Exits 1 if any
# check failed OR did not report.
#
# The missing-line case is the whole reason this is a function rather than a
# grep. A probe pod that is evicted, times out, or dies before its first echo
# produces empty output, and a grader that walks what it received reports zero
# failures over it — a green verdict from an instrument that ran nothing. So the
# expected set is the table, not the transcript, and an absent line is a FAIL
# with its own reason.
grade_probe_output() {
	local -A seen=()
	local instance check value row repo ref expected got failed=0
	while read -r instance check value; do
		[[ -n "${instance}" ]] || continue
		seen["${instance} ${check}"]="${value}"
	done
	for row in "${MIRROR_INSTANCES[@]}"; do
		read -r instance repo ref <<<"${row}"
		for check in "${PROBE_CHECKS[@]}"; do
			got="${seen["${instance} ${check}"]:-}"
			if [[ -z "${got}" ]]; then
				echo "FAIL ${instance} ${check}: no result reported (the probe did not run this check)"
				failed=1
				continue
			fi
			case "${check}" in
			v2 | manifest) expected=200 ;;
			# 405 is what proxy mode answers, measured both locally (§3.6) and
			# independently of `delete.enabled`. Any 2xx here means the mirror
			# accepted an upload and §3.1's read-only property is false.
			push) expected=405 ;;
			# curl 7: connection refused / nothing listening.
			debug) expected=7 ;;
			esac
			if [[ "${got}" == "${expected}" ]]; then
				echo "PASS ${instance} ${check}"
			elif [[ "${check}" == "push" && "${got}" =~ ^2[0-9][0-9]$ ]]; then
				echo "FAIL ${instance} ${check}: upload ACCEPTED (${got}) — the mirror is not read-only"
				failed=1
			else
				echo "FAIL ${instance} ${check}: got ${got}, want ${expected}"
				failed=1
			fi
		done
	done
	return "${failed}"
}

# probe_pod_overrides — the pod-spec JSON that makes the probe ride a worker's
# network path: the workload label the tenant-side `e2e-mirror-egress` policy
# selects on. Compact JSON so it passes cleanly as --overrides.
probe_pod_overrides() {
	printf '{"metadata":{"labels":{"actions-gateway/component":"workload"}},"spec":{"restartPolicy":"Never"}}'
}

# --- cluster-side ------------------------------------------------------------

# check_instances_available — grade the `available` check for every declared
# instance off the Deployment's Available condition. Reads each instance by name
# rather than listing the label, for the reason the instance table gives: a
# listing shrinks the battery when an instance is missing instead of failing it.
check_instances_available() {
	local row instance repo ref status failed=0
	for row in "${MIRROR_INSTANCES[@]}"; do
		read -r instance repo ref <<<"${row}"
		status="$(kubectl get deployment "${instance}" --namespace "${MIRROR_NAMESPACE}" \
			-o 'jsonpath={.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || true)"
		if [[ "${status}" == "True" ]]; then
			echo "PASS ${instance} available"
		else
			echo "FAIL ${instance} available: Available=${status:-<absent>}"
			failed=1
		fi
	done
	return "${failed}"
}

# run_probe — run the probe pod to completion in the tenant namespace and print
# its stdout. A pod left over from a crashed prior run is removed first, so the
# script is safe to re-run; the delete after is unconditional for the same
# reason, and detached so a slow teardown does not hold the verdict.
#
# Read the logs rather than streaming an attached `-i --rm` pod: an interactive
# attach interleaves kubectl's own "pod deleted" notice with the container's
# stdout, which lands inside the lines the grader parses (the pattern
# scripts/dev/validate-egress-ip.sh records).
run_probe() {
	local script
	script="$(mirror_probe_script "${MIRROR_NAMESPACE}")"
	kubectl delete pod "${PROBE_POD}" --namespace "${TENANT_NAMESPACE}" \
		--ignore-not-found >&2
	kubectl run "${PROBE_POD}" --namespace "${TENANT_NAMESPACE}" \
		--image="${PROBE_IMAGE}" --restart=Never \
		--overrides="$(probe_pod_overrides)" \
		--command -- sh -c "${script}" >&2
	kubectl wait --namespace "${TENANT_NAMESPACE}" \
		--for=jsonpath='{.status.phase}'=Succeeded "pod/${PROBE_POD}" \
		--timeout="${PROBE_TIMEOUT}s" >&2 || true
	local logs
	logs="$(kubectl logs "${PROBE_POD}" --namespace "${TENANT_NAMESPACE}" 2>/dev/null || true)"
	# An empty transcript grades as twenty unreported checks, which is the right
	# verdict and a poor diagnosis. Print why the pod produced nothing while it
	# still exists: on a scarce dogfood session, ImagePullBackOff and a policy
	# that never admitted the probe are one `describe` apart and both look like
	# silence from here.
	if [[ -z "${logs}" ]]; then
		echo "The probe pod produced no output. Its status:" >&2
		kubectl get pod "${PROBE_POD}" --namespace "${TENANT_NAMESPACE}" \
			-o 'jsonpath={.status.phase}{"\n"}{range .status.containerStatuses[*]}{.state}{"\n"}{end}' >&2 || true
	fi
	printf '%s\n' "${logs}"
	kubectl delete pod "${PROBE_POD}" --namespace "${TENANT_NAMESPACE}" \
		--ignore-not-found --wait=false >&2 || true
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

	# The probe runs in the tenant namespace on purpose (see the header), so its
	# absence is a precondition failure with a named remedy rather than a check
	# that quietly grades nothing.
	if ! kubectl get namespace "${TENANT_NAMESPACE}" >/dev/null 2>&1; then
		die "namespace '${TENANT_NAMESPACE}' is absent — bring the e2e tenant up first (scripts/dogfood/e2e-start.sh)"
	fi

	local failed=0

	step "Instance availability (${MIRROR_NAMESPACE})"
	check_instances_available || failed=1

	step "Serving, read-only and debug-listener probes (from ${TENANT_NAMESPACE})"
	local probe_out
	probe_out="$(run_probe)"
	grade_probe_output <<<"${probe_out}" || failed=1

	echo
	if ((failed)); then
		echo "Q408 Phase 2 validation: FAIL" >&2
		return 1
	fi
	echo "Q408 Phase 2 validation: PASS — every declared instance serves, refuses uploads,"
	echo "and exposes no debug listener. Enforcement remains Phase 4's."
}

[[ -n "${E2E_MIRROR_VALIDATE_LIB_ONLY:-}" ]] || main "$@"
