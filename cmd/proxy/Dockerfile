# syntax=docker/dockerfile:1.7
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
# --platform=$BUILDPLATFORM: the builder always runs on the build host's native
# platform and CROSS-COMPILES for $TARGETARCH — no QEMU emulation of the Go
# toolchain on a multi-platform build.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:68cb6d68bed024785b69195b89af7ac7a444f27791435f98647edff595aa0479 AS builder
WORKDIR /src
# Build context is the repo root so the workspace vendor/ is available.
# `go build` auto-selects -mod=vendor when go.work and vendor/ are present.
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
    go build -trimpath -ldflags=-buildid= -o /bin/proxy ./cmd/proxy

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
      org.opencontainers.image.title="proxy" \
      org.opencontainers.image.description="Per-tenant egress proxy providing isolated egress IPs for GitHub traffic" \
      org.opencontainers.image.licenses="Apache-2.0"
# Bundle the license attribution this image redistributes: the project's own
# Apache-2.0 LICENSE + NOTICE, and THIRD-PARTY-NOTICES (the aggregated
# license/notice texts of the statically linked vendored deps). /licenses is the
# Red Hat/OpenShift container-certification convention and pairs with the
# org.opencontainers.image.licenses label above. Regenerate THIRD-PARTY-NOTICES
# with `make third-party-notices`. COPY needs no shell, so this works on distroless.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY --from=builder /bin/proxy /proxy
ENTRYPOINT ["/proxy"]
