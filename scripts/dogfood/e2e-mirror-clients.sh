#!/usr/bin/env bash
#
# Who actually connects to the registry mirrors, and does every one of them carry
# the workload label? The reading Q1026 asks for, taken BEFORE the shared
# topology's ingress peer is narrowed to `actions-gateway/component: workload`.
#
# WHY IT HAS TO BE TAKEN AT ALL. The narrowing is fail-closed and total: a client
# that does not carry the label loses the path entirely and cannot pull, and the
# four wired clients read four separate configurations
# (deploy/registry-mirror/README.md#how-the-job-is-wired-to-these-instances), so
# a source read that finds them all labelled has checked the clients somebody
# thought of. This checks the clients that connected.
#
# WHERE THE SOURCE ADDRESS LIVES. In the deny proxy's log, not the registry's.
# Every mirror pod fronts its registry with the `catalog-deny` container (Q1022),
# so the registry's own access log records 127.0.0.1 for every request and the
# client address survives only in the proxy's. That is also why this is a
# separate script from e2e-mirror-hits.sh, which reads the other container to
# answer a different question — what was fetched, not by whom.
#
# WHAT THE VERDICT MEANS, AND WHAT IT CANNOT MEAN. A pod that has since been
# deleted does not resolve, and a worker pod is deleted at the end of every job.
# An unresolved address is therefore the ORDINARY case for a window that has
# finished, and grading it green would make this script report safety it never
# measured. So an unresolved address is a refusal (exit 2), not a pass: run this
# while the workers are still up, or accept that the reading was not taken.
#
# The kubelet's readiness and liveness probes reach 5000 from the NODE address on
# GKE Dataplane V2, so node addresses are expected and exempt. They are resolved
# rather than pattern-matched: an address assumed to be a node is an address not
# checked.
#
# Required env vars (export before running):
#   PROJECT   GCP project ID
#   CLUSTER   GKE cluster name
#   ZONE      GCP zone
#
# Optional:
#   MIRROR_NAMESPACE  namespace holding the instances (default: gag-registry-mirror)
#
# Exit: 0 every client that connected is a workload-labelled pod (or the kubelet),
# 1 at least one is not, 2 a reading that could not be taken — including an
# address that resolves to nothing, and a window in which no client connected at
# all, which grades nothing and must not read as safe.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

MIRROR_NAMESPACE="${MIRROR_NAMESPACE:-gag-registry-mirror}"

# The instance set, as data rather than derived from the live Deployments, for
# the reason e2e-mirror-validate.sh gives: derived, a missing instance shrinks
# the reading instead of failing it. That only holds because every instance is
# graded BY NAME below -- an earlier version pooled all five through one
# `sort -u`, and a four-of-five read was then byte-identical to a five-of-five
# one.
MIRROR_INSTANCES=(
	mirror-docker-io
	mirror-ghcr-io
	mirror-quay-io
	mirror-registry-k8s-io
	mirror-gcr-io
)

# The container holding the client addresses, named rather than left to kubectl's
# default: an unqualified `kubectl logs` picks by position and would read the
# registry, whose every client is 127.0.0.1.
PROXY_CONTAINER="catalog-deny"

# The label the narrowed ingress peer requires, and the one the worker-side
# egress policy already selects.
WORKLOAD_LABEL="actions-gateway/component=workload"

# --- pure helpers (unit-tested; no kubectl, no cluster) ----------------------

# client_addresses — read a HAProxy log on stdin, print each distinct client
# address. The first field is <address>:<port>, and the port is stripped from the
# right so an IPv6 address keeps its colons:
#
#   10.4.2.17:51234 [29/Aug/2026:07:02:29.359] mirror registry/local 0/0/0/2/2 200 …
#
# Lines that do not begin with an address are the proxy's own NOTICE/WARNING
# startup output and are skipped rather than parsed, so a config warning cannot
# enter the client set.
client_addresses() {
	awk '
		$1 ~ /^\[?[0-9a-fA-F.:]+\]?:[0-9]+$/ {
			addr = $1
			sub(/:[0-9]+$/, "", addr)
			gsub(/^\[|\]$/, "", addr)
			print addr
		}' | sort -u
}

# grade_clients — read `<address> <kind> <detail>` on stdin and print one verdict
# line per address, in the order given. Sets no state; the caller reads the exit
# status.
#
#   pod-workload   <ns>/<name>   the narrowing keeps this client
#   pod-unlabelled <ns>/<name>   the narrowing CUTS THIS CLIENT OFF
#   node           <name>        the kubelet's probes, exempt
#   unresolved     -             cannot be graded either way
#
# Exit 1 when any client is unlabelled, 2 when any is unresolved or nothing was
# read at all. 1 beats 2: an unlabelled client is a finding, and a finding is
# worth more than the refusal beside it.
grade_clients() {
	local addr kind detail unlabelled=0 unresolved=0 graded=0
	while read -r addr kind detail; do
		[[ -n "${addr}" ]] || continue
		graded=$((graded + 1))
		case "${kind}" in
		pod-workload)
			echo "OK      ${addr} ${detail} carries ${WORKLOAD_LABEL}"
			;;
		pod-unlabelled)
			echo "FAIL    ${addr} ${detail} does NOT carry ${WORKLOAD_LABEL} — narrowing the ingress peer cuts this client off entirely"
			unlabelled=1
			;;
		node)
			echo "EXEMPT  ${addr} node/${detail} — the kubelet's probes, which no pod selector governs"
			;;
		ambiguous)
			echo "REFUSE  ${addr} ${detail} — a hostNetwork pod shares this node's address, so whether the narrowing keeps this client is not decidable from here"
			unresolved=1
			;;
		*)
			echo "REFUSE  ${addr} resolves to no pod and no node — most likely a worker that has already been deleted, so this client cannot be graded"
			unresolved=1
			;;
		esac
	done
	if ((graded == 0)); then
		echo "REFUSE  no client connected to any mirror in this window, so nothing was graded"
		return 2
	fi
	((unlabelled)) && return 1
	((unresolved)) && return 2
	return 0
}

# --- cluster-side ------------------------------------------------------------

# collect_client_addresses — emit two record kinds, one line each:
#
#   read   <instance> ok|failed   whether that instance's proxy log was readable
#   client <instance> <address>   one per distinct address that instance saw
#
# The `read` record is the whole point, and keying it on `kubectl logs`' EXIT
# STATUS rather than on an empty address list is what makes it usable. An
# instance that was read and saw nobody is legitimate -- not every mirror gets
# traffic in every window -- while an instance that could not be read at all is
# a hole in the reading. An empty address list cannot tell those apart, and the
# earlier version could not either: it pooled all five instances through one
# `sort -u`, so one unreadable log left no trace whatever and a four-of-five
# read produced byte-identical output to a five-of-five one.
collect_client_addresses() {
	local instance logs rc addr
	for instance in "${MIRROR_INSTANCES[@]}"; do
		set +e
		logs="$(kubectl logs "deployment/${instance}" --namespace "${MIRROR_NAMESPACE}" \
			--container "${PROXY_CONTAINER}" --tail=-1 2>/dev/null)"
		rc=$?
		set -e
		if ((rc != 0)); then
			printf 'read %s failed\n' "${instance}"
			continue
		fi
		printf 'read %s ok\n' "${instance}"
		while read -r addr; do
			[[ -n "${addr}" ]] || continue
			printf 'client %s %s\n' "${instance}" "${addr}"
		done < <(client_addresses <<<"${logs}")
	done
}

# grade_instance_reads — read collect_client_addresses' records and report one
# line per DECLARED instance, refusing for any whose log was not read.
#
# The expected set is the table rather than the transcript, for the reason every
# battery here gives: an instance that produced no record at all is the same
# hole as one that produced `failed`, and walking what arrived would find
# neither. Returns 2 when any instance is unread, which is a refusal and never a
# verdict about labels.
grade_instance_reads() {
	local kind instance state seen_line unread=0
	local -A state_of=()
	while read -r kind instance state; do
		[[ "${kind}" == "read" ]] || continue
		state_of["${instance}"]="${state}"
	done
	for instance in "${MIRROR_INSTANCES[@]}"; do
		seen_line="${state_of["${instance}"]:-}"
		case "${seen_line}" in
		ok) echo "READ    ${instance}" ;;
		failed)
			echo "REFUSE  ${instance}: its ${PROXY_CONTAINER} log could not be read, so any client that reached only this mirror is invisible to this reading"
			unread=1
			;;
		*)
			echo "REFUSE  ${instance}: no read was attempted or reported at all"
			unread=1
			;;
		esac
	done
	((unread)) && return 2
	return 0
}

# resolve_addresses — read addresses on stdin, print `<address> <kind> <detail>`.
# Two cluster reads, both taken once: every pod IP with its component label, and
# every node address. A pod whose label is absent renders as an empty third
# field, which is what distinguishes pod-unlabelled from pod-workload.
#
# BOTH LOOKUPS ARE TAKEN, and a collision is reported rather than resolved by
# ordering. A hostNetwork pod's `status.podIP` IS its node's address, so an
# address can genuinely match both, and neither answer is right: as a pod it
# would be graded on a label that no selector can usefully apply to it, and as a
# node it would be waved through as the kubelet. Checking pods first would have
# graded the kubelet's own probes as an unlabelled pod wherever a hostNetwork
# pod holds that address, which on a Dataplane V2 cluster is routine. Whichever
# way it were ordered the wrong answer would be silent, so the collision is its
# own kind and a human settles it.
resolve_addresses() {
	local pods nodes addr pod_line node_line
	pods="$(kubectl get pods --all-namespaces \
		-o 'jsonpath={range .items[*]}{.status.podIP}{" "}{.metadata.namespace}/{.metadata.name}{" "}{.metadata.labels.actions-gateway\/component}{"\n"}{end}' 2>/dev/null || true)"
	nodes="$(kubectl get nodes \
		-o 'jsonpath={range .items[*]}{range .status.addresses[*]}{.address}{" "}{end}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
	while read -r addr; do
		[[ -n "${addr}" ]] || continue
		pod_line="$(awk -v a="${addr}" '$1 == a { print $2 " " $3; exit }' <<<"${pods}")"
		node_line="$(awk -v a="${addr}" '{ for (i = 1; i < NF; i++) if ($i == a) { print $NF; exit } }' <<<"${nodes}")"
		local name component
		read -r name component <<<"${pod_line}"
		if [[ -n "${pod_line}" && -n "${node_line}" ]]; then
			printf '%s ambiguous %s+node/%s\n' "${addr}" "${name}" "${node_line}"
		elif [[ -n "${pod_line}" ]]; then
			if [[ "${component}" == "workload" ]]; then
				printf '%s pod-workload %s\n' "${addr}" "${name}"
			else
				printf '%s pod-unlabelled %s\n' "${addr}" "${name}"
			fi
		elif [[ -n "${node_line}" ]]; then
			printf '%s node %s\n' "${addr}" "${node_line}"
		else
			printf '%s unresolved -\n' "${addr}"
		fi
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
		die "namespace '${MIRROR_NAMESPACE}' is absent — apply the mirror first (scripts/dogfood/e2e-start.sh)"
	fi

	step "Proxy logs read, per declared instance (${MIRROR_NAMESPACE})"
	local records read_rc=0 client_rc=0 rc=0
	records="$(collect_client_addresses)"
	grade_instance_reads <<<"${records}" || read_rc=$?

	step "Clients that reached the mirrors (${PROXY_CONTAINER} logs)"
	local addresses resolved
	addresses="$(awk '$1 == "client" { print $3 }' <<<"${records}" | sort -u)"
	resolved="$(resolve_addresses <<<"${addresses}")"
	grade_clients <<<"${resolved}" || client_rc=$?

	# An unlabelled client outranks any refusal beside it: it is the finding this
	# whole reading exists for, and one unread instance must not bury it.
	if ((client_rc == 1)); then
		rc=1
	elif ((read_rc != 0 || client_rc != 0)); then
		rc=2
	fi

	echo
	case "${rc}" in
	0)
		echo "Every client that reached a mirror is a workload-labelled pod or the kubelet."
		echo "Narrowing the shared component's ingress peer to that label cuts nothing off"
		echo "ON THIS READING, which covers the clients that connected while it was taken."
		;;
	1)
		echo "At least one client would lose the path. Do NOT narrow the ingress peer" >&2
		echo "until it carries ${WORKLOAD_LABEL}." >&2
		;;
	*)
		echo "The reading was not taken. Re-run while the workers are still up." >&2
		;;
	esac
	return "${rc}"
}

[[ -n "${E2E_MIRROR_CLIENTS_LIB_ONLY:-}" ]] || main "$@"
