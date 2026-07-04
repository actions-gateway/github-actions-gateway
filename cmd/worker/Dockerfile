# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# ^ BuildKit frontend pinned by digest. Dependabot's docker ecosystem does not
#   bump syntax directives, so re-pin manually when bumping the 1.7 tag:
#     docker buildx imagetools inspect docker/dockerfile:1.7
## Stage 1: build the Go entrypoint wrapper.
## This produces a static binary that reads the job payload Secret
## and feeds it into Runner.Worker via anonymous pipes.
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
# --platform=$BUILDPLATFORM: the builder always runs on the build host's native
# platform and CROSS-COMPILES for $TARGETARCH — no QEMU emulation of the Go
# toolchain on a multi-platform build.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609 AS builder
WORKDIR /src
# Build context is the repo root so the workspace vendor/ is available.
# `go build` auto-selects -mod=vendor when go.work and vendor/ are present.
COPY . .
# TARGETOS/TARGETARCH are populated by BuildKit from the requested target
# platform (the host platform for a plain single-platform build).
ARG TARGETOS TARGETARCH
# -trimpath strips absolute filesystem paths; -ldflags=-buildid= clears the
# build ID — together they make the binary bit-for-bit reproducible (a
# reproducible-build input). At publish time publish.yml emits a signed,
# Rekor-logged build-provenance attestation for the image (SLSA Build L2 via
# GitHub Actions OIDC + Sigstore); reaching SLSA Build L3 would additionally
# require an isolated reusable-workflow builder, not yet adopted.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags=-buildid= -o /wrapper ./cmd/worker

## Stage 2: runner image with the wrapper as ENTRYPOINT.
## Built on top of ghcr.io/actions/actions-runner (the ARC gha-runner-scale-set
## default) so tenants copy-pasting from ARC manifests see the same base. The
## image runs as USER runner (UID 1001) and ships Runner.Worker at
## /home/runner/bin/Runner.Worker. That directory is NOT on the image's default
## PATH, so prepend it here — the wrapper resolves the binary via
## exec.LookPath("Runner.Worker").
##
## Pinned to the multi-arch manifest-index digest for supply-chain integrity
## (M-19 in docs/plan/security.md). The version MUST match the pinned runner
## version in cmd/agc/names (RunnerVersion / WorkerImageDigest) — the single
## source of truth that drives both the AGC's DefaultWorkerImage and the
## GITHUB_RUNNER_VERSION the GMC injects (the agent.version GitHub validates at
## session creation). The lockstep test cmd/agc/names/runner_version_test.go
## fails CI if this FROM line and those constants disagree, so a Dependabot bump
## here that forgets the constants cannot drift silently.
##
## Runner-version bump procedure:
##   1. Pick the new ghcr.io/actions/actions-runner tag (X.Y.Z).
##   2. Re-resolve the digest:
##        docker buildx imagetools inspect ghcr.io/actions/actions-runner:X.Y.Z
##      Use the top-level "Digest:" line (the OCI image index), not a
##      per-platform manifest.
##   3. Update this FROM line AND RunnerVersion + WorkerImageDigest in
##      cmd/agc/names/names.go to the same X.Y.Z@sha256:… (provisioner's
##      DefaultWorkerImage and the GMC's GITHUB_RUNNER_VERSION follow
##      automatically).
##   4. Update any hardcoded prior runner-version string in
##      cmd/agc/internal/agentpool tests so the registered runnerVersion matches.
FROM ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29
ENV PATH=/home/runner/bin:$PATH
# OpenContainers image labels carry provenance for SBOM scanners. REVISION and
# VERSION arrive as build args (wired from GIT_SHA in docker-bake.hcl); the
# defaults cover a bare `docker build` with no args. Declared in the final stage
# so changing them re-runs only the cheap label layer, not the build.
ARG REVISION="unknown"
ARG VERSION="dev"
LABEL org.opencontainers.image.source="https://github.com/actions-gateway/github-actions-gateway" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.title="worker" \
      org.opencontainers.image.description="Ephemeral GitHub Actions runner worker — wraps Runner.Worker with a job-payload entrypoint" \
      org.opencontainers.image.licenses="Apache-2.0"
# Bundle the license attribution this image redistributes for the wrapper binary
# we add on top of the runner base: the project's own Apache-2.0 LICENSE +
# NOTICE, and THIRD-PARTY-NOTICES (the aggregated license/notice texts of the
# statically linked vendored deps). /licenses is the Red Hat/OpenShift
# container-certification convention and pairs with the
# org.opencontainers.image.licenses label above. The upstream actions-runner base
# carries its own license files for its components; this covers only what we add.
# Regenerate THIRD-PARTY-NOTICES with `make third-party-notices`.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY --from=builder /wrapper /usr/local/bin/wrapper
# The actions-runner base already ends as USER runner (UID 1001); pin it
# explicitly so the non-root guarantee is local to this Dockerfile and survives
# a base-image change rather than depending on the upstream's final USER.
USER runner
ENTRYPOINT ["/usr/local/bin/wrapper"]
