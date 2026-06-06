# syntax=docker/dockerfile:1.7
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
FROM golang:1.26@sha256:68cb6d68bed024785b69195b89af7ac7a444f27791435f98647edff595aa0479 AS builder
WORKDIR /src
# Dependencies live in the workspace vendor/ at the repo root; `go build`
# auto-selects -mod=vendor when go.work and vendor/ are both present.
COPY . .
# Cache mount holds Go's compiled-package cache (`.a` files keyed by content
# hash). Survives across layer-cache misses so incremental source edits only
# recompile changed packages instead of the whole dep tree.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -C cmd/gmc -o /bin/manager ./cmd/main.go

# gcr.io/distroless/static:nonroot — pinned to the multi-arch manifest digest.
# Update with: docker buildx imagetools inspect gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=builder /bin/manager /manager
ENTRYPOINT ["/manager"]
