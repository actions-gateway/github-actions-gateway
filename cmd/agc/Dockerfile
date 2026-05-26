# syntax=docker/dockerfile:1.7
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
FROM golang:1.26@sha256:2d6c80227255c3112a4d08e67ba98e58efd3846daf15d9d7d4c389565d881b1a AS builder
WORKDIR /src
# Dependencies live in the workspace vendor/ at the repo root; `go build`
# auto-selects -mod=vendor when go.work and vendor/ are both present.
COPY . .
# Cache mount holds Go's compiled-package cache (`.a` files keyed by content
# hash). Survives across layer-cache misses so incremental source edits only
# recompile changed packages instead of the whole dep tree.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -C cmd/agc -o /bin/agc .

# gcr.io/distroless/static:nonroot — pinned to the multi-arch manifest digest.
# Update with: docker buildx imagetools inspect gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=builder /bin/agc /agc
ENTRYPOINT ["/agc"]
