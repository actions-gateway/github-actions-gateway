#!/usr/bin/env bash
# karpenter-cluster.sh — stand up a kind cluster running a REAL upstream
# Karpenter against fake kwok nodes (Q479). Idempotent: safe to re-run.
#
# Why this exists: the capacity gate's elastic-cluster signal
# (cmd/agc/internal/controller/autoscaler_verdict.go) recognizes Karpenter's
# declination by kube-scheduler's own reason string, FailedScheduling — the arm
# whose entire correctness is the reporter discrimination. Those are upstream's
# strings and attribution habits, pinned in our unit table from recorded
# samples, and a change upstream silently disables the gate: a missed
# declination fails open, so nothing breaks loudly. This cluster is what turns
# that silence into a test failure — the Karpenter counterpart of
# scripts/e2e/autoscaler-cluster.sh.
#
# Unlike cluster-autoscaler, upstream publishes NO image for its kwok provider —
# the project's own workflow is `ko build` from a checkout. So this script
# clones the pinned tag, builds the provider with the repo's Go toolchain, and
# reproduces ko's output shape (static binary, empty base) via
# test/karpenter/Dockerfile, loading it straight into the kind cluster.
#
# Environment:
#   KARPENTER_CLUSTER   — kind cluster name (default: gag-karpenter)
#   KIND_NODE_IMAGE     — pin the node image (optional; leave UNSET. Karpenter
#                         is not released per Kubernetes minor — one release
#                         supports a wide range — so unlike the
#                         cluster-autoscaler harness there is no minor to keep
#                         in lockstep; riding kind's default is simply the
#                         cheapest current cluster.)
#   KARPENTER_VERSION   — kubernetes-sigs/karpenter tag to build (default
#                         below). Bumping this is the point: a bump that changed
#                         the event vocabulary, the reporter attribution, or the
#                         recorder generation fails `make test-karpenter`. CI
#                         runs the gate on any PR that edits this file
#                         (.github/workflows/autoscaler-drift.yml). Bumps arrive
#                         weekly by themselves (updatecli.d/karpenter.yaml,
#                         Q529) — the latest release, whatever the minor;
#                         Karpenter is not released per Kubernetes minor, so
#                         there is no kind coupling to wait for.
#   KWOK_VERSION        — kwok release providing the fake kubelet (default
#                         below; kept the same as autoscaler-cluster.sh)
#
# After this script runs, `make test-karpenter` drives pods through it.
# Tear down with `make karpenter-cluster-delete`.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

KARPENTER_CLUSTER=${KARPENTER_CLUSTER:-gag-karpenter}
# Moves on its own: updatecli.d/karpenter.yaml rewrites the line below weekly to
# the latest upstream release, and the resulting PR trips the drift gate's path
# filter (Q529). Keep the assignment on one line in this exact shape — the
# manifest matches it by regex.
KARPENTER_VERSION=${KARPENTER_VERSION:-v1.14.0}
KWOK_VERSION=${KWOK_VERSION:-v0.8.0}

KUBE_CONTEXT="kind-${KARPENTER_CLUSTER}"
KWOK_BASE="https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}"
# The checkout is keyed by tag so a version bump can never reuse a stale tree;
# tmp/ is the repo's gitignored scratch space.
KARPENTER_SRC="tmp/karpenter-src/${KARPENTER_VERSION}"
IMAGE="gag-karpenter-kwok:${KARPENTER_VERSION}"

# kubectl_ctx pins every call to this script's own cluster. Parallel sessions
# share the ambient kubectl context, and an unpinned `kubectl apply` of a
# controller with node create/delete rights is not a mistake worth leaving
# reachable.
kubectl_ctx() {
	kubectl --context "${KUBE_CONTEXT}" "$@"
}

create_cluster() {
	if kind get clusters 2>/dev/null | grep -qx "${KARPENTER_CLUSTER}"; then
		echo "==> kind cluster ${KARPENTER_CLUSTER} already exists"
		return 0
	fi
	local create_args=(create cluster --name "${KARPENTER_CLUSTER}" --wait 120s)
	if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
		echo "==> using pinned node image ${KIND_NODE_IMAGE}"
		create_args+=(--image "${KIND_NODE_IMAGE}")
	fi
	echo "==> creating kind cluster ${KARPENTER_CLUSTER}"
	kind "${create_args[@]}"
}

install_kwok() {
	echo "==> installing kwok ${KWOK_VERSION}"
	kubectl_ctx apply -f "${KWOK_BASE}/kwok.yaml"
	# The fast stages are what make a created Node reach Ready (and a pod placed
	# on it reach Running) without a kubelet. The provider annotates every node
	# it creates with kwok.x-k8s.io/node=fake, which is exactly the selector the
	# stock kwok deployment manages.
	kubectl_ctx apply -f "${KWOK_BASE}/stage-fast.yaml"
	kubectl_ctx -n kube-system rollout status deploy/kwok-controller --timeout=180s
}

fetch_karpenter_source() {
	if [[ -d "${KARPENTER_SRC}/.git" ]]; then
		echo "==> karpenter ${KARPENTER_VERSION} source already present"
		return 0
	fi
	echo "==> cloning kubernetes-sigs/karpenter ${KARPENTER_VERSION}"
	# Clone to a staging path and move into place only when complete, so an
	# interrupted clone cannot masquerade as a finished one on the next run.
	rm -rf "${KARPENTER_SRC}" "${KARPENTER_SRC}.partial"
	git clone --depth 1 --branch "${KARPENTER_VERSION}" \
		https://github.com/kubernetes-sigs/karpenter "${KARPENTER_SRC}.partial"
	mv "${KARPENTER_SRC}.partial" "${KARPENTER_SRC}"
}

build_provider_image() {
	# The binary runs inside the kind node container, so its architecture is the
	# docker server's, not necessarily the host toolchain's default.
	local arch
	arch="$(docker version --format '{{.Server.Arch}}')"
	echo "==> building the kwok provider (linux/${arch}) from source"
	local bin_dir="${KARPENTER_SRC}-bin"
	mkdir -p "${bin_dir}"
	# GOWORK=off: the checkout sits under this repo's root, and Go would
	# otherwise find our go.work and refuse to build a module that is not in it.
	# THROTTLE_PREFIX is the desktop-safety demotion from local-throttle.sh (a
	# no-op on CI/headless) — an unthrottled build of a tree this size can
	# starve a GUI dev machine (Q92).
	init_throttle
	# shellcheck disable=SC2086  # the throttle prefix word-splits intentionally
	(cd "${KARPENTER_SRC}" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
		$THROTTLE_PREFIX go build -o "${REPO_ROOT}/${bin_dir}/karpenter-kwok" ./kwok)
	docker build -f test/karpenter/Dockerfile -t "${IMAGE}" "${bin_dir}"
	kind load docker-image "${IMAGE}" --name "${KARPENTER_CLUSTER}"
}

install_karpenter() {
	echo "==> installing karpenter ${KARPENTER_VERSION} (kwok provider)"
	kubectl_ctx apply -f "${KARPENTER_SRC}/kwok/charts/crds"
	# The chart is upstream's own (from the pinned checkout), pointed at the
	# locally built image. kube-system because the chart runs the controller at
	# system-cluster-critical priority.
	# The two explicit feature gates work around an upstream chart bug at
	# v1.14.0: the deployment template renders them into FEATURE_GATES but
	# values.yaml omits their keys, and the empty string panics the controller
	# at startup ("invalid value of StaticCapacity"). Both default to false.
	helm upgrade --install karpenter "${KARPENTER_SRC}/kwok/charts" \
		--kube-context "${KUBE_CONTEXT}" --namespace kube-system --skip-crds \
		--set controller.image.repository="${IMAGE%%:*}" \
		--set controller.image.tag="${IMAGE##*:}" \
		--set settings.featureGates.staticCapacity=false \
		--set settings.featureGates.capacityBuffer=false
	kubectl_ctx -n kube-system rollout status deploy/karpenter --timeout=180s
}

install_nodepool() {
	echo "==> installing the test node pool"
	kubectl_ctx apply -f test/karpenter/nodepool.yaml
	# NodePool Ready proves the whole chain — the controller came up, resolved
	# the KWOKNodeClass, and accepted the pool — which is the first moment a
	# test can trust an absent event to mean "no verdict yet" rather than
	# "nobody looked".
	kubectl_ctx wait nodepool/standard --for=condition=Ready --timeout=120s
}

main() {
	create_cluster
	install_kwok
	fetch_karpenter_source
	build_provider_image
	install_karpenter
	install_nodepool
	echo
	echo "==> ready. Run the live matcher test with:"
	echo "      make test-karpenter"
}

main "$@"
