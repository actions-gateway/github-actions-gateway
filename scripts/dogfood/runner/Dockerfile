# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# ^ BuildKit frontend pinned by digest. Dependabot's docker ecosystem does not
#   bump syntax directives, so re-pin manually when bumping the 1.7 tag:
#     docker buildx imagetools inspect docker/dockerfile:1.7
#
# Build-capable dogfood runner image (Q239). This is NOT a released, signed
# artifact — it is a dev convenience that lets the GKE dogfood run THIS repo's
# own make-based CI green. publish.yml builds and signs only the five product
# images (gmc/agc/proxy/worker/wrapper); this one is built ad hoc by
# scripts/dogfood-runner-build.sh and pushed to GHCR for the dogfood tenant to
# pin as its RunnerTemplate workerImage. The Q235 wrapper is still injected on
# top of it at provision time, exactly as it is on the bare upstream default.
#
# Why it exists: the bare upstream ghcr.io/actions/actions-runner (the AGC's
# DefaultWorkerImage) has no build toolchain, so every `make …` job fails
# `exit 127: make: command not found`. This image adds just enough to run the
# offline-capable migrated jobs (lint, shellcheck, unit-test, coverage):
#   - build-essential: make + a C toolchain (the -race unit suite needs cgo/gcc)
#   - curl, ca-certificates, xz-utils: fetch + unpack the pinned shellcheck
#     tarball at BUILD time (see the bake-in step below)
#   - sudo: retained for parity with the GitHub-hosted runner; the shellcheck
#     job uses it only on its download fallback path
#
# Egress caveat (Q246): GitHub *release-asset* downloads 302-redirect from
# github.com to objects.githubusercontent.com — a Fastly-fronted CDN that is NOT
# in the tenant's GitHub-CIDR egress allowlist, so a runtime download from the
# worker times out. (An earlier version of this comment wrongly assumed the
# download was permitted github.com traffic.) The fix is to bake release assets
# in at build time, where the build host has unrestricted egress: the pinned
# shellcheck binary is baked below, and the workflow's install step skips the
# download when this exact version is already present. setup-go's Go-toolchain
# download (from the actions/go-versions release) hits the same CDN and is the
# remaining offline-job egress dependency — tracked on Q246.
#
# The vendor-check/tidy-check jobs are unblocked separately by the in-cluster
# Athens Go module proxy (Q244), not by this image.
#
# Pinned to the same runner version + multi-arch index digest as
# cmd/worker/Dockerfile and cmd/agc/names (RunnerVersion / WorkerImageDigest) so
# the dogfood runs the same runner the AGC would otherwise gap-fill. Bump it in
# lockstep with those when the runner version moves.
FROM ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29
# apt runs as root, then the image drops back to the non-root runner user.
USER root
# Unpinned apt versions are deliberate here: this is a dev/reference image that
# tracks the base distro's current toolchain, not a reproducible release artifact
# (the five signed images carry no apt layer at all). Pinning would rot against the
# base's moving package index for no supply-chain gain on a non-shipped image.
# The ignore directive must sit immediately above RUN for hadolint to honor it.
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        sudo \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*
# Bake the pinned shellcheck (Q246). Downloading it at runtime on the worker
# fails: the GitHub release asset 302-redirects to objects.githubusercontent.com,
# off the tenant's GitHub-CIDR egress allowlist (see the Egress caveat above).
# Baking it here — on a build host with unrestricted egress — removes that
# runtime dependency; the workflow's install step skips the download when this
# exact version is already present. Keep ARG version+checksum in lockstep with
# SHELLCHECK_VERSION / SHELLCHECK_SHA256 in .github/workflows/unit-test.yml —
# updatecli bumps all four together (updatecli.d/shellcheck.yaml).
ARG SHELLCHECK_VERSION=v0.11.0
ARG SHELLCHECK_SHA256=8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198
RUN set -eux; \
    base="shellcheck-${SHELLCHECK_VERSION}"; \
    url="https://github.com/koalaman/shellcheck/releases/download/${SHELLCHECK_VERSION}/${base}.linux.x86_64.tar.xz"; \
    curl -fsSL "$url" -o /tmp/sc.tar.xz; \
    printf '%s  /tmp/sc.tar.xz\n' "${SHELLCHECK_SHA256}" > /tmp/sc.sha256; \
    sha256sum -c /tmp/sc.sha256; \
    tar -xf /tmp/sc.tar.xz -C /tmp; \
    install "/tmp/${base}/shellcheck" /usr/local/bin/shellcheck; \
    rm -rf /tmp/sc.tar.xz /tmp/sc.sha256 "/tmp/${base}"; \
    shellcheck --version
# The upstream actions-runner base ends as USER runner (UID 1001); pin it
# explicitly so the non-root guarantee is local to this Dockerfile and survives
# a base-image change.
USER runner
