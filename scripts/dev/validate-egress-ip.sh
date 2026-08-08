#!/usr/bin/env bash
# validate-egress-ip.sh — live GKE validation of per-tenant egress-IP pinning
# (Q243, the residual left after Q282).
#
# WHAT THIS PROVES
#   The 2026-07-07 campaign (the q243-q245-q230 live-validation-campaign plan,
#   indexed in docs/plan/README.md)
#   proved the Cloud NAT *mechanism* — two tenants on two pod secondary ranges
#   egress from distinct, stable IPs — but found that a real EgressProxy could not
#   be pinned to one tenant pool: its built-in cross-node anti-affinity *spread* a
#   2-replica pool across both pools, so one tenant egressed from TWO IPs. Q282
#   added EgressProxy.spec.scheduling (nodeSelector/tolerations/affinity) and made a
#   supplied podAntiAffinity REPLACE the built-in spread — but that fix was only
#   asserted in envtest, never live. This script closes that gap end-to-end:
#
#     For each tenant, a GMC-provisioned EgressProxy pinned via spec.scheduling
#     lands ALL its replicas on that tenant's node pool (pod IPs in that tenant's
#     secondary range), so the whole pool egresses from exactly ONE Cloud NAT IP —
#     distinct per tenant and stable across a pod reschedule.
#
#   PASS = each tenant's proxy pool egresses from a single reserved IP, the two
#   tenants' IPs are disjoint, and the IP is unchanged after a pod delete/recreate.
#   FAIL = a proxy pool spans pools / ranges (>1 egress IP), or the IPs collide, or
#   an IP moves on reschedule — a real regression of the Q243 claim; record it
#   honestly (secure-by-default), do not paper over it.
#
# SAFETY / COST
#   This creates BILLABLE cloud resources (a GKE Standard cluster, 3 node pools,
#   3 Cloud NAT gateways, 3 reserved static IPs). Order-of-magnitude a few USD if
#   torn down same-session; ~$10-14/day if left running. It refuses to run against
#   the dogfood project and gates every billable step behind a confirmation.
#
#   Like scripts/dogfood/setup.sh, this does NOT create the GCP project or link
#   billing (account-scoped, awkward to script idempotently). Point it at an
#   EXISTING throwaway project with billing linked; the cleanest teardown is to
#   delete that whole project afterward (atomic — no orphaned NAT/IPs keep billing).
#
# USAGE
#   PROJECT=<throwaway-project> GAG_IMAGE_TAG=<ref> scripts/dev/validate-egress-ip.sh
#   PROJECT=<throwaway-project> scripts/dev/validate-egress-ip.sh --teardown-only
#
# REQUIRED ENV
#   PROJECT          Throwaway GCP project ID (must NOT be the dogfood project).
#   GAG_IMAGE_TAG    Git ref whose pushed ghcr.io/actions-gateway/{gmc,proxy}:<ref>
#                    images carry Q282 (spec.scheduling). Build+push with BUILD_IMAGE=1,
#                    or pre-push and pass the tag. Also names the CRD chart git ref
#                    (kept identical so schema and image never drift — as in dogfood).
#
# OPTIONAL ENV
#   ZONE             GCP zone (default us-east1-b — dogfood's known-good quota).
#   CLUSTER          GKE cluster name (default gag-egress-val).
#   IP_REFLECTOR     External source-IP echo URL (default https://api.ipify.org).
#   BUILD_IMAGE=1    Build the GMC/proxy images from GAG_IMAGE_TAG and push to GHCR
#                    (a registry publish — off by default; assume pre-pushed).
#   KEEP=1           Skip teardown at the end (inspect the cluster; tear down later
#                    with --teardown-only).
#   ASSUME_YES=1     Skip the interactive confirmation (automation).
#
# FLAGS
#   --teardown-only  Delete the resources this script creates in PROJECT, then exit.
#
# See the q243 egress-IP reference-arch plan (indexed in docs/plan/README.md)
# § "Re-runnable live validation".
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

# --- Configuration ----------------------------------------------------------

PROJECT="${PROJECT:-}"
ZONE="${ZONE:-us-east1-b}"
REGION="${ZONE%-*}"
CLUSTER="${CLUSTER:-gag-egress-val}"
GAG_IMAGE_TAG="${GAG_IMAGE_TAG:-}"
IP_REFLECTOR="${IP_REFLECTOR:-https://api.ipify.org}"
NETWORK="${CLUSTER}-net"
SUBNET="${CLUSTER}-subnet"
ROUTER="${CLUSTER}-router"

# The dogfood project is prod-classified; never build/destroy validation infra in it.
DOGFOOD_PROJECT="actions-gateway-dogfood"

# Two tenants is sufficient — per-tenant isolation is a pairwise property. Each row:
#   name  node-pool   pod-range   pod-CIDR       reserved-IP-name   NAT-name
TENANTS=(
	"a pool-a pods-a 10.5.0.0/16 egress-ip-a nat-a"
	"b pool-b pods-b 10.6.0.0/16 egress-ip-b nat-b"
)
# The system pod range + its NAT (GMC/kube-system pods egress here, off the
# per-tenant paths so they never contend for a tenant IP).
SYS_RANGE="pods-default"
SYS_CIDR="10.4.0.0/16"
SYS_NAT="nat-default"
SVC_RANGE="svc"
SVC_CIDR="10.8.0.0/20"
NODE_MACHINE="e2-standard-2"

# gc PROJECT-pinned gcloud. Every call pins --project so a repointed shared
# ~/.config/gcloud (a parallel session's `config set`) can never land it elsewhere.
gc() { gcloud --project="${PROJECT}" --quiet "$@"; }

# --- Preflight --------------------------------------------------------------

preflight() {
	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd helm "https://helm.sh/docs/intro/install/"
	require_cmd gke-gcloud-auth-plugin "https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl"

	[[ -n "${PROJECT}" ]] || { echo "PROJECT is required (a throwaway project)." >&2; exit 1; }
	if [[ "${PROJECT}" == "${DOGFOOD_PROJECT}" ]]; then
		echo "Refusing to run against the dogfood project '${DOGFOOD_PROJECT}'." >&2
		echo "This creates and destroys infra — use a throwaway project." >&2
		exit 1
	fi
	# Confirm the project exists and we can see it before creating anything in it.
	gc projects describe "${PROJECT}" --format='value(projectId)' >/dev/null 2>&1 || {
		echo "Cannot describe project '${PROJECT}' — create it and link billing first." >&2
		exit 1
	}
}

# --- Networking -------------------------------------------------------------

create_network() {
	echo "== Creating VPC ${NETWORK} + subnet ${SUBNET} (3 pod ranges) =="
	gc compute networks describe "${NETWORK}" >/dev/null 2>&1 ||
		gc compute networks create "${NETWORK}" --subnet-mode=custom

	if ! gc compute networks subnets describe "${SUBNET}" --region="${REGION}" >/dev/null 2>&1; then
		gc compute networks subnets create "${SUBNET}" \
			--network="${NETWORK}" --region="${REGION}" --range=10.0.0.0/22 \
			--secondary-range="${SYS_RANGE}=${SYS_CIDR},${SVC_RANGE}=${SVC_CIDR}" \
			--enable-private-ip-google-access
		# Add the per-tenant pod ranges (subnets create takes one --secondary-range
		# flag cleanly; append the rest via update so each tenant is explicit).
		local tenant name pod_range pod_cidr rest
		for tenant in "${TENANTS[@]}"; do
			read -r name pool pod_range pod_cidr rest <<<"${tenant}"
			gc compute networks subnets update "${SUBNET}" --region="${REGION}" \
				--add-secondary-ranges="${pod_range}=${pod_cidr}"
		done
	fi
}

reserve_ips() {
	echo "== Reserving per-tenant static egress IPs =="
	local tenant name pool pod_range pod_cidr ip_name rest
	for tenant in "${TENANTS[@]}"; do
		read -r name pool pod_range pod_cidr ip_name rest <<<"${tenant}"
		gc compute addresses describe "${ip_name}" --region="${REGION}" >/dev/null 2>&1 ||
			gc compute addresses create "${ip_name}" --region="${REGION}"
	done
	gc compute addresses describe "${SYS_NAT}-ip" --region="${REGION}" >/dev/null 2>&1 ||
		gc compute addresses create "${SYS_NAT}-ip" --region="${REGION}"
}

# reserved_ip NAME — print the reserved static address for NAME.
reserved_ip() {
	gc compute addresses describe "$1" --region="${REGION}" --format='value(address)'
}

# --- Cluster ----------------------------------------------------------------

create_cluster() {
	echo "== Creating GKE Standard/DPv2 cluster ${CLUSTER} =="
	if ! gc container clusters describe "${CLUSTER}" --zone="${ZONE}" >/dev/null 2>&1; then
		# --disable-default-snat makes pod IPs (from the per-tenant ranges), not the
		# node IP, the Cloud NAT source — the load-bearing flag for per-range → per-IP
		# mapping. GKE only permits it together with --enable-private-nodes, so the
		# nodes are private and the control plane is reached over its PUBLIC endpoint
		# from THIS operator host (the CRD + GMC install below runs kubectl locally).
		# A private-nodes control plane drops off-cluster kubectl unless the caller's
		# IP is an authorized network — an earlier revision without this timed out on
		# the CRD install — so detect this host's public egress IP and authorize it.
		local op_ip
		op_ip="$(curl -fsS --max-time 20 "${IP_REFLECTOR}" || true)"
		[[ "${op_ip}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
			{ echo "Could not determine this host's public IP from ${IP_REFLECTOR} (got '${op_ip}')." >&2; exit 1; }
		echo "== Creating cluster; authorizing operator IP ${op_ip}/32 on the control plane =="
		gc container clusters create "${CLUSTER}" --zone="${ZONE}" \
			--enable-dataplane-v2 --enable-ip-alias --disable-default-snat \
			--enable-private-nodes --master-ipv4-cidr=172.16.0.0/28 \
			--enable-master-authorized-networks \
			--master-authorized-networks="${op_ip}/32" \
			--network="${NETWORK}" --subnetwork="${SUBNET}" \
			--cluster-secondary-range-name="${SYS_RANGE}" \
			--services-secondary-range-name="${SVC_RANGE}" \
			--machine-type="${NODE_MACHINE}" --num-nodes=1
	fi

	local tenant name pool pod_range pod_cidr rest
	for tenant in "${TENANTS[@]}"; do
		read -r name pool pod_range pod_cidr rest <<<"${tenant}"
		gc container node-pools describe "${pool}" --cluster="${CLUSTER}" --zone="${ZONE}" >/dev/null 2>&1 && continue
		echo "== Creating tenant node pool ${pool} (range ${pod_range}) =="
		# Taint + label so only this tenant's pods (via tolerations + nodeSelector)
		# land here. --pod-ipv4-range binds the pool's pods to its own range.
		gc container node-pools create "${pool}" --cluster="${CLUSTER}" --zone="${ZONE}" \
			--machine-type="${NODE_MACHINE}" --num-nodes=1 \
			--pod-ipv4-range="${pod_range}" \
			--node-labels="tenant=${name}" \
			--node-taints="tenant=${name}:NoSchedule"
	done

	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"
}

create_nat() {
	echo "== Creating Cloud Router + per-range NAT gateways =="
	gc compute routers describe "${ROUTER}" --region="${REGION}" >/dev/null 2>&1 ||
		gc compute routers create "${ROUTER}" --network="${NETWORK}" --region="${REGION}"

	# System NAT: primary range + the system pod/service ranges → the system IP.
	gc compute routers nats describe "${SYS_NAT}" --router="${ROUTER}" --region="${REGION}" >/dev/null 2>&1 ||
		gc compute routers nats create "${SYS_NAT}" --router="${ROUTER}" --region="${REGION}" \
			--nat-custom-subnet-ip-ranges="${SUBNET},${SUBNET}:${SYS_RANGE}" \
			--nat-external-ip-pool="${SYS_NAT}-ip"

	local tenant name pool pod_range pod_cidr ip_name nat_name
	for tenant in "${TENANTS[@]}"; do
		read -r name pool pod_range pod_cidr ip_name nat_name <<<"${tenant}"
		gc compute routers nats describe "${nat_name}" --router="${ROUTER}" --region="${REGION}" >/dev/null 2>&1 && continue
		echo "== NAT ${nat_name} scoped to ${pod_range} → ${ip_name} =="
		# Scope this NAT to ONLY the tenant's pod range, sourced from its reserved IP —
		# that disjoint scoping is what gives the tenant a deterministic, private egress IP.
		gc compute routers nats create "${nat_name}" --router="${ROUTER}" --region="${REGION}" \
			--nat-custom-subnet-ip-ranges="${SUBNET}:${pod_range}" \
			--nat-external-ip-pool="${ip_name}"
	done
}

# --- GAG install ------------------------------------------------------------

build_push_image() {
	[[ "${BUILD_IMAGE:-}" == "1" ]] || return 0
	require_cmd docker "https://docs.docker.com/get-docker/"
	require_cmd gh "https://cli.github.com/"
	echo "== Building + pushing GMC/proxy images at ${GAG_IMAGE_TAG} to GHCR =="
	confirm_or_exit "This PUBLISHES images to ghcr.io/actions-gateway (a shared registry)."
	gh auth refresh -s write:packages
	GIT_SHA="${GAG_IMAGE_TAG}" docker buildx bake gmc proxy \
		--set '*.platform=linux/amd64' \
		--set "gmc.tags=ghcr.io/actions-gateway/gmc:${GAG_IMAGE_TAG}" \
		--set "proxy.tags=ghcr.io/actions-gateway/proxy:${GAG_IMAGE_TAG}" \
		--push
}

install_crds() {
	echo "== Installing v2 CRDs at ${GAG_IMAGE_TAG} =="
	local crd_src
	crd_src="$(mktemp -d)"
	trap 'rm -rf "${crd_src:-}"' RETURN
	git -C "${REPO_ROOT}" archive "${GAG_IMAGE_TAG}" charts/actions-gateway-crds-v2 |
		tar -x -C "${crd_src}"
	# Render + server-side apply (the v2 CRD chart exceeds Helm's 1 MiB release-Secret
	# limit — Q276/Q277 — so `helm install` can't store it). Identical to dogfood.
	helm template actions-gateway-crds-v2 "${crd_src}/charts/actions-gateway-crds-v2" \
		--namespace gmc-system |
		kubectl apply --server-side --force-conflicts -f -
}

install_gmc() {
	echo "== Installing GMC control plane =="
	local values
	values="$(mktemp)"
	trap 'rm -f "${values:-}"' RETURN
	# Only the GMC is needed — it provisions the EgressProxy Deployments we probe.
	# Pin GMC on default-pool (untainted) so it never competes for a tenant pool.
	cat >"${values}" <<EOF
allowFloatingImageTags: true
replicaCount: 1
gmc:
  image:
    tag: ${GAG_IMAGE_TAG}
proxy:
  image:
    tag: ${GAG_IMAGE_TAG}
certManager:
  enabled: false
nodeSelector:
  cloud.google.com/gke-nodepool: default-pool
podDisruptionBudget:
  enabled: false
EOF
	helm upgrade --install gag "${REPO_ROOT}/charts/actions-gateway" \
		--namespace gmc-system --create-namespace --values "${values}"
	# system-cluster-critical (chart default) needs a scope-matching ResourceQuota.
	kubectl apply -f - <<'QUOTA'
apiVersion: v1
kind: ResourceQuota
metadata:
  name: gmc-system-critical-pods
  namespace: gmc-system
spec:
  hard:
    pods: "10"
  scopeSelector:
    matchExpressions:
      - operator: In
        scopeName: PriorityClass
        values: ["system-cluster-critical"]
QUOTA
	# The chart names the manager Deployment "<namePrefix>-controller-manager"
	# (namePrefix defaults to "gmc"), NOT "<release>-gmc" — independent of the
	# "gag" release name. Waiting on the wrong name fails NotFound.
	kubectl -n gmc-system rollout status deploy/gmc-controller-manager --timeout=180s
}

# patch_crd_cabundle — wire the GMC's self-signed webhook CA into each v2 CRD's
# spec.conversion.webhook.clientConfig.caBundle. Since Q74 the v2 kinds are stored
# at v2beta1 and served at v2alpha1 through the GMC-hosted conversion webhook; the
# apiserver calls that webhook over TLS on every CR apply and must trust its serving
# cert via each CRD's caBundle. With certManager.enabled=false the CRD chart renders
# an EMPTY caBundle, so without this the first EgressProxy apply fails the handshake
# ("x509: certificate signed by unknown authority"). Mirrors scripts/dogfood/setup.sh
# Part B3b (Q279). Secure by default: this RESTORES webhook TLS verification — never
# fall back to a caBundle-less clientConfig or insecureSkipTLSVerify.
patch_crd_cabundle() {
	echo "== Wiring GMC webhook CA into the v2 CRD conversion caBundle =="
	# The chart mints a self-signed CA once and stores it in webhook-server-cert;
	# data["ca.crt"] is already base64(PEM) — the encoding a CRD caBundle wants — so
	# pass it straight through. Read lazily so a pre-Q74 (strategy None) image is a no-op.
	local ca_bundle="" crd strategy patched=0
	for crd in actionsgateways runnersets runnertemplates egressproxies clusterrunnertemplates; do
		strategy="$(kubectl get crd "${crd}.actions-gateway.com" \
			-o jsonpath='{.spec.conversion.strategy}' 2>/dev/null || true)"
		if [[ "${strategy}" != "Webhook" ]]; then
			echo "  ${crd}: conversion strategy '${strategy:-None}' (not Webhook) — skipping."
			continue
		fi
		if [[ -z "${ca_bundle}" ]]; then
			ca_bundle="$(kubectl get secret webhook-server-cert -n gmc-system \
				-o jsonpath='{.data.ca\.crt}')"
			[[ -n "${ca_bundle}" ]] ||
				{ echo "webhook-server-cert Secret has no ca.crt — cannot wire the CRD caBundle." >&2; exit 1; }
		fi
		kubectl patch crd "${crd}.actions-gateway.com" --type=merge \
			-p "{\"spec\":{\"conversion\":{\"webhook\":{\"clientConfig\":{\"caBundle\":\"${ca_bundle}\"}}}}}"
		patched=$((patched + 1))
	done
	echo "Wired the GMC webhook CA into ${patched} v2 CRD conversion caBundle(s)."
}

# --- Deploy the pinned proxies ----------------------------------------------

deploy_proxies() {
	echo "== Deploying a pinned EgressProxy per tenant =="
	local tenant name pool pod_range pod_cidr ip_name rest
	for tenant in "${TENANTS[@]}"; do
		read -r name pool pod_range pod_cidr ip_name rest <<<"${tenant}"
		kubectl create namespace "tenant-${name}" --dry-run=client -o yaml | kubectl apply -f -
		# The GMC service account is admission-gated by the ValidatingAdmissionPolicy
		# gmc-tenant-resource-guard: it may only write tenant resources (the proxy TLS
		# Secret, Deployment, …) in namespaces labeled actions-gateway.com/tenant=managed.
		# Without this the reconcile fails "ensure proxy TLS cert: … is forbidden" and no
		# proxy pods are ever created. security-profile + PSA enforce match dogfood so the
		# proxy pods are admitted under Pod Security. (Mirrors scripts/dogfood/setup.sh.)
		kubectl label namespace "tenant-${name}" \
			actions-gateway.com/tenant=managed \
			actions-gateway.com/security-profile=baseline \
			pod-security.kubernetes.io/enforce=baseline \
			--overwrite
		# spec.scheduling pins the pool to the tenant node pool AND opts out of the
		# built-in cross-node spread (podAntiAffinity: {}), so both default replicas
		# (minReplicas=2) land on this one pool → one pod range → one NAT IP.
		# This is exactly the API that did not exist on 2026-07-07.
		kubectl apply -f - <<EOF
apiVersion: actions-gateway.com/v2beta1
kind: EgressProxy
metadata:
  name: tenant-${name}-proxy
  namespace: tenant-${name}
spec:
  scheduling:
    nodeSelector:
      cloud.google.com/gke-nodepool: ${pool}
    tolerations:
      - key: tenant
        operator: Equal
        value: "${name}"
        effect: NoSchedule
    affinity:
      podAntiAffinity: {}
EOF
	done

	local tenant name rest
	for tenant in "${TENANTS[@]}"; do
		read -r name rest <<<"${tenant}"
		# EgressProxy is named "tenant-<name>-proxy"; the reconciler's Deployment is
		# that + "-proxy" → "tenant-<name>-proxy-proxy".
		wait_proxy_ready "tenant-${name}" "tenant-${name}-proxy-proxy"
	done
}

# wait_proxy_ready NS — block until the GMC-provisioned proxy Deployment in NS is
# rolled out. The reconciler creates the Deployment ASYNCHRONOUSLY after the CR
# apply, and `kubectl wait`/`rollout status` fast-fail ("no matching resources
# found") when the object does not exist yet — so poll for the Deployment to appear
# (by the app label) before waiting on its rollout. On a real provisioning failure,
# dump the EgressProxy status + namespace events so the FAIL is diagnosable, not mute.
wait_proxy_ready() {
	local ns="$1" dep="deployment/$2" i
	echo "Waiting for ${dep} to be created by the GMC in ${ns}..."
	# Poll by EXACT name (the EgressProxy reconciler names the Deployment
	# "<ep.Name>-proxy"), not by a label selector: the Deployment's own metadata
	# labels are the app.kubernetes.io/* recommended set, NOT the pods' functional
	# selector — a pod-style selector matches zero Deployments even
	# when a healthy one exists. rollout status fast-fails "not found" before the
	# reconciler creates it, so wait for it to appear first.
	for ((i = 0; i < 60; i++)); do
		kubectl -n "${ns}" get "${dep}" >/dev/null 2>&1 && break
		sleep 3
	done
	if ! kubectl -n "${ns}" get "${dep}" >/dev/null 2>&1; then
		echo "   FAIL: the GMC created no ${dep} in ${ns} within 180s." >&2
		kubectl -n "${ns}" get egressproxy -o yaml >&2 2>/dev/null || true
		kubectl -n "${ns}" get events --sort-by=.lastTimestamp >&2 2>/dev/null || true
		exit 1
	fi
	echo "Waiting for ${dep} (2 replicas) to be Ready..."
	kubectl -n "${ns}" rollout status "${dep}" --timeout=180s
}

# --- Assertions -------------------------------------------------------------

# proxy_pod_nodes NS — print "podIP nodePool" for each proxy pod in NS. Selects by
# the presence of the per-EgressProxy identity label, which every v2 pool's pods
# carry and no v1 pool's do (Q582); the pools this script stands up are v2.
proxy_pod_nodes() {
	local ns="$1"
	kubectl -n "${ns}" get pods -l "actions-gateway.com/egress-proxy" \
		-o jsonpath='{range .items[*]}{.status.podIP}{" "}{.spec.nodeName}{"\n"}{end}'
}

# ip_in_cidr IP CIDR — true when IPv4 IP is inside CIDR (bash-only, no deps).
ip_in_cidr() {
	local ip="$1" cidr="${2%/*}" bits="${2#*/}"
	local -i ipn cidrn mask
	ipn=$(ip_to_int "${ip}"); cidrn=$(ip_to_int "${cidr}")
	mask=$(( 0xFFFFFFFF << (32 - bits) & 0xFFFFFFFF ))
	(( (ipn & mask) == (cidrn & mask) ))
}

ip_to_int() {
	local -a o
	IFS=. read -r -a o <<<"$1"
	echo $(( (o[0] << 24) + (o[1] << 16) + (o[2] << 8) + o[3] ))
}

# observed_egress_ip TENANT — run a throwaway curl pod pinned like the tenant's
# proxy and echo the source IP the reflector sees.
observed_egress_ip() {
	local name="$1" pool="$2"
	local ns="tenant-${name}" pod="egress-probe-${name}" ip
	# Run the probe WITHOUT -i/--rm and read its logs, rather than streaming attached
	# output. `kubectl run -i --rm` interleaves the curl body with kubectl's own
	# "pod ... deleted" notice — and the interactive attach can echo the body twice —
	# which pollutes a captured egress IP (observed "<ip>pod ... deleted"). A
	# non-interactive pod + `kubectl logs` yields exactly the container's stdout (the
	# bare reflector IP); extract the single IPv4 and hand it back clean.
	kubectl -n "${ns}" run "${pod}" --restart=Never --image=curlimages/curl:8.10.1 \
		--overrides="{\"spec\":{\"nodeSelector\":{\"cloud.google.com/gke-nodepool\":\"${pool}\"},\"tolerations\":[{\"key\":\"tenant\",\"operator\":\"Equal\",\"value\":\"${name}\",\"effect\":\"NoSchedule\"}]}}" \
		--command -- curl -s --max-time 20 "${IP_REFLECTOR}" >/dev/null 2>&1 || true
	kubectl -n "${ns}" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${pod}" --timeout=60s >/dev/null 2>&1 || true
	ip="$(kubectl -n "${ns}" logs "${pod}" 2>/dev/null | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)"
	kubectl -n "${ns}" delete pod "${pod}" --wait=false >/dev/null 2>&1 || true
	printf '%s' "${ip}"
}

assert_pinning() {
	echo "== Asserting per-tenant egress-IP pinning =="
	local failed=0 tenant name pool pod_range pod_cidr ip_name rest
	local -a seen_ips=()
	for tenant in "${TENANTS[@]}"; do
		read -r name pool pod_range pod_cidr ip_name rest <<<"${tenant}"
		local want_ip; want_ip="$(reserved_ip "${ip_name}")"
		echo "-- tenant-${name}: pool=${pool} range=${pod_cidr} reserved=${want_ip}"

		# (1) Structural: every proxy pod is on the tenant pool and in its pod range.
		local count=0 pod_ip node
		while read -r pod_ip node; do
			[[ -n "${pod_ip}" ]] || continue
			count=$((count + 1))
			if ! ip_in_cidr "${pod_ip}" "${pod_cidr}"; then
				echo "   FAIL: proxy pod ${pod_ip} on ${node} is OUTSIDE ${pod_cidr} (pool spread!)" >&2
				failed=1
			fi
		done < <(proxy_pod_nodes "tenant-${name}")
		if (( count < 2 )); then
			echo "   FAIL: expected >=2 proxy pods, found ${count}" >&2
			failed=1
		else
			echo "   OK: all ${count} proxy pods in ${pod_cidr}"
		fi

		# (2) Live: a probe pinned like the proxy egresses from the reserved IP.
		local got_ip; got_ip="$(observed_egress_ip "${name}" "${pool}" || true)"
		if [[ "${got_ip}" == "${want_ip}" ]]; then
			echo "   OK: observed egress IP ${got_ip} == reserved ${want_ip}"
		else
			echo "   FAIL: observed egress IP '${got_ip}' != reserved '${want_ip}'" >&2
			failed=1
		fi
		# (3) Distinctness across tenants.
		local prior
		for prior in "${seen_ips[@]}"; do
			if [[ "${prior}" == "${got_ip}" ]]; then
				echo "   FAIL: egress IP ${got_ip} collides with another tenant" >&2
				failed=1
			fi
		done
		seen_ips+=("${got_ip}")
	done

	# (4) Stability across reschedule: delete one tenant-a proxy pod, re-probe.
	echo "-- Stability: delete a tenant-a proxy pod, confirm the egress IP is unchanged"
	local a_name a_pool a_ip_name
	read -r a_name a_pool _ _ a_ip_name _ <<<"${TENANTS[0]}"
	kubectl -n "tenant-${a_name}" delete pod -l "actions-gateway.com/egress-proxy" \
		--field-selector="status.phase=Running" --wait=false | head -1 || true
	# rollout status (via wait_proxy_ready), not a pod-selector wait: after the delete
	# the terminating pod still matches the selector and never reaches Ready, which
	# hangs/errors a `kubectl wait`. The egress IP binds to the pod range + NAT, not to
	# a specific pod, so the probe below re-measures it regardless.
	wait_proxy_ready "tenant-${a_name}" "tenant-${a_name}-proxy-proxy"
	local want_a after_a
	want_a="$(reserved_ip "${a_ip_name}")"
	after_a="$(observed_egress_ip "${a_name}" "${a_pool}" || true)"
	if [[ "${after_a}" == "${want_a}" ]]; then
		echo "   OK: egress IP stable at ${after_a} after reschedule"
	else
		echo "   FAIL: egress IP changed to '${after_a}' after reschedule (want ${want_a})" >&2
		failed=1
	fi

	if (( failed )); then
		echo ""
		echo "RESULT: FAIL — per-tenant egress-IP pinning is NOT proven. Record this honestly." >&2
		return 1
	fi
	echo ""
	echo "RESULT: PASS — each tenant's proxy pool egresses from a single, distinct, stable IP."
}

# --- Teardown ---------------------------------------------------------------

teardown() {
	echo "== Tearing down validation infra in ${PROJECT} =="
	echo "   (For atomic cost hygiene, prefer deleting the whole throwaway project.)"
	gc container clusters delete "${CLUSTER}" --zone="${ZONE}" >/dev/null 2>&1 || true
	local tenant name pool pod_range pod_cidr ip_name nat_name
	for tenant in "${TENANTS[@]}"; do
		read -r name pool pod_range pod_cidr ip_name nat_name <<<"${tenant}"
		gc compute routers nats delete "${nat_name}" --router="${ROUTER}" --region="${REGION}" >/dev/null 2>&1 || true
	done
	gc compute routers nats delete "${SYS_NAT}" --router="${ROUTER}" --region="${REGION}" >/dev/null 2>&1 || true
	gc compute routers delete "${ROUTER}" --region="${REGION}" >/dev/null 2>&1 || true
	for tenant in "${TENANTS[@]}"; do
		read -r name pool pod_range pod_cidr ip_name nat_name <<<"${tenant}"
		gc compute addresses delete "${ip_name}" --region="${REGION}" >/dev/null 2>&1 || true
	done
	gc compute addresses delete "${SYS_NAT}-ip" --region="${REGION}" >/dev/null 2>&1 || true
	gc compute networks subnets delete "${SUBNET}" --region="${REGION}" >/dev/null 2>&1 || true
	gc compute networks delete "${NETWORK}" >/dev/null 2>&1 || true
	echo "== Teardown done. Verify no residual billable resources, or: gcloud projects delete ${PROJECT}"
}

# --- Main -------------------------------------------------------------------

main() {
	if [[ "${1:-}" == "--teardown-only" ]]; then
		preflight
		teardown
		return 0
	fi

	preflight
	[[ -n "${GAG_IMAGE_TAG}" ]] || { echo "GAG_IMAGE_TAG is required." >&2; exit 1; }
	confirm_or_exit "About to create a BILLABLE GKE cluster + 3 Cloud NATs + 3 static IPs in '${PROJECT}' (~a few USD if torn down same-session)."

	# Best-effort teardown on any exit unless KEEP=1 — never leave billing running
	# after a mid-run failure.
	if [[ "${KEEP:-}" != "1" ]]; then
		trap teardown EXIT
	fi

	build_push_image
	create_network
	reserve_ips
	create_cluster
	create_nat
	install_crds
	install_gmc
	patch_crd_cabundle
	deploy_proxies
	assert_pinning
}

main "$@"
