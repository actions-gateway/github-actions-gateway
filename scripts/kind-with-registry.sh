#!/usr/bin/env bash
# kind-with-registry.sh — start a local OCI registry and a kind cluster
# wired to use it. Idempotent: safe to re-run.
#
# Environment:
#   KIND_CLUSTER   — cluster name (required)
#   KIND_CONFIG    — kind cluster config file (required)
#   REGISTRY_NAME  — registry container name (default: kind-registry)
#   REGISTRY_PORT  — host port the registry binds to (default: 5000)
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

# 1. Start the registry container if it isn't already running.
running="$(docker inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null || true)"
if [ "${running}" != 'true' ]; then
  echo "==> starting registry container ${REGISTRY_NAME} on host 127.0.0.1:${REGISTRY_PORT}"
  docker run \
    -d --restart=always \
    -p "127.0.0.1:${REGISTRY_PORT}:5000" \
    --network bridge \
    --name "${REGISTRY_NAME}" \
    registry:2 >/dev/null
else
  echo "==> registry container ${REGISTRY_NAME} already running"
fi

# Wait for the registry to accept connections — `docker run -d` returns once
# the container is started, not when registry:2 is actually listening. Without
# this, a fast caller (buildx push) can race and fail on the first attempt.
echo "==> waiting for registry to accept connections"
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS "http://127.0.0.1:${REGISTRY_PORT}/v2/" >/dev/null 2>&1; then
    break
  fi
  if [ "${i}" = '10' ]; then
    echo "registry did not become ready within 10s" >&2
    exit 1
  fi
  sleep 1
done

# 2. Create the kind cluster (with a containerd patch enabling certs.d) if it
#    does not already exist. The patch tells containerd to honour per-registry
#    config files under /etc/containerd/certs.d, which step 3 populates.
if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
  echo "==> kind cluster ${KIND_CLUSTER} already exists"
else
  echo "==> creating kind cluster ${KIND_CLUSTER} (config: ${KIND_CONFIG})"
  # Concatenate the user's config file with the containerd patch and feed the
  # result to kind via stdin.
  cat "${KIND_CONFIG}" - <<EOF | kind create cluster --name "${KIND_CLUSTER}" --config=-
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
