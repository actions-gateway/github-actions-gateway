#!/usr/bin/env bash
# kind-load-image.sh — load registry-pulled images onto a kind cluster's nodes.
#
# `kind load docker-image` cannot load a multi-arch image pulled into a Docker
# daemon that uses the containerd image store. `docker pull` fetches only the
# host's platform but keeps the full index, `docker save` exports that index,
# and kind imports it with `ctr images import --all-platforms`, which fails on
# the first platform whose content was never pulled:
#
#   ctr: content digest sha256:...: not found
#
# Measured 2026-10-27 on Docker 29.6.2 (containerd store) with kind v0.32.0
# against registry.k8s.io/pause:3.10, and on the hosted runner's Docker 29.4.2
# with kind v0.33.0 against quay.io/calico/calico:v3.33.0, where the missing
# digest was the index's arm64 entry. Docker 28.0.4's classic store, which the
# hosted runner used until then, loaded the same kind of pull cleanly.
#
# So export only the daemon's own platform (`docker save --platform`) and load
# that archive. It loads on both stores, and from both a fresh pull and an
# image restored by `docker load` from a cached tarball. A daemon whose `save`
# has no --platform flag predates the containerd store default, so plain
# `kind load docker-image` is used there.
#
# Usage:
#   scripts/e2e/kind-load-image.sh <cluster> <image-ref>...

set -euo pipefail
shopt -s inherit_errexit

if (( $# < 2 )); then
  echo "usage: $0 <cluster> <image-ref>..." >&2
  exit 2
fi
cluster="$1"
shift

platform="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

for img in "$@"; do
  echo "==> loading ${img} (${platform}) onto kind cluster ${cluster}"
  archive="${work}/image.tar"
  if docker save --platform "${platform}" -o "${archive}" "${img}" 2>"${work}/save.err"; then
    kind load image-archive --name "${cluster}" "${archive}"
  elif grep -q 'unknown flag: --platform' "${work}/save.err"; then
    kind load docker-image --name "${cluster}" "${img}"
  else
    cat "${work}/save.err" >&2
    exit 1
  fi
  rm -f "${archive}"
done
