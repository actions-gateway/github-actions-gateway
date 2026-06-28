#!/usr/bin/env bash
#
# Pre-install cluster preflight (Q184). Validates that the target cluster can
# actually uphold the tenant-isolation guarantees the GMC/AGC depend on BEFORE
# `helm install`, surfacing the dangerous silent failure modes loudly:
#
#   1. CNI NetworkPolicy ENFORCEMENT (the critical one). A non-enforcing CNI
#      (kindnet's kube-network-policies does not drop egress) silently voids
#      tenant isolation: every NetworkPolicy the chart installs is inert, so
#      tenants are NOT confined. This is a HARD FAIL — the loudest check.
#   2. Kubernetes server version >= 1.30 (the GA ValidatingAdmissionPolicy API
#      the GMC's namespace-psa-guard / tenant-resource-guard policies need).
#      HARD FAIL below 1.30.
#   3. cert-manager present (issues the webhook serving cert under the default
#      certManager.enabled=true). WARN — an install with certManager.enabled=false
#      uses the chart's self-signed fallback and does not need it.
#   4. metrics-server present (the resource metrics the GMC/AGC HPAs consume).
#      WARN — install succeeds without it; autoscaling stays degraded until it
#      is present.
#
# Backs `make validate-cluster`. Detection-based by design: it classifies the
# installed CNI rather than scheduling a live deny-policy probe, so it needs no
# workload images, no RBAC to create pods, and is safe to run against a fresh
# cluster. The pure classification/parsing helpers (classify_cni,
# parse_k8s_minor, k8s_meets_min) are unit-tested by
# scripts/validate-cluster-test.sh under `make check`.
#
# Env:
#   KUBECTL                 kubectl binary to use (default: kubectl on PATH).
#   VALIDATE_STRICT         when "1"/"true", treat WARN checks as failures too
#                           (exit non-zero on any WARN). Default: only FAIL fails.
#
# Exit: 0 when every check passes (or only warns, unless VALIDATE_STRICT); 1 when
# any check hard-fails, when VALIDATE_STRICT and any check warns, or when the
# cluster is unreachable (a preflight that cannot reach the cluster has failed).
set -euo pipefail

# --- pure helpers (unit-tested; no kubectl, no cluster) ----------------------

# classify_cni — read newline-separated DaemonSet names on stdin (one per line,
# basename only) and print one verdict word to stdout:
#   pass <name>   an enforcing CNI is present (Calico, Cilium, Antrea, Weave,
#                 kube-router, Canal, or GKE Dataplane V2's `anetd`) —
#                 NetworkPolicy egress/ingress is enforced.
#   fail <name>   kindnet (and no enforcing CNI) — NetworkPolicy is INERT.
#   warn unknown  no recognised CNI DaemonSet — cannot determine enforcement.
# An enforcing CNI wins over kindnet if both somehow appear (a Calico install on
# kind replaces kindnet, but be defensive). Matching is case-insensitive and on
# the whole name so `calico-node`, `cilium`, `antrea-agent` etc. all match.
# GKE Dataplane V2 runs Cilium under the DaemonSet name `anetd` (size-suffixed
# variants like `anetd-l` too); plain `netd` (non-Dataplane-V2 GKE) is NOT
# matched — those clusters enforce NetworkPolicy via Calico (`calico-node`).
classify_cni() {
	local names
	names="$(tr '[:upper:]' '[:lower:]')"
	# Enforcing CNIs first: any one present means NetworkPolicy is enforced.
	if grep -qE '(^|[^a-z])(calico-node|canal)([^a-z]|$)' <<<"$names"; then
		echo "pass Calico"
	elif grep -qE '(^|[^a-z])cilium([^a-z]|$)' <<<"$names"; then
		echo "pass Cilium"
	elif grep -qE '(^|[^a-z])anetd([^a-z]|$)' <<<"$names"; then
		echo "pass GKE Dataplane V2 (Cilium)"
	elif grep -qE '(^|[^a-z])antrea' <<<"$names"; then
		echo "pass Antrea"
	elif grep -qE '(^|[^a-z])weave' <<<"$names"; then
		echo "pass Weave Net"
	elif grep -qE '(^|[^a-z])kube-router([^a-z]|$)' <<<"$names"; then
		echo "pass kube-router"
	elif grep -qE '(^|[^a-z])kindnet' <<<"$names"; then
		echo "fail kindnet"
	else
		echo "warn unknown"
	fi
}

# parse_k8s_minor — parse a Kubernetes gitVersion ("v1.30.2", "v1.29.4-gke.100",
# "v1.31.0+rke2r1") and print "MAJOR MINOR". Returns 1 if unparseable.
parse_k8s_minor() {
	local v="${1#v}"
	# Drop any pre-release / build metadata suffix (-... or +...).
	v="${v%%[-+]*}"
	# A bare "major" with no minor (e.g. "v1") is not a usable version.
	[[ "$v" == *.* ]] || return 1
	local major minor rest
	major="${v%%.*}"
	rest="${v#*.}"
	minor="${rest%%.*}"
	[[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ ]] || return 1
	printf '%s %s' "$major" "$minor"
}

# k8s_meets_min MAJOR MINOR REQ_MAJOR REQ_MINOR — exit 0 if (MAJOR,MINOR) is at
# least (REQ_MAJOR,REQ_MINOR), else 1.
k8s_meets_min() {
	local maj="$1" min="$2" rmaj="$3" rmin="$4"
	if ((maj > rmaj)); then return 0; fi
	if ((maj < rmaj)); then return 1; fi
	((min >= rmin))
}

# --- reporting ---------------------------------------------------------------

MIN_K8S_MAJOR=1
MIN_K8S_MINOR=30

# Counters for the final summary / exit code.
n_fail=0
n_warn=0

# report VERDICT CHECK MESSAGE [REMEDIATION] — print one aligned result line and
# tally the verdict. VERDICT is pass|warn|fail.
report() {
	local verdict="$1" check="$2" msg="$3" remediation="${4:-}"
	local tag
	case "$verdict" in
	pass) tag="PASS" ;;
	warn)
		tag="WARN"
		n_warn=$((n_warn + 1))
		;;
	fail)
		tag="FAIL"
		n_fail=$((n_fail + 1))
		;;
	*) tag="????" ;;
	esac
	printf '  [%s] %-18s %s\n' "$tag" "$check" "$msg"
	[[ -n "$remediation" ]] && printf '         ↳ %s\n' "$remediation"
	return 0
}

# --- live checks (kubectl) ---------------------------------------------------

# check_cni — classify the cluster CNI from its DaemonSets. The critical check:
# a non-enforcing CNI silently voids every NetworkPolicy the chart ships.
check_cni() {
	local ds verdict name
	if ! ds="$("$KUBECTL" get daemonsets -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)"; then
		report warn "CNI enforcement" "could not list DaemonSets to detect the CNI" \
			"Ensure kubectl can reach the cluster, then re-run; verify the CNI enforces NetworkPolicy."
		return 0
	fi
	read -r verdict name <<<"$(classify_cni <<<"$ds")"
	case "$verdict" in
	pass)
		report pass "CNI enforcement" "$name detected — NetworkPolicy is enforced"
		;;
	fail)
		report fail "CNI enforcement" \
			"$name detected — it does NOT enforce NetworkPolicy egress; tenant isolation would be SILENTLY VOID" \
			"Install on a cluster with an enforcing CNI (Calico, Cilium). See docs/operations/install.md#prerequisites."
		;;
	*)
		report warn "CNI enforcement" \
			"no recognised CNI DaemonSet found — could not confirm NetworkPolicy enforcement" \
			"Confirm your CNI enforces NetworkPolicy (Calico/Cilium do; kindnet does NOT) before relying on tenant isolation."
		;;
	esac
}

# check_k8s_version — assert the server is >= 1.30 (GA ValidatingAdmissionPolicy).
check_k8s_version() {
	local raw parsed maj min
	if ! raw="$("$KUBECTL" version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // empty')" || [[ -z "$raw" ]]; then
		report warn "K8s version" "could not read the server version" \
			"Ensure kubectl can reach the cluster; the GMC needs Kubernetes >= ${MIN_K8S_MAJOR}.${MIN_K8S_MINOR}."
		return 0
	fi
	if ! parsed="$(parse_k8s_minor "$raw")"; then
		report warn "K8s version" "could not parse server version '$raw'" \
			"Verify the server is >= ${MIN_K8S_MAJOR}.${MIN_K8S_MINOR}."
		return 0
	fi
	read -r maj min <<<"$parsed"
	if k8s_meets_min "$maj" "$min" "$MIN_K8S_MAJOR" "$MIN_K8S_MINOR"; then
		report pass "K8s version" "server is $raw (>= ${MIN_K8S_MAJOR}.${MIN_K8S_MINOR})"
	else
		report fail "K8s version" \
			"server is $raw — below the required ${MIN_K8S_MAJOR}.${MIN_K8S_MINOR}" \
			"The GMC admission policies need the GA ValidatingAdmissionPolicy API (1.30+). Upgrade the cluster."
	fi
}

# check_cert_manager — cert-manager issues the webhook serving cert under the
# default certManager.enabled=true. WARN, not FAIL: certManager.enabled=false
# uses the chart's self-signed fallback and needs none of this.
check_cert_manager() {
	if ! "$KUBECTL" get crd certificates.cert-manager.io >/dev/null 2>&1; then
		report warn "cert-manager" "cert-manager CRDs not found" \
			"Install cert-manager (https://cert-manager.io) for the default cert path, or install with --set certManager.enabled=false."
		return 0
	fi
	# CRDs present — confirm the webhook is actually serving (Available).
	if "$KUBECTL" get deploy -A -l app.kubernetes.io/name=webhook,app.kubernetes.io/instance=cert-manager \
		-o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Available")].status}{"\n"}{end}' 2>/dev/null | grep -q '^True$'; then
		report pass "cert-manager" "installed and the webhook is Available"
	else
		report warn "cert-manager" "CRDs present but the webhook is not reporting Available yet" \
			"Wait for the cert-manager-webhook Deployment to become Available before installing the chart."
	fi
}

# check_metrics_server — the resource metrics the GMC/AGC HPAs consume. WARN:
# install succeeds without it; autoscaling stays degraded until present.
check_metrics_server() {
	if "$KUBECTL" get apiservice v1beta1.metrics.k8s.io \
		-o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null | grep -q '^True$'; then
		report pass "metrics-server" "metrics.k8s.io API is Available"
	else
		report warn "metrics-server" "metrics.k8s.io API is not Available" \
			"Install metrics-server (https://github.com/kubernetes-sigs/metrics-server) so the GMC/AGC HorizontalPodAutoscalers can scale."
	fi
}

# --- main --------------------------------------------------------------------

main() {
	KUBECTL="${KUBECTL:-kubectl}"
	REPO_ROOT="$(git rev-parse --show-toplevel)"
	# shellcheck source=scripts/lib/common.sh
	source "$REPO_ROOT/scripts/lib/common.sh"

	require_cmd "$KUBECTL" "https://kubernetes.io/docs/tasks/tools/"
	require_cmd jq "https://jqlang.github.io/jq/download/"

	echo "==> Validating cluster preflight for the actions-gateway install"

	# A preflight that cannot reach the cluster has not validated anything.
	if ! "$KUBECTL" cluster-info >/dev/null 2>&1; then
		echo "ERROR: cannot reach a cluster with '$KUBECTL' — point KUBECONFIG/--context at the target cluster and re-run." >&2
		exit 1
	fi

	check_cni
	check_k8s_version
	check_cert_manager
	check_metrics_server

	echo
	local strict=0
	case "${VALIDATE_STRICT:-}" in 1 | true | TRUE | yes) strict=1 ;; esac

	if ((n_fail > 0)); then
		echo "RESULT: FAIL — ${n_fail} blocking issue(s), ${n_warn} warning(s). Do NOT install until the failures above are resolved." >&2
		exit 1
	fi
	if ((n_warn > 0)) && ((strict == 1)); then
		echo "RESULT: FAIL (strict) — ${n_warn} warning(s) treated as failures (VALIDATE_STRICT)." >&2
		exit 1
	fi
	if ((n_warn > 0)); then
		echo "RESULT: PASS with ${n_warn} warning(s) — review the warnings above; install can proceed."
	else
		echo "RESULT: PASS — the cluster meets all install prerequisites."
	fi
}

# Run main only when executed directly, so validate-cluster-test.sh can source
# this file to exercise the pure helpers without running the live checks.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
