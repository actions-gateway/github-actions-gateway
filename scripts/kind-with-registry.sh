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
  # Concatenate the user's config file with the containerd patch and feed the
  # result to kind via stdin.
  cat "${KIND_CONFIG}" - <<EOF | kind create cluster "${create_args[@]}"
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
EOF
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
