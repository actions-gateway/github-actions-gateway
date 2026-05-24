# syntax=docker/dockerfile:1.7
FROM golang:1.26 AS builder
WORKDIR /src
# Dependencies live in the workspace vendor/ at the repo root; `go build`
# auto-selects -mod=vendor when go.work and vendor/ are both present.
COPY . .
# Cache mount holds Go's compiled-package cache (`.a` files keyed by content
# hash). Survives across layer-cache misses so incremental source edits only
# recompile changed packages instead of the whole dep tree.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -C cmd/agc -o /bin/agc .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/agc /agc
ENTRYPOINT ["/agc"]
