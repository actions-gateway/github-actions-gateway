#!/usr/bin/env bash
# autoscaler-cluster.sh — stand up a kind cluster running a REAL upstream
# cluster-autoscaler against fake kwok nodes (Q474). Idempotent: safe to re-run.
#
# Why this exists: the capacity gate's elastic-cluster signal
# (cmd/agc/internal/controller/autoscaler_verdict.go) recognizes cluster-autoscaler
# by two Event reasons and a reporter name. Those are upstream's strings, pinned in
# our unit table from recorded samples, and a reword upstream silently disables the
# gate — a missed declination fails open, so nothing breaks loudly. This cluster is
# what turns that silence into a test failure.
#
# The kwok cloud provider is upstream's own answer to "run CA without a cloud":
# it materializes each scaled-up node as a fake Node object that kwok marks Ready.
# So the autoscaler, its evaluation, and its events are all real; only the nodes
# are not.
#
# Environment:
#   AUTOSCALER_CLUSTER  — kind cluster name (default: gag-autoscaler)
#   KIND_NODE_IMAGE     — pin the node image, e.g. kindest/node:vX.Y.Z@sha256:...
#                         (optional; leave UNSET here — riding kind's release
#                         default is what ties the cluster's Kubernetes minor to
#                         the kind version, and so to CA_VERSION's minor below.
#                         The e2e tier pins its node image DOWN; this harness
#                         must not copy that. See docs/development/testing.md.)
#   CA_VERSION          — cluster-autoscaler image tag (default below). Bumping
#                         this is the point: a bump that reworded the vocabulary
#                         fails `make test-autoscaler`. CI runs the gate on any
#                         PR that edits this file (.github/workflows/autoscaler-drift.yml),
#                         so a bump cannot land untested (Q480). Patch bumps
#                         inside the pinned minor arrive weekly by themselves
#                         (updatecli.d/cluster-autoscaler.yaml, Q483); the minor
#                         still moves by hand, with the kind bump.
#   KWOK_VERSION        — kwok release providing the fake kubelet (default below)
#
# After this script runs, `make test-autoscaler` drives pods through it.
# Tear down with `make autoscaler-cluster-delete`.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

AUTOSCALER_CLUSTER=${AUTOSCALER_CLUSTER:-gag-autoscaler}
# Keep CA_VERSION on the same minor as the node image: cluster-autoscaler is
# released per Kubernetes minor and its scheduler-framework predicates are the
# ones from that release. With KIND_NODE_IMAGE unset that minor is kind's
# default node image (kind v0.32.0 -> kindest/node:v1.36.1), so a kind bump that
# moves the default is what moves this MINOR — by hand, in that PR.
#
# The PATCH moves on its own: updatecli.d/cluster-autoscaler.yaml rewrites the
# line below weekly to the newest patch published inside this minor, and the
# resulting PR trips the drift gate's path filter (Q483). Keep the assignment on
# one line in this exact shape — the manifest matches it by regex.
CA_VERSION=${CA_VERSION:-v1.36.1}
KWOK_VERSION=${KWOK_VERSION:-v0.8.0}

KUBE_CONTEXT="kind-${AUTOSCALER_CLUSTER}"
KWOK_BASE="https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}"

# kubectl_ctx pins every call to this script's own cluster. Parallel sessions
# share the ambient kubectl context, and an unpinned `kubectl apply` of a
# cluster-autoscaler with node create/delete rights is not a mistake worth
# leaving reachable.
kubectl_ctx() {
	kubectl --context "${KUBE_CONTEXT}" "$@"
}

create_cluster() {
	if kind get clusters 2>/dev/null | grep -qx "${AUTOSCALER_CLUSTER}"; then
		echo "==> kind cluster ${AUTOSCALER_CLUSTER} already exists"
		return 0
	fi
	local create_args=(create cluster --name "${AUTOSCALER_CLUSTER}" --wait 120s)
	if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
		echo "==> using pinned node image ${KIND_NODE_IMAGE}"
		create_args+=(--image "${KIND_NODE_IMAGE}")
	fi
	echo "==> creating kind cluster ${AUTOSCALER_CLUSTER}"
	kind "${create_args[@]}"
}

install_kwok() {
	echo "==> installing kwok ${KWOK_VERSION}"
	kubectl_ctx apply -f "${KWOK_BASE}/kwok.yaml"
	# The fast stages are what make a created Node reach Ready (and a pod placed on
	# it reach Running) without a kubelet. Without them CA's scale-up succeeds and
	# then times out waiting for a node that never registers.
	kubectl_ctx apply -f "${KWOK_BASE}/stage-fast.yaml"
	kubectl_ctx -n kube-system rollout status deploy/kwok-controller --timeout=180s
}

install_autoscaler() {
	echo "==> installing cluster-autoscaler ${CA_VERSION} (kwok provider)"
	kubectl_ctx apply -f test/autoscaler/kwok-provider.yaml
	# awk, not sed: the image ref contains '/' and ':' and the style guide keeps
	# variable substitution out of sed's delimiter/metacharacter minefield.
	awk -v tag="${CA_VERSION}" '{ gsub(/CA_VERSION/, tag); print }' \
		test/autoscaler/cluster-autoscaler.yaml | kubectl_ctx apply -f -
	kubectl_ctx -n kube-system rollout status deploy/cluster-autoscaler --timeout=180s
}

# wait_for_autoscaler_loop blocks until cluster-autoscaler has completed a full
# evaluation, which is the first moment its events can be trusted to be absent
# rather than merely not-yet-emitted. It reads the status ConfigMap CA maintains,
# because a Ready Deployment only means the process started — it has not
# necessarily loaded the kwok provider config or seen a node group yet, and a test
# that raced that would read "no declination" from a cluster that had not looked.
wait_for_autoscaler_loop() {
	local deadline=$((SECONDS + 120))
	echo "==> waiting for cluster-autoscaler's first evaluation"
	while ((SECONDS < deadline)); do
		if kubectl_ctx -n kube-system get configmap cluster-autoscaler-status \
			-o jsonpath='{.data}' 2>/dev/null | grep -q 'standard'; then
			echo "==> cluster-autoscaler is evaluating node groups"
			return 0
		fi
		sleep 5
	done
	echo "error: cluster-autoscaler never reported a node group; dumping diagnostics" >&2
	kubectl_ctx -n kube-system get configmap cluster-autoscaler-status -o yaml || true
	kubectl_ctx -n kube-system logs deploy/cluster-autoscaler --tail=100 || true
	return 1
}

main() {
	create_cluster
	install_kwok
	install_autoscaler
	wait_for_autoscaler_loop
	echo
	echo "==> ready. Run the live matcher test with:"
	echo "      make test-autoscaler"
}

main "$@"
