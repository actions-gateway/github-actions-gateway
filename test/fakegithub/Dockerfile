# syntax=docker/dockerfile:1.7
## Stage 1: build the fakegithub binary from the workspace.
## Dependencies live in the workspace vendor/ at the repo root; `go build`
## auto-selects -mod=vendor when go.work and vendor/ are both present.
FROM golang:1.26 AS builder
WORKDIR /src
COPY . .
# Cache mount holds Go's compiled-package cache (`.a` files keyed by content
# hash). Survives across layer-cache misses so incremental source edits only
# recompile changed packages instead of the whole dep tree.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /bin/fakegithub ./test/fakegithub

## Stage 2: minimal distroless runtime image.
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/fakegithub /fakegithub
ENTRYPOINT ["/fakegithub"]
