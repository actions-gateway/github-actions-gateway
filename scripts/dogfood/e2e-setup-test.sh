#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/e2e-setup.sh: the one-time bring-up of the e2e
# tenant's cluster infra — the nested-virtualization node pool, the Kata
# runtime, the namespace and the GitHub App Secret.
#
# Why it is tested: every failure here is silent until much later, and two of
# them are security properties rather than breakages.
#
# --workload-metadata=GKE_METADATA on the pool is the load-bearing one. Kata
# isolates the kernel, not the pod network, so a pool created without Workload
# Identity leaves a runner able to mint the node service account's token from
# the metadata server — a cluster that comes up, passes e2e, and is wrong. The
# pool is created once and `node-pools describe` then short-circuits every
# re-run, so the flag can only be fixed by deleting the pool. Q380 is the same
# shape one level up: --workload-pool went missing from the cluster create for
# months because every run took the already-exists branch.
#
# The secret is the other one. The App private key comes off the keychain
# through a pipe, so a keychain miss yields an empty file rather than an error,
# and a Secret created from it fails later as an opaque GAG auth error. It must
# abort instead — and the key must never reach argv, where `ps` and the shell
# history can read it.
#
# The script is sourced with E2E_SETUP_LIB_ONLY=1 so main() does not run;
# `gcloud`, `kubectl`, `helm`, `security` and `xxd` are stubbed, so no cluster,
# no keychain and no billable create. confirm_or_exit and
# gke_get_credentials_and_verify run for real against those stubs — they are the
# two gates standing between a fat-fingered run and someone else's cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
E2E_SETUP_LIB_ONLY=1
export E2E_SETUP_LIB_ONLY
# shellcheck source=scripts/dogfood/e2e-setup.sh
source "${REPO_ROOT}/scripts/dogfood/e2e-setup.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0

PROJECT=dogfood-proj
CLUSTER=gag-dogfood
ZONE=us-east1-b
APP_ID=3752347
INSTALLATION_ID=99887766

# --- Stubs -----------------------------------------------------------------
#
# Every external command logs its argv to one shared file, so a test can assert
# both what was called and in what order — ordering carries the fail-closed
# argument here (nothing may be written to a cluster before the context that
# names it has been verified). The log is a file because main() runs in a
# subshell.
CALL_LOG="${WORKDIR}/calls.log"
POOL_EXISTS=1
CONTEXT=""
# The hex the keychain hands back. Empty models a keychain miss, which is the
# case that must abort rather than create a Secret.
KEY_HEX="deadbeef"

gcloud() {
	printf 'gcloud %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	container\ node-pools\ describe*) return "${POOL_EXISTS}" ;;
	esac
	return 0
}

kubectl() {
	printf 'kubectl %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	config\ current-context) echo "${CONTEXT}" ;;
	# The applies read a manifest on stdin; drain it so the writer never sees a
	# closed pipe, and keep it so the rendered objects can be asserted on.
	*-f\ -*) cat >>"${WORKDIR}/manifests.yaml" ;;
	esac
	return 0
}

helm() { printf 'helm %s\n' "$*" >>"${CALL_LOG}"; }

# The keychain read and its hex decode. `security` is macOS-only and `xxd` is
# not on every runner, so both are stubbed rather than required — this suite
# asserts what is done with the key, never that the host can produce one.
security() { printf '%s' "${KEY_HEX}"; }
xxd() { cat; }

require_cmd() { :; }

# reset_stubs — restore the default knobs: an e2e pool that does not exist yet,
# a keychain holding a key, and the expected context already active.
reset_stubs() {
	: >"${CALL_LOG}"
	: >"${WORKDIR}/manifests.yaml"
	POOL_EXISTS=1
	CONTEXT="gke_${PROJECT}_${ZONE}_${CLUSTER}"
	KEY_HEX="deadbeef"
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

echo "scripts/dogfood/e2e-setup-test.sh"

# --- the happy path ----------------------------------------------------------

reset_stubs
run_main
check "a fresh cluster sets the e2e tenant up cleanly" 0 "${MAIN_RC}"

# --- the node pool the whole isolation story rests on ------------------------

pool_create="$(call_line 'node-pools create e2e')"
# Kata boots a microVM per pod off /dev/kvm, which only a nested-virt node has.
check_contains "creates the pool with nested virtualization" \
	"--enable-nested-virtualization" "${pool_create}"
# Without the explicit image type GKE defaults to COS, onto which kata-deploy
# cannot install at all (Q226).
check_contains "pins an image type kata-deploy can install onto" \
	"--image-type=UBUNTU_CONTAINERD" "${pool_create}"
# The security prerequisite: Kata isolates the kernel, not the pod network, so
# without Workload Identity the runner can still mint the node service account's
# token from the metadata server.
check_contains "binds the pool to Workload Identity" \
	"--workload-metadata=GKE_METADATA" "${pool_create}"
# n2 for quota headroom, not performance: the regional C2_CPUS default is 8, one
# node of this shape, so a refused scale-up has nowhere to retry (Q627).
check_contains "uses the n2 family the regional quota can grow into" \
	"--machine-type=n2-standard-8" "${pool_create}"
# kata-deploy is scoped to the pool by this label; without it the installer
# targets every node in the cluster.
check_contains "labels the pool for the kata-deploy nodeSelector" \
	"${KATA_POOL_LABEL_KEY}=true" "${pool_create}"
# Nothing but e2e work belongs on a billable nested-virt node.
check_contains "taints the pool for e2e work only" \
	"--node-taints=dedicated=e2e:NoSchedule" "${pool_create}"
# Scale-to-zero is what makes a standing nested-virt pool cost nothing at rest.
check_contains "leaves the pool autoscaling from zero" \
	"--min-nodes=0" "${pool_create}"

# --- idempotence: a second run must not disturb the live pool ----------------

reset_stubs
POOL_EXISTS=0
run_main
check "a re-run against an existing pool succeeds" 0 "${MAIN_RC}"
check_not_contains "never re-creates a pool that already exists" \
	"node-pools create" "$(cat "${CALL_LOG}")"
check_contains "says why the create was skipped" \
	"already exists" "${MAIN_OUT}"
# The rest of the setup is upserts, so a re-run must still reach them.
check_contains "still installs Kata on a re-run" \
	"upgrade --install kata-deploy" "$(call_line 'kata-deploy')"
check_contains "still upserts the App Secret on a re-run" \
	"create secret generic github-app-v1" "$(call_line 'create secret')"

# --- nothing is written before the target cluster is pinned ------------------

# The context is process-wide and shared with whatever else is using
# ~/.kube/config, so every write below has to sit behind the verification.
reset_stubs
run_main
check_before "verifies the context before installing Kata" \
	"config current-context" "kata-deploy"
check_before "verifies the context before creating the namespace" \
	"config current-context" "create namespace"
check_before "verifies the context before creating the App Secret" \
	"config current-context" "create secret"

reset_stubs
CONTEXT="gke_other-proj_us-west1-a_prod"
run_main
check "a context that is not the target fails the setup" 1 "${MAIN_RC}"
check_not_contains "never installs Kata onto the wrong cluster" \
	"kata-deploy" "$(cat "${CALL_LOG}")"
check_not_contains "never creates the App Secret on the wrong cluster" \
	"create secret" "$(cat "${CALL_LOG}")"

# --- the App key: abort on a miss, and never through argv --------------------

reset_stubs
KEY_HEX=""
run_main
check "an empty keychain read fails the setup" 1 "${MAIN_RC}"
# A Secret built from an empty key surfaces much later as an opaque GAG auth
# failure, with nothing pointing back here.
check_not_contains "never creates a Secret from an empty key" \
	"create secret" "$(cat "${CALL_LOG}")"
check_contains "says the keychain read came back empty" \
	"private key from keychain is empty" "${MAIN_OUT}"

reset_stubs
KEY_HEX="0123456789abcdef"
run_main
secret_call="$(call_line 'create secret')"
# --from-file, never --from-literal: argv is readable by any process on the box
# and lands in shell history.
check_contains "passes the private key by file" \
	"--from-file=privateKey=" "${secret_call}"
check_not_contains "never passes the private key through argv" \
	"--from-literal=privateKey" "${secret_call}"
# The decoded key itself must not appear anywhere in what was executed.
check_not_contains "never puts key material on a command line" \
	"0123456789abcdef" "$(cat "${CALL_LOG}")"
# The temp file the key was written to is removed on the way out.
# Read the path back rather than asserting on it directly: an empty capture
# would make the removal check below pass against `ls ""`, so it is checked
# first — otherwise a cascade from the assertion above turns this one vacuous.
pem_file="$(printf '%s\n' "${secret_call}" | sed -n 's/.*--from-file=privateKey=\([^ ]*\).*/\1/p')"
check_contains "names the key file it created the Secret from" "/" "${pem_file}"
check "cleans up the key temp file" "" "$(ls "${pem_file}" 2>/dev/null || true)"

# --- the namespace carries the tenant marker and nothing isolation-specific --

reset_stubs
run_main
label_call="$(call_line 'label namespace')"
check_contains "marks the namespace as a managed tenant" \
	"actions-gateway.com/tenant=managed" "${label_call}"
# The security profile and the PSA level are isolation-specific (dind is
# privileged, kata needs guest-scoped capability adds), so they belong to the
# overlay e2e-start.sh applies. Stamping one here would fight that overlay.
check_not_contains "leaves the security profile to the variant overlay" \
	"security-profile" "${label_call}"
check_not_contains "leaves the PSA level to the variant overlay" \
	"pod-security.kubernetes.io" "${label_call}"

# --- the kata RuntimeClass alias ---------------------------------------------

# `scheduling` accepts only nodeSelector and tolerations; an earlier revision
# nested them under scheduling.nodeClassification and the API server rejected
# the object outright (Q226).
check_not_contains "never re-introduces the rejected nodeClassification field" \
	"nodeClassification" "$(cat "${WORKDIR}/manifests.yaml")"
check_contains "aliases kata onto the chart-owned kata-qemu handler" \
	"handler: kata-qemu" "$(cat "${WORKDIR}/manifests.yaml")"
# The pool is tainted dedicated=e2e:NoSchedule and the chart ships no
# tolerations, so without these the installer can never reach the only nodes it
# targets (found live under Q286).
check_contains "tolerates the e2e taint so kata-deploy can land" \
	"tolerations[0].value=e2e" "$(call_line 'kata-deploy')"

# --- the confirmation gate ---------------------------------------------------

reset_stubs
run_main
check_contains "names the project being spent against" \
	"${PROJECT}" "${MAIN_OUT}"
check_contains "names the cluster being written to" \
	"${CLUSTER}" "${MAIN_OUT}"

# A declined confirmation must leave the account exactly as it was found: this
# create is billable and the pool it makes is nested-virt.
reset_stubs
unset ASSUME_YES
run_main
check "a declined confirmation fails the setup" 1 "${MAIN_RC}"
check_not_contains "creates nothing when the confirmation is declined" \
	"node-pools create" "$(cat "${CALL_LOG}")"
check_not_contains "creates no Secret when the confirmation is declined" \
	"create secret" "$(cat "${CALL_LOG}")"

# --- missing configuration aborts before anything runs -----------------------

for missing in PROJECT CLUSTER ZONE APP_ID INSTALLATION_ID; do
	reset_stubs
	# A nameref rather than eval: shellcheck can see the write through it, and
	# the value never round-trips through a string the shell re-parses.
	declare -n slot="${missing}"
	saved="${slot}"
	slot=""
	run_main
	check "an unset ${missing} fails the setup" 1 "${MAIN_RC}"
	check_not_contains "creates nothing with ${missing} unset" \
		"node-pools create" "$(cat "${CALL_LOG}")"
	slot="${saved}"
	unset -n slot
done

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all e2e-setup.sh tests passed"
