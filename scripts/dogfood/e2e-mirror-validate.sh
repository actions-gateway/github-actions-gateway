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
#   debug       REGISTRY_HTTP_DEBUG_ADDR is set and empty in the pod spec, which
#               is what unbinds the bundled config's :5001 pprof + /metrics
#               listener.
#
# `debug` is read off the object rather than probed over the network, and the
# network is the reading that cannot work. Each Service declares one port
# (5000/5000) and `registry-mirror-worker-access` admits only TCP/5000, so a
# connection to a ClusterIP on 5001 never reaches the pod whatever the listener
# is doing: the dataplane decides the result, so the probe grades the Service
# and the policy rather than the config it names. Which way it fails is
# unmeasured and both ways are wrong: an unmatched ClusterIP port that is dropped
# times out, failing a healthy cluster, and one that is rejected gives the same
# refusal a probe scores as healthy whether or not the listener is bound.
#
# The other observable reading is an ephemeral container in the pod's own netns
# (`kubectl debug --target`). Not taken: this namespace enforces PSA
# `restricted`, `kubectl debug` sets no securityContext on the container it
# injects, and whether that is admitted is a venue question no run here can
# settle. An object read has no venue.
#
# It does NOT prove enforcement, and not because the probe pod is the wrong
# vehicle: it runs in the tenant namespace carrying the workload label, so it
# rides the same NetworkPolicy pair a worker does and exercises the mirror-side
# `registry-mirror-worker-access` ingress rule for real. What it cannot show is a
# path that is NOT here — every check above is a reachability check, and the
# posture is a claim about what is unreachable. That half is
# scripts/e2e/egress-negatives.sh, which runs inside the job because the claim is
# about a Kata worker and this pod is not one (Q408 Phase 4).
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

# The three checks the probe pod reports. `available` and `debug` are read from
# the Deployment object by this script instead, so they are graded separately.
PROBE_CHECKS=(v2 manifest push)

# The env var whose empty value unbinds the image's bundled :5001 debug listener.
# Read by name AND value: kubectl's jsonpath renders an empty value and an absent
# entry identically, and an absent entry means the listener is bound.
DEBUG_ADDR_VAR="REGISTRY_HTTP_DEBUG_ADDR"

# The container the env var is read from, selected by name so a prepended sidecar
# cannot shift it. All five Deployments name it `registry`.
MIRROR_CONTAINER="registry"

# --- pure helpers (unit-tested; no kubectl, no cluster) ----------------------

# mirror_probe_script NAMESPACE — emit the shell program the probe pod runs,
# reading the instance table from MIRROR_INSTANCES. It prints one
# `<instance> <check> <value>` line per probe and nothing else, so the grader
# parses a fixed shape rather than curl's or the registry's prose.
#
# Every curl carries `|| true`: a connection refused by NetworkPolicy is a
# finding to report, not a reason for the pod to exit before the later probes
# run. Every probe here addresses 5000 and carries an HTTP status, so a
# connection the dataplane never completes surfaces as an empty value, which the
# grader reports as an unreported check rather than as a status.
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
			esac
			if [[ "${got}" == "${expected}" ]]; then
				echo "PASS ${instance} ${check}"
			elif [[ "${check}" == "push" && "${got}" =~ ^2[0-9][0-9]$ ]]; then
				echo "FAIL ${instance} ${check}: upload ACCEPTED (${got}) — the mirror is not read-only"
				failed=1
			elif [[ "${got}" == 429 ]]; then
				# Anonymous pull-through shares the mirror's egress IP, so Docker
				# Hub's per-IP limit can answer here. It is a rate limit rather
				# than a broken mirror, and the code alone does not say so.
				echo "FAIL ${instance} ${check}: got 429, want ${expected} — upstream rate limit, not a mirror fault; retry, or set proxy.username/password"
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

# check_instance_objects — grade the two checks that are properties of the
# Deployment rather than of a request: `available` and `debug`. Reads each
# instance by name rather than listing the label, for the reason the instance
# table gives: a listing shrinks the battery when an instance is missing instead
# of failing it.
#
# One read per instance, three fields, pipe-separated. Both the env name and its
# value are read because they answer different halves: an entry present with an
# empty value is what unbinds :5001, while no entry at all leaves the bundled
# config's listener bound. kubectl's jsonpath renders those identically on the
# value alone, so reading only the value would grade the dangerous state green.
# A kubectl that fails outright leaves all three empty, which fails both checks.
#
# The container is selected by name rather than by index: a sidecar prepended to
# the pod would shift index 0, and the env read would then come back empty and
# fail with a reason line describing a missing variable rather than the wrong
# container.
check_instance_objects() {
	local row instance repo ref raw status envname envvalue failed=0
	local c="containers[?(@.name==\"${MIRROR_CONTAINER}\")]"
	local jsonpath="jsonpath={.status.conditions[?(@.type==\"Available\")].status}"
	jsonpath+="|{.spec.template.spec.${c}.env[?(@.name==\"${DEBUG_ADDR_VAR}\")].name}"
	jsonpath+="|{.spec.template.spec.${c}.env[?(@.name==\"${DEBUG_ADDR_VAR}\")].value}"
	for row in "${MIRROR_INSTANCES[@]}"; do
		read -r instance repo ref <<<"${row}"
		raw="$(kubectl get deployment "${instance}" --namespace "${MIRROR_NAMESPACE}" \
			-o "${jsonpath}" 2>/dev/null || true)"
		IFS='|' read -r status envname envvalue <<<"${raw}"
		if [[ "${status}" == "True" ]]; then
			echo "PASS ${instance} available"
		else
			echo "FAIL ${instance} available: Available=${status:-<absent>}"
			failed=1
		fi
		if [[ -z "${envname}" ]]; then
			echo "FAIL ${instance} debug: ${DEBUG_ADDR_VAR} is not set, so the image's bundled :5001 listener is bound"
			failed=1
		elif [[ -n "${envvalue}" ]]; then
			echo "FAIL ${instance} debug: ${DEBUG_ADDR_VAR}=${envvalue}, want empty"
			failed=1
		else
			echo "PASS ${instance} debug"
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

	# Both namespaces get a precondition with a named remedy rather than a check
	# that grades nothing: a typo in either produces a well-formed all-FAIL run
	# whose reason lines describe the wrong problem.
	if ! kubectl get namespace "${MIRROR_NAMESPACE}" >/dev/null 2>&1; then
		die "namespace '${MIRROR_NAMESPACE}' is absent — apply the mirror first (scripts/dogfood/e2e-start.sh)"
	fi

	local failed=0

	step "Instance availability and debug-listener config (${MIRROR_NAMESPACE})"
	check_instance_objects || failed=1

	step "Serving and read-only probes (from ${TENANT_NAMESPACE})"
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
