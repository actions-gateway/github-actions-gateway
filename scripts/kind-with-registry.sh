#!/usr/bin/env bash
# kind-with-registry.sh — start a local OCI registry and a kind cluster
# wired to use it. Idempotent: safe to re-run.
#
# Environment:
#   KIND_CLUSTER     — cluster name (required)
#   KIND_CONFIG      — kind cluster config file (required)
#   REGISTRY_NAME    — registry container name (default: kind-registry)
#   REGISTRY_PORT    — host port the registry binds to (default: 5000)
#   KIND_NODE_IMAGE  — pin the node image, e.g. kindest/node:vX.Y.Z@sha256:...
#                      (optional; when unset kind picks its release default)
#   KIND_CNI         — "kindnet" (default: kind's bundled CNI) or "calico".
#                      calico creates the cluster with disableDefaultCNI and
#                      podSubnet 192.168.0.0/16, applies the pinned Calico
#                      manifest, and waits for it to be ready. Use this profile
#                      to observe NetworkPolicy egress negatives enforce at
#                      runtime — kindnet's kube-network-policies does not drop
#                      egress traffic (see docs/plan/worker-egress-proxy.md, Q7b).
#   CALICO_VERSION   — Calico release tag for KIND_CNI=calico (default: v3.31.5)
#
# After this script runs:
#   * Images pushed to localhost:${REGISTRY_PORT}/... from the host are
#     pull-able by kind nodes inside the cluster.
#   * The `local-registry-hosting` ConfigMap in `kube-public` advertises the
#     endpoint per https://kind.sigs.k8s.io/docs/user/local-registry/.
#
# Based on the upstream recipe at
#   https://kind.sigs.k8s.io/docs/user/local-registry/

set -euo pipefail

: "${KIND_CLUSTER:?KIND_CLUSTER is required}"
: "${KIND_CONFIG:?KIND_CONFIG is required}"
REGISTRY_NAME=${REGISTRY_NAME:-kind-registry}
REGISTRY_PORT=${REGISTRY_PORT:-5000}
KIND_CNI=${KIND_CNI:-kindnet}
CALICO_VERSION=${CALICO_VERSION:-v3.31.5}

case "${KIND_CNI}" in
  kindnet|calico) ;;
  *)
    echo "error: KIND_CNI must be 'kindnet' or 'calico' (got '${KIND_CNI}')" >&2
    exit 1
    ;;
esac

# install_calico applies the pinned Calico manifest and waits for the CNI to be
# ready. Idempotent: re-applying the same manifest is a no-op. Refuses to run
# against a cluster that was created with kindnet — the two CNIs cannot be
# swapped in place; delete and recreate instead.
install_calico() {
  local ctx="kind-${KIND_CLUSTER}"
  if kubectl --context "${ctx}" get daemonset -n kube-system kindnet >/dev/null 2>&1; then
    echo "error: cluster ${KIND_CLUSTER} is running kindnet; a CNI cannot be swapped in place." >&2
    echo "       Delete the cluster (make e2e-cluster-delete) and re-run with KIND_CNI=calico." >&2
    exit 1
  fi
  echo "==> installing Calico ${CALICO_VERSION}"

  # Fetch the manifest to a temp file (retried) rather than applying straight
  # from the URL, so we can both preload its images onto the nodes below and
  # avoid a second network fetch. The RETURN trap cleans it up on normal exit.
  local manifest
  manifest="$(mktemp)"
  # shellcheck disable=SC2064  # intentional: capture this call's manifest path now
  trap "rm -f '${manifest}'" RETURN
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 -o "${manifest}" \
    "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml"

  # Preload any Calico images already present in the local Docker daemon onto the
  # kind nodes so the rollout below pulls nothing from quay.io — the dominant
  # flake/latency source on this CNI's bring-up, and one the local registry
  # cannot cover (it is wired only after Calico is up). CI pre-pulls and caches
  # the images (see .github/workflows/e2e-reusable.yml); a plain local run has
  # none cached and the nodes pull from quay as before. The loaded refs match
  # the manifest's tagged refs, so kubelet's IfNotPresent policy finds them.
  local img
  while read -r img; do
    [[ -z "${img}" ]] && continue
    if docker image inspect "${img}" >/dev/null 2>&1; then
      echo "==> preloading ${img} onto kind nodes"
      kind load docker-image --name "${KIND_CLUSTER}" "${img}"
    fi
  done < <(awk '$1 == "image:" { gsub(/"/, "", $2); print $2 }' "${manifest}" | sort -u)

  kubectl --context "${ctx}" apply -f "${manifest}"
  echo "==> waiting for calico-node DaemonSet rollout"
  # 600s headroom: when images are not preloaded a cold quay.io pull of the
  # calico images on every node takes well over the kubectl default; 300s was
  # observed too tight.
  kubectl --context "${ctx}" rollout status daemonset/calico-node -n kube-system --timeout=600s
  echo "==> waiting for all nodes to be Ready"
  kubectl --context "${ctx}" wait --for=condition=Ready nodes --all --timeout=300s
}

# 1. Bring up the registry (idempotent). Extracted into start-registry.sh so CI
#    can start it early and build images against it while this script goes on to
#    create the cluster.
REGISTRY_NAME="${REGISTRY_NAME}" REGISTRY_PORT="${REGISTRY_PORT}" \
  "$(dirname "$0")/start-registry.sh"

# 2. Create the kind cluster (with a containerd patch enabling certs.d) if it
#    does not already exist. The patch tells containerd to honour per-registry
#    config files under /etc/containerd/certs.d, which step 3 populates.
if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
  echo "==> kind cluster ${KIND_CLUSTER} already exists"
else
  echo "==> creating kind cluster ${KIND_CLUSTER} (config: ${KIND_CONFIG})"
  # Pin the node image when KIND_NODE_IMAGE is set (CI does, to make the cluster
  # K8s version deterministic and to let a pre-pulled/cached image be reused).
  create_args=(--name "${KIND_CLUSTER}" --config=-)
  if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
    echo "==> using pinned node image ${KIND_NODE_IMAGE}"
    create_args+=(--image "${KIND_NODE_IMAGE}")
  fi
  # Concatenate the user's config file with the containerd patch (and, for
  # KIND_CNI=calico, a networking block) and feed the result to kind via stdin.
  # Note: this appends top-level keys, so KIND_CONFIG must not already define
  # `networking:` or `containerdConfigPatches:` (the checked-in configs don't).
  cni_config=""
  if [[ "${KIND_CNI}" == "calico" ]]; then
    # Calico's manifest creates its default IPv4 pool at 192.168.0.0/16; align
    # the cluster podSubnet with it (the Tigera kind quickstart does the same).
    cni_config=$'networking:\n  disableDefaultCNI: true\n  podSubnet: "192.168.0.0/16"'
  fi
  cat "${KIND_CONFIG}" - <<EOF | kind create cluster "${create_args[@]}"
${cni_config}
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
EOF
fi

# With disableDefaultCNI the nodes stay NotReady until a CNI is installed, so
# this must run before anything that waits on node readiness.
if [[ "${KIND_CNI}" == "calico" ]]; then
  install_calico
fi

# 3. Wire each node's containerd to resolve localhost:${REGISTRY_PORT} via the
#    registry container's hostname on the kind network.
REGISTRY_DIR="/etc/containerd/certs.d/localhost:${REGISTRY_PORT}"
for node in $(kind get nodes --name "${KIND_CLUSTER}"); do
  docker exec "${node}" mkdir -p "${REGISTRY_DIR}"
  cat <<EOF | docker exec -i "${node}" cp /dev/stdin "${REGISTRY_DIR}/hosts.toml"
[host."http://${REGISTRY_NAME}:5000"]
EOF
done

# 4. Connect the registry to the kind network so nodes can reach it by name.
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${REGISTRY_NAME}")" = 'null' ]; then
  echo "==> connecting ${REGISTRY_NAME} to the kind network"
  docker network connect kind "${REGISTRY_NAME}"
fi

# 5. Publish the registry endpoint via the kube-public ConfigMap convention.
cat <<EOF | kubectl --context "kind-${KIND_CLUSTER}" apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${REGISTRY_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

echo "==> registry ready at localhost:${REGISTRY_PORT} (kind-network: ${REGISTRY_NAME}:5000)"
