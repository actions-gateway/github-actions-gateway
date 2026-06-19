# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# ^ BuildKit frontend pinned by digest. Dependabot's docker ecosystem does not
#   bump syntax directives, so re-pin manually when bumping the 1.7 tag:
#     docker buildx imagetools inspect docker/dockerfile:1.7
#
# TEST-ONLY image: a fake GitHub API server used by the e2e suite, never
# published. It intentionally omits the OCI provenance labels, reproducible-build
# flags, and /licenses bundle that the production Dockerfiles (cmd/{gmc,agc,proxy,
# worker}) carry — do NOT copy this as a template for a shipped image.
## Stage 1: build the fakegithub binary from the workspace.
## Dependencies live in the workspace vendor/ at the repo root; `go build`
## auto-selects -mod=vendor when go.work and vendor/ are both present.
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
# --platform=$BUILDPLATFORM: the builder always runs on the build host's native
# platform and CROSS-COMPILES for $TARGETARCH — no QEMU emulation of the Go
# toolchain on a multi-platform build.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:792443b89f65105abba56b9bd5e97f680a80074ac62fc844a584212f8c8102c3 AS builder
WORKDIR /src
COPY . .
# TARGETOS/TARGETARCH are populated by BuildKit from the requested target
# platform (the host platform for a plain single-platform build).
ARG TARGETOS TARGETARCH
# Cache mount holds Go's compiled-package cache (`.a` files keyed by content
# hash). Survives across layer-cache misses so incremental source edits only
# recompile changed packages instead of the whole dep tree.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -o /bin/fakegithub ./test/fakegithub

## Stage 2: minimal distroless runtime image.
# gcr.io/distroless/static:nonroot — pinned to the multi-arch manifest digest.
# Update with: docker buildx imagetools inspect gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=builder /bin/fakegithub /fakegithub
# distroless :nonroot already runs as UID 65532; pin it explicitly so the
# non-root guarantee survives a base-image/tag change instead of living only in
# the FROM tag.
USER 65532:65532
ENTRYPOINT ["/fakegithub"]
