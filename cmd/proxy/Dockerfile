FROM golang:1.26 AS builder
WORKDIR /src
# Build context is the repo root so the workspace vendor/ is available.
# `go build` auto-selects -mod=vendor when go.work and vendor/ are present.
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/proxy ./cmd/proxy

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/proxy /proxy
ENTRYPOINT ["/proxy"]
