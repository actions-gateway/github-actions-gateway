#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/setup.sh: the one-time bootstrap that creates
# the dogfood cluster and its node pools, installs the v2 CRDs and the GAG
# chart, and provisions the gag-dogfood tenant.
#
# Why it is tested: the script is idempotent by design, which is what makes its
# failures durable. Every create is guarded by an exists check, so a flag left
# off the first run is skipped by every run after it and can only be repaired by
# deleting the resource. Q380 is the recorded instance — --workload-pool went
# missing from the cluster create while the runbook documented it, and nobody
# saw it because every subsequent run took the already-exists branch. The pool
# flags below are the same shape: --disk-type=pd-standard is what keeps worker
# capacity off the 500 GB regional SSD quota that capped the pool at ~4 nodes
# (Q248).
#
# The other class is ordering. GAG_IMAGE_TAG is used twice — as the image tag
# and as the git ref the CRD chart is archived from — and the whole point is
# that one ref keeps the schema and the code together; a caBundle patched
# before the chart has minted its CA writes an empty one and every CR apply then
# fails the conversion webhook's TLS handshake; and the CRDs must reach the
# cluster before the GMC starts or it never enables its v2 controllers.
#
# And the App private key: it comes off the keychain through a pipe, so a miss
# yields an empty file rather than an error. A Secret built from that fails much
# later as an opaque GAG auth error, so the read must abort instead — and the key
# must never reach argv.
#
# The script is sourced with SETUP_LIB_ONLY=1 so main() does not run; `gcloud`,
# `kubectl`, `helm`, `git`, `tar`, `security` and `xxd` are stubbed, so no
# cluster, no keychain and no billable create, and REPO_ROOT is repointed at a
# fixture tree so preflight runs a stub rather than the real validate-cluster.sh.
# confirm_or_exit and gke_get_credentials_and_verify run for real against those
# stubs — they are the two gates standing between a fat-fingered run and someone
# else's cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
SETUP_LIB_ONLY=1
export SETUP_LIB_ONLY
# shellcheck source=scripts/dogfood/setup.sh
source "${REPO_ROOT}/scripts/dogfood/setup.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0

PROJECT=dogfood-proj
CLUSTER=gag-dogfood
ZONE=us-east1-b
REPO=octo/repo
APP_ID=3752347
INSTALLATION_ID=99887766
GAG_IMAGE_TAG=abc1234

# --- Stubs -----------------------------------------------------------------
#
# Every external command logs its argv to one shared file, so a test can assert
# both what was called and in what order — ordering carries most of the
# correctness argument here. The log is a file because main() runs in a
# subshell.
CALL_LOG="${WORKDIR}/calls.log"
export CALL_LOG
HELM_VALUES="${WORKDIR}/helm-values.yaml"
MANIFESTS="${WORKDIR}/manifests.yaml"

CLUSTER_EXISTS=1
POOLS_EXIST=1
CONTEXT=""
KEY_HEX="deadbeef"
CRD_STRATEGY="Webhook"
CA_BUNDLE="Y2EtY2VydA=="
EXISTING_RUNNER_IMAGE=""
GMC_IS_READY=0
PREFLIGHT_RC=0
export PREFLIGHT_RC

# preflight execs a path under REPO_ROOT, so it cannot be stubbed by a shell
# function. Repoint REPO_ROOT at a fixture tree holding a stub instead — every
# other use of it (the chart path, the git-archive dir, the Athens overlay) is
# an argument to a stubbed command, so the fake root is only ever logged.
FAKE_ROOT="${WORKDIR}/root"
mkdir -p "${FAKE_ROOT}/scripts/e2e"
cat >"${FAKE_ROOT}/scripts/e2e/validate-cluster.sh" <<'STUB'
#!/usr/bin/env bash
printf 'validate-cluster.sh\n' >>"${CALL_LOG}"
exit "${PREFLIGHT_RC:-0}"
STUB
chmod +x "${FAKE_ROOT}/scripts/e2e/validate-cluster.sh"
REPO_ROOT="${FAKE_ROOT}"

gcloud() {
	printf 'gcloud %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	container\ clusters\ describe*) return "${CLUSTER_EXISTS}" ;;
	container\ node-pools\ describe*) return "${POOLS_EXIST}" ;;
	esac
	return 0
}

kubectl() {
	printf 'kubectl %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	config\ current-context) echo "${CONTEXT}" ;;
	get\ crd*) echo "${CRD_STRATEGY}" ;;
	get\ secret\ webhook-server-cert*) echo "${CA_BUNDLE}" ;;
	*get\ runnertemplate*) echo "${EXISTING_RUNNER_IMAGE}" ;;
	# The applies read a manifest on stdin; drain it so the writer never sees a
	# closed pipe, and keep it so the rendered objects can be asserted on. Every
	# apply looks alike in argv, so log a marker for the tenant CR — the ordering
	# claims below are about it rather than about the CRD chart's own apply.
	*-f\ -*)
		cat >>"${MANIFESTS}.in"
		grep -q '^kind: ActionsGateway$' "${MANIFESTS}.in" \
			&& printf 'apply-tenant-cr\n' >>"${CALL_LOG}"
		cat "${MANIFESTS}.in" >>"${MANIFESTS}"
		: >"${MANIFESTS}.in"
		;;
	esac
	return 0
}

# helm keeps the rendered values file: it is where the four image tags and the
# dogfood-specific chart overrides land, and none of them show up in argv.
helm() {
	printf 'helm %s\n' "$*" >>"${CALL_LOG}"
	local prev="" arg
	for arg in "$@"; do
		if [[ "${prev}" == "--values" ]]; then
			cat "${arg}" >>"${HELM_VALUES}"
		fi
		prev="${arg}"
	done
	return 0
}

# install_crds git-archives the CRD chart at GAG_IMAGE_TAG and untars it. Both
# sides are stubbed; the ref it archived is the assertion.
git() { printf 'git %s\n' "$*" >>"${CALL_LOG}"; }
tar() { cat >/dev/null; }

# The keychain read and its hex decode. `security` is macOS-only and `xxd` is
# not on every runner, so both are stubbed rather than required — this suite
# asserts what is done with the key, never that the host can produce one.
security() { printf '%s' "${KEY_HEX}"; }
xxd() { cat; }

require_cmd() { :; }
# GMC readiness is gmc.sh's, and gmc-test.sh covers it. Here it is a knob: the
# not-ready branch is what decides whether the rollout gets restarted.
gmc_ready() { return "${GMC_IS_READY}"; }
wait_for_gmc() { printf 'wait_for_gmc %s\n' "$*" >>"${CALL_LOG}"; }

# reset_stubs — restore the default knobs: a first run against a project with no
# cluster, a keychain holding a key, post-Q74 Webhook CRDs with a mintable CA,
# and no RunnerTemplate to read an image back from.
reset_stubs() {
	: >"${CALL_LOG}"
	: >"${HELM_VALUES}"
	: >"${MANIFESTS}"
	: >"${MANIFESTS}.in"
	CLUSTER_EXISTS=1
	POOLS_EXIST=1
	CONTEXT="gke_${PROJECT}_${ZONE}_${CLUSTER}"
	KEY_HEX="deadbeef"
	CRD_STRATEGY="Webhook"
	CA_BUNDLE="Y2EtY2VydA=="
	EXISTING_RUNNER_IMAGE=""
	GMC_IS_READY=0
	PREFLIGHT_RC=0
	DOGFOOD_RUNNER_IMAGE=""
	ATHENS_PERSISTENT=0
	ASSUME_YES=1
}

# run_main — run main() in a subshell and record its status in MAIN_RC and its
# combined output in MAIN_OUT. stdin is closed so the confirmation prompt can
# never block, and reads as a refusal when ASSUME_YES is not set.
#
# The subshell must not be an operand of `||` or `if`: bash suppresses errexit
# inside such a subshell even when it re-runs `set -e` itself, which would let
# main() sail past a failed gate and make the abort assertions below pass
# vacuously. Hence set +e around a plain call.
MAIN_RC=0
MAIN_OUT=""
run_main() {
	set +e
	(
		set -e
		main
	) >"${WORKDIR}/main.out" 2>&1 </dev/null
	MAIN_RC=$?
	set -e
	MAIN_OUT="$(cat "${WORKDIR}/main.out")"
}

# --- Assertions -------------------------------------------------------------

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

check_not_contains() {
	local name="$1" needle="$2" haystack="$3"
	if [[ "${haystack}" != *"${needle}"* ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: '${needle}' unexpectedly present" >&2
		fails=$((fails + 1))
	fi
}

# call_index NEEDLE — 1-based position of the first call log line containing
# NEEDLE, or 0 when absent.
call_index() {
	local needle="$1" i=0 line
	while IFS= read -r line; do
		i=$((i + 1))
		if [[ "${line}" == *"${needle}"* ]]; then
			echo "${i}"
			return
		fi
	done <"${CALL_LOG}"
	echo 0
}

check_before() {
	local name="$1" first="$2" second="$3" a b
	a="$(call_index "${first}")"
	b="$(call_index "${second}")"
	if ((a > 0 && b > 0 && a < b)); then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: '${first}' at ${a}, '${second}' at ${b}" >&2
		fails=$((fails + 1))
	fi
}

# call_line NEEDLE — the first call log line containing NEEDLE, empty if absent.
call_line() { grep -m1 -F -- "$1" "${CALL_LOG}" || true; }

# calls_matching NEEDLE — every call log line containing NEEDLE.
calls_matching() { grep -F -- "$1" "${CALL_LOG}" || true; }

echo "scripts/dogfood/setup-test.sh"

# --- the happy path: a first run against an empty project --------------------

reset_stubs
run_main
check "a first run bootstraps cleanly" 0 "${MAIN_RC}"

# --- the cluster create, whose flags a re-run can never repair ---------------

cluster_create="$(call_line 'clusters create')"
# Workload Identity is control-plane-wide and a HARD prerequisite of the Part F
# e2e pool, whose --workload-metadata=GKE_METADATA is rejected with a 400
# without it. Its absence is invisible until e2e-setup.sh runs, months later,
# and is then repairable only by a separate control-plane update (Q380).
check_contains "enables Workload Identity on the cluster" \
	"--workload-pool=${PROJECT}.svc.id.goog" "${cluster_create}"
# GAG's whole tenant-isolation story is NetworkPolicy, which the default GKE
# dataplane does not enforce.
check_contains "creates a cluster whose CNI enforces NetworkPolicy" \
	"--enable-dataplane-v2" "${cluster_create}"

# --- the worker pools --------------------------------------------------------

worker_create="$(call_line 'node-pools create workers ')"
# Spot is the right default for routine CI: cheap, and a preemption just re-runs.
check_contains "makes the routine worker pool spot" "--spot" "${worker_create}"
# pd-balanced boot disks count against the 500 GB regional SSD quota, which
# capped this pool at ~4 nodes — a self-inflicted ceiling, not a real shortage
# (Q248). pd-standard counts against the 4096 GB DISKS_TOTAL_GB instead.
check_contains "keeps worker disks off the regional SSD quota" \
	"--disk-type=pd-standard" "${worker_create}"
check_contains "autoscales the worker pool from zero" "--min-nodes=0" "${worker_create}"
check_contains "lifts the worker ceiling to what the quota allows" \
	"--max-nodes=8" "${worker_create}"
# Nothing but worker pods belongs on a spot node: a preempted GMC or AGC is an
# outage, not a re-run.
check_contains "taints the worker pool for worker pods only" \
	"--node-taints=dedicated=workers:NoSchedule" "${worker_create}"

od_create="$(call_line 'node-pools create workers-od')"
# A benchmark wants a fixed, known node count. Q260 chased a job-starvation
# signal that turned out to be spot preemption dropping nodes 3->1 mid-burst;
# the run only became readable once it was pinned to a non-preemptible pool.
check_not_contains "keeps the benchmark pool off spot" "--spot" "${od_create}"
check_not_contains "never autoscales the benchmark pool" \
	"--enable-autoscaling" "${od_create}"
# Same taint as `workers`, so the identical worker-pod toleration schedules onto
# either pool.
check_contains "taints the benchmark pool the same way" \
	"--node-taints=dedicated=workers:NoSchedule" "${od_create}"

# --- idempotence: a re-run must not disturb what is live ---------------------

reset_stubs
CLUSTER_EXISTS=0
POOLS_EXIST=0
run_main
check "a re-run against a live cluster succeeds" 0 "${MAIN_RC}"
check_not_contains "never re-creates an existing cluster" \
	"clusters create" "$(cat "${CALL_LOG}")"
check_not_contains "never re-creates an existing node pool" \
	"node-pools create" "$(cat "${CALL_LOG}")"
# The installs are upserts, so a re-run must still reach them — that is what
# makes a partial failure safe to re-run.
check_contains "still upgrades the GAG chart on a re-run" \
	"upgrade --install gag" "$(call_line 'upgrade --install gag')"
check_contains "still applies the tenant CRs on a re-run" \
	"kind: ActionsGateway" "$(cat "${MANIFESTS}")"

# --- one ref for the image and the CRD schema --------------------------------

reset_stubs
GAG_IMAGE_TAG="feedface"
run_main
# The v2 alpha schema drifts between refs, and a CRD/image mismatch makes every
# reconcile fail validation — a stale githubAppRef CRD silently drops the
# credential and the AGC crash-loops on a missing App key. git-archive is what
# pins the chart to the image's ref rather than the local worktree.
check_contains "archives the CRD chart at the image ref" \
	"archive feedface charts/actions-gateway-crds-v2" "$(call_line 'archive')"
for component in gmc agc proxy wrapper; do
	check_contains "pins the ${component} image to the same ref" \
		"tag: feedface" "$(grep -A2 "^${component}:" "${HELM_VALUES}")"
done
GAG_IMAGE_TAG=abc1234

# The chart's wrapper tag defaults to empty, which renders
# ghcr.io/.../wrapper:latest — a tag this registry never publishes — so worker
# injection would ImagePullBackOff without an explicit pin.
check_contains "never leaves the wrapper tag to the chart default" \
	"wrapper:" "$(cat "${HELM_VALUES}")"

# --- the chart overrides dogfood cannot run without -------------------------

reset_stubs
run_main
# With a single GMC replica the chart's minAvailable: 1 permits zero voluntary
# disruptions, so the scale-to-0 stop can never drain the system node — it
# lingers Ready,SchedulingDisabled and keeps billing (Q236).
check_contains "disables the PodDisruptionBudget that would block scale-to-0" \
	"enabled: false" "$(grep -A1 '^podDisruptionBudget:' "${HELM_VALUES}")"
check_contains "runs a single GMC replica" "replicaCount: 1" "$(cat "${HELM_VALUES}")"
# Dogfood tracks pre-release code by git ref, which is a floating tag.
check_contains "permits the floating image tags dogfood installs by" \
	"allowFloatingImageTags: true" "$(cat "${HELM_VALUES}")"

# --- install ordering --------------------------------------------------------

check_before "preflights the cluster before installing anything" \
	"validate-cluster.sh" "upgrade --install gag"
# The GMC detects the v2 CRDs at startup and only then enables its v2
# controllers and conversion webhook, so this order must not be reversed.
check_before "installs the v2 CRDs before the GMC that serves them" \
	"archive" "upgrade --install gag"
# The CA is only mintable once the chart has created webhook-server-cert.
check_before "wires the caBundle only after the chart has minted the CA" \
	"upgrade --install gag" "patch crd"
# ...and before the first CR, so it already round-trips through a TLS-verified
# conversion webhook.
check_before "wires the caBundle before applying the first CR" \
	"patch crd" "apply-tenant-cr"
check_before "verifies the cluster context before installing anything" \
	"config current-context" "upgrade --install gag"
check_before "verifies the cluster context before creating the App Secret" \
	"config current-context" "create secret"

# A preflight that fails must stop the install, not warn past it.
reset_stubs
PREFLIGHT_RC=1
run_main
check "a failed preflight fails the bootstrap" 1 "${MAIN_RC}"
check_not_contains "never installs onto a cluster that failed preflight" \
	"upgrade --install gag" "$(cat "${CALL_LOG}")"

# --- the conversion caBundle -------------------------------------------------

reset_stubs
run_main
patch_call="$(call_line 'patch crd')"
check_contains "patches the caBundle read off the chart's Secret" \
	"${CA_BUNDLE}" "${patch_call}"
# A JSON merge patch sets the leaf and leaves the chart-rendered clientConfig
# service block alone; a strategic or replace patch would strip it.
check_contains "sets the caBundle leaf with a merge patch" \
	"--type=merge" "${patch_call}"
check "wires every v2 CRD" 5 "$(calls_matching 'patch crd' | wc -l | tr -d ' ')"

# A pre-Q74 image ships single-version CRDs with no conversion block, where
# patching a webhook clientConfig is rejected outright. Skipping keeps setup
# valid across the image-tag transition.
reset_stubs
CRD_STRATEGY=""
run_main
check "a single-version CRD set still bootstraps" 0 "${MAIN_RC}"
check_not_contains "never patches a CRD that has no conversion webhook" \
	"patch crd" "$(cat "${CALL_LOG}")"
check_contains "says which CRDs had no caBundle to wire" \
	"not Webhook" "${MAIN_OUT}"

# Secure by default: an unreadable CA aborts rather than falling back to a
# caBundle-less clientConfig, which would leave the conversion webhook
# unverified.
reset_stubs
CA_BUNDLE=""
run_main
check "an unreadable webhook CA fails the bootstrap" 1 "${MAIN_RC}"
check_not_contains "never patches an empty caBundle" \
	"patch crd" "$(cat "${CALL_LOG}")"
check_not_contains "never applies a CR through an unverified webhook" \
	"kind: ActionsGateway" "$(cat "${MANIFESTS}")"

# --- the GMC rollout ---------------------------------------------------------

# A prior run can leave the ReplicaSet in pod-creation backoff from before the
# permitting quota existed; restarting clears it instead of waiting the backoff
# out.
reset_stubs
GMC_IS_READY=1
run_main
check_contains "restarts a GMC that is not ready" \
	"rollout restart" "$(call_line 'rollout restart')"
# GKE only permits system-cluster-critical in a namespace with a quota scoped to
# it; without one the GMC ReplicaSet cannot create pods at all.
check_before "permits the system-critical PriorityClass before waiting" \
	"upgrade --install gag" "wait_for_gmc"

reset_stubs
GMC_IS_READY=0
run_main
check_not_contains "never bounces a healthy GMC" \
	"rollout restart" "$(cat "${CALL_LOG}")"

# --- the App key: abort on a miss, and never through argv --------------------

reset_stubs
KEY_HEX=""
run_main
check "an empty keychain read fails the bootstrap" 1 "${MAIN_RC}"
check_not_contains "never creates a Secret from an empty key" \
	"create secret" "$(cat "${CALL_LOG}")"
check_contains "says the keychain read came back empty" \
	"private key from keychain is empty" "${MAIN_OUT}"

reset_stubs
KEY_HEX="0123456789abcdef"
run_main
secret_call="$(call_line 'create secret generic github-app-v1')"
check_contains "passes the private key by file" \
	"--from-file=privateKey=" "${secret_call}"
check_not_contains "never passes the private key through argv" \
	"--from-literal=privateKey" "${secret_call}"
check_not_contains "never puts key material on a command line" \
	"0123456789abcdef" "$(cat "${CALL_LOG}")"
pem_file="$(printf '%s\n' "${secret_call}" | sed -n 's/.*--from-file=privateKey=\([^ ]*\).*/\1/p')"
check "cleans up the key temp file" "" "$(ls "${pem_file}" 2>/dev/null || true)"

# --- the runner image, whose default is a footgun ----------------------------

# 1. Pinned explicitly.
reset_stubs
DOGFOOD_RUNNER_IMAGE="ghcr.io/octo/runner:build"
run_main
check_contains "pins the runner image when one is given" \
	"image: ghcr.io/octo/runner:build" "$(cat "${MANIFESTS}")"

# 2. Unset, with a toolchain-pinned image already live. Resetting that back to
# the image-less upstream default makes `make` and Go vanish from every worker —
# the Q295 footgun that cost a full validation cycle.
reset_stubs
EXISTING_RUNNER_IMAGE="ghcr.io/octo/runner:pinned-by-hand"
run_main
check_contains "preserves a live runner image when none is given" \
	"image: ghcr.io/octo/runner:pinned-by-hand" "$(cat "${MANIFESTS}")"
check_contains "says the live image was preserved" \
	"preserving existing runner image" "${MAIN_OUT}"
# The read-back pins the context rather than trusting the active one: a parallel
# session sharing ~/.kube/config could have repointed it since get_credentials.
check_contains "pins the context on the runner image read-back" \
	"--context gke_${PROJECT}_${ZONE}_${CLUSTER}" "$(call_line 'get runnertemplate')"

# 3. Unset with nothing live: stay image-less so the AGC gap-fills
# DefaultWorkerImage and injects the wrapper (the Q235 injection default).
reset_stubs
run_main
check_not_contains "stays image-less on a first run" \
	"image: ghcr.io" "$(cat "${MANIFESTS}")"

# --- the tenant objects ------------------------------------------------------

reset_stubs
run_main
manifests="$(cat "${MANIFESTS}")"
# Classic acquires a job at GitHub before deciding whether to provision a
# worker, orphaning every job it declines: 85 acquired, 16 worker pods, 69
# orphaned on this tenant (Q399). acquisitionProtocol is immutable, so this is
# set explicitly rather than left to the default.
check_contains "runs the tenant on the ScaleSet protocol" \
	"acquisitionProtocol: ScaleSet" "${manifests}"
# maxWorkers is also the capacity advertised to GitHub on this path, so GitHub
# never assigns more jobs than the tenant can place — and it matches the pool's
# max-nodes.
check_contains "advertises the capacity the worker pool can place" \
	"maxWorkers: 8" "${manifests}"
# The sizing clamps are safety rails, not decoration: this tenant runs the
# project's real CI, so a derivation from noisy early samples must not starve a
# heavy job or ask for a pod no node can hold (an e2-standard-4 worker has ~3.4
# vCPU allocatable, so 3 schedules and 4 would not).
check_contains "floors the derived CPU request under a heavy job" \
	'cpu: "1"' "$(grep -A4 'minRequests:' "${MANIFESTS}")"
check_contains "caps the derived CPU request at what a node can hold" \
	'cpu: "3"' "$(grep -A3 'maxRequests:' "${MANIFESTS}")"
# The namespace markers: tenant=managed authorizes the GMC to operate here, and
# the security profile drives the Pod Security level it stamps.
label_call="$(call_line 'label namespace gag-dogfood')"
check_contains "marks the namespace as a managed tenant" \
	"actions-gateway.com/tenant=managed" "${label_call}"
check_contains "stamps the tenant's security profile" \
	"actions-gateway.com/security-profile=baseline" "${label_call}"
# Workers cannot reach proxy.golang.org (egress NetworkPolicy, and GKE DPv2 has
# no FQDN policy), so Go module fetches have to route through in-cluster Athens.
check_contains "routes worker module fetches through Athens" \
	"go-module-proxy.gag-dogfood.svc.cluster.local:3000" "${manifests}"
check_contains "waits for Athens before the tenant CRs land" \
	"rollout status deployment/athens" "$(call_line 'deployment/athens')"

# The persistent overlay keeps the module cache warm across scale-to-zero at the
# cost of a continuously-billed disk, so it is opt-in.
reset_stubs
ATHENS_PERSISTENT=1
run_main
check_contains "renders the persistent Athens overlay on request" \
	"deploy/athens/overlays/persistent" "$(call_line 'apply -k')"

# --- nothing is written to a cluster that is not the target ------------------

reset_stubs
CONTEXT="gke_other-proj_us-west1-a_prod"
run_main
check "a context that is not the target fails the bootstrap" 1 "${MAIN_RC}"
check_not_contains "never installs GAG onto the wrong cluster" \
	"upgrade --install gag" "$(cat "${CALL_LOG}")"
check_not_contains "never creates the App Secret on the wrong cluster" \
	"create secret" "$(cat "${CALL_LOG}")"
check_not_contains "never applies tenant CRs to the wrong cluster" \
	"kind: ActionsGateway" "$(cat "${MANIFESTS}")"

# --- the confirmation gate ---------------------------------------------------

reset_stubs
run_main
check_contains "names the project being spent against" "${PROJECT}" "${MAIN_OUT}"
check_contains "names the cluster being written to" "${CLUSTER}" "${MAIN_OUT}"
check_contains "names the repo the tenant will serve" "${REPO}" "${MAIN_OUT}"

reset_stubs
unset ASSUME_YES
run_main
check "a declined confirmation fails the bootstrap" 1 "${MAIN_RC}"
check_not_contains "creates nothing when the confirmation is declined" \
	"clusters create" "$(cat "${CALL_LOG}")"
check_not_contains "installs nothing when the confirmation is declined" \
	"upgrade --install" "$(cat "${CALL_LOG}")"

# --- missing configuration aborts before anything runs -----------------------

for missing in PROJECT CLUSTER ZONE REPO APP_ID INSTALLATION_ID; do
	reset_stubs
	# A nameref rather than eval: shellcheck can see the write through it, and
	# the value never round-trips through a string the shell re-parses.
	declare -n slot="${missing}"
	saved="${slot}"
	slot=""
	run_main
	check "an unset ${missing} fails the bootstrap" 1 "${MAIN_RC}"
	check_not_contains "creates nothing with ${missing} unset" \
		"clusters create" "$(cat "${CALL_LOG}")"
	slot="${saved}"
	unset -n slot
done

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all setup.sh tests passed"
