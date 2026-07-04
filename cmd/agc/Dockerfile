# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# ^ BuildKit frontend pinned by digest. Dependabot's docker ecosystem does not
#   bump syntax directives, so re-pin manually when bumping the 1.7 tag:
#     docker buildx imagetools inspect docker/dockerfile:1.7
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
# --platform=$BUILDPLATFORM: the builder always runs on the build host's native
# platform and CROSS-COMPILES for $TARGETARCH — no QEMU emulation of the Go
# toolchain on a multi-platform build.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609 AS builder
WORKDIR /src
# Dependencies live in the workspace vendor/ at the repo root; `go build`
# auto-selects -mod=vendor when go.work and vendor/ are both present.
COPY . .
# TARGETOS/TARGETARCH are populated by BuildKit from the requested target
# platform (the host platform for a plain single-platform build).
ARG TARGETOS TARGETARCH
# Cache mount holds Go's compiled-package cache (`.a` files keyed by content
# hash). Survives across layer-cache misses so incremental source edits only
# recompile changed packages instead of the whole dep tree.
# -trimpath strips absolute filesystem paths; -ldflags=-buildid= clears the
# build ID — together they make the binary bit-for-bit reproducible (a
# reproducible-build input). At publish time publish.yml emits a signed,
# Rekor-logged build-provenance attestation for the image (SLSA Build L2 via
# GitHub Actions OIDC + Sigstore); reaching SLSA Build L3 would additionally
# require an isolated reusable-workflow builder, not yet adopted.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -C cmd/agc -trimpath -ldflags=-buildid= -o /bin/agc .

# gcr.io/distroless/static:nonroot — pinned to the multi-arch manifest digest.
# Update with: docker buildx imagetools inspect gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
# OpenContainers image labels carry provenance for SBOM scanners. REVISION and
# VERSION arrive as build args (wired from GIT_SHA in docker-bake.hcl); the
# defaults cover a bare `docker build` with no args. Declared in the final stage
# so changing them re-runs only the cheap label layer, not the build.
ARG REVISION="unknown"
ARG VERSION="dev"
LABEL org.opencontainers.image.source="https://github.com/actions-gateway/github-actions-gateway" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.title="agc" \
      org.opencontainers.image.description="Actions Gateway Controller — multiplexes virtual runner sessions and provisions ephemeral worker pods" \
      org.opencontainers.image.licenses="Apache-2.0"
# Bundle the license attribution this image redistributes: the project's own
# Apache-2.0 LICENSE + NOTICE, and THIRD-PARTY-NOTICES (the aggregated
# license/notice texts of the statically linked vendored deps). /licenses is the
# Red Hat/OpenShift container-certification convention and pairs with the
# org.opencontainers.image.licenses label above. Regenerate THIRD-PARTY-NOTICES
# with `make third-party-notices`. COPY needs no shell, so this works on distroless.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY --from=builder /bin/agc /agc
# distroless :nonroot already runs as UID 65532; pin it explicitly so the
# non-root guarantee survives a base-image/tag change instead of living only in
# the FROM tag.
USER 65532:65532
ENTRYPOINT ["/agc"]
