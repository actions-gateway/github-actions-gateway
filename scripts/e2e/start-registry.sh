#!/usr/bin/env bash
# start-registry.sh — start the local OCI registry container that kind nodes
# pull from and that host-side `docker buildx bake` pushes to. Idempotent.
#
# Split out of kind-with-registry.sh so CI can bring the registry up first and
# kick off the image build in the background while the kind cluster is still
# being created. The build only needs this registry (reachable on the host at
# 127.0.0.1:${REGISTRY_PORT}), not the cluster, so the two overlap.
#
# Environment:
#   REGISTRY_NAME  — registry container name (default: kind-registry)
#   REGISTRY_PORT  — host port the registry binds to (default: 5000)

set -euo pipefail

REGISTRY_NAME=${REGISTRY_NAME:-kind-registry}
REGISTRY_PORT=${REGISTRY_PORT:-5000}

# Start the registry container if it isn't already running.
running="$(docker inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null || true)"
if [[ "${running}" != 'true' ]]; then
  echo "==> starting registry container ${REGISTRY_NAME} on host 127.0.0.1:${REGISTRY_PORT}"
  # Publish on the IPv4 loopback only. Callers must therefore reference the
  # registry as 127.0.0.1:${REGISTRY_PORT}, NOT localhost — Docker daemon IPv6 is
  # not guaranteed on GitHub runners, so a pusher that resolves "localhost" to
  # IPv6 [::1] first would hit a closed port and fail intermittently with
  # "connect: connection refused". All host-side refs (docker-bake.hcl
  # IMAGE_REGISTRY, the *_IMG env vars, the kind certs.d host dir) use 127.0.0.1
  # to stay deterministically IPv4.
  docker run \
    -d --restart=always \
    -p "127.0.0.1:${REGISTRY_PORT}:5000" \
    --network bridge \
    --name "${REGISTRY_NAME}" \
    registry:2 >/dev/null
else
  echo "==> registry container ${REGISTRY_NAME} already running"
fi

# Wait for the registry to accept connections — `docker run -d` returns once the
# container is started, not when registry:2 is actually listening. Without this,
# a fast caller (buildx push) can race and fail on the first attempt.
echo "==> waiting for registry to accept connections"
for i in $(seq 1 10); do
  if curl -fsS "http://127.0.0.1:${REGISTRY_PORT}/v2/" >/dev/null 2>&1; then
    break
  fi
  if (( i == 10 )); then
    echo "registry did not become ready within 10s" >&2
    exit 1
  fi
  sleep 1
done
