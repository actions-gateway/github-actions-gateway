# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# ^ BuildKit frontend pinned by digest (re-pin when bumping the 1.7 tag:
#   docker buildx imagetools inspect docker/dockerfile:1.7).
#
# Build-capable dogfood e2e runner image (Q231). Extends the dogfood-runner
# image (Q239: build-essential on the upstream actions-runner) with the tools the
# e2e job assumes preinstalled on a GitHub-hosted runner but that neither the
# upstream runner nor dogfood-runner ships:
#   - docker CLI + buildx plugin: e2e-reusable.yml calls `docker {load,pull,tag,
#     save}` and `docker buildx` directly to bake the six images and load the kind
#     node image. The DinD sidecar provides the *daemon* (DOCKER_HOST=tcp://
#     localhost:2375); this image provides the *client*.
#   - helm: the e2e suite installs the GMC via `helm upgrade --install`.
# kind is NOT baked — the workflow installs a pinned kind binary itself; Go comes
# from setup-go; make/git/gcc come from the dogfood-runner base.
#
# This is a dev convenience, NOT a signed release artifact (publish.yml signs only
# the five product images). Built ad hoc by scripts/dogfood-e2e-runner-build.sh,
# pushed to GHCR, and pinned as the e2e tenant RunnerTemplate workerImage. The
# Q235 wrapper is still injected on top at provision time.
#
# Bump the base tag in lockstep with scripts/dogfood/runner/Dockerfile's runner
# version when that moves.
FROM ghcr.io/actions-gateway/dogfood-runner:2.335.1
USER root

# Docker CLI + buildx plugin from Docker's official apt repo. Unpinned apt
# versions are deliberate (dev/reference image tracking the repo's current
# toolchain), as in the dogfood-runner base.
# hadolint ignore=DL3008,DL3009
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && . /etc/os-release \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli docker-buildx-plugin \
    && rm -rf /var/lib/apt/lists/*

# helm (static release tarball; no pipe, to satisfy hadolint DL4006).
ARG HELM_VERSION=v3.16.3
RUN set -eux; \
    curl -fsSL -o /tmp/helm.tgz "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz"; \
    tar -xzf /tmp/helm.tgz -C /tmp; \
    install -m 0755 /tmp/linux-amd64/helm /usr/local/bin/helm; \
    rm -rf /tmp/helm.tgz /tmp/linux-amd64; \
    docker --version; docker buildx version; helm version --short

# kubectl + jq — `make e2e-cluster` and the e2e suite use them (preinstalled on
# GitHub-hosted ubuntu-latest; absent from the actions-runner base). kubectl is
# pinned to the kind node image's Kubernetes version. (kustomize/cosign are NOT
# in the e2e-cluster path — only the separate manifest-validate gate — so omitted.)
ARG KUBECTL_VERSION=v1.35.0
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends jq \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod 0755 /usr/local/bin/kubectl \
    && kubectl version --client \
    && jq --version

# Drop back to the non-root runner user (UID 1001), as the base image ends.
USER runner
