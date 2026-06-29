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
#   - curl, ca-certificates, xz-utils, sudo: the shellcheck job downloads a
#     pinned shellcheck tarball (.tar.xz) from a GitHub release and `sudo install`s
#     it; setup-go likewise pulls Go from the actions/go-versions GitHub release.
# Both downloads are github.com traffic, which the tenant egress NetworkPolicy
# permits. The vendor-check/tidy-check jobs are NOT unblocked by this image —
# they re-fetch modules from proxy.golang.org on a cold cache, which the
# GitHub-only egress allowlist blocks; that needs a GOPROXY/egress follow-up.
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
# The upstream actions-runner base ends as USER runner (UID 1001); pin it
# explicitly so the non-root guarantee is local to this Dockerfile and survives
# a base-image change.
USER runner
