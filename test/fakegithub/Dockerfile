## Stage 1: build the fakegithub binary from the workspace.
## Dependencies live in the workspace vendor/ at the repo root; `go build`
## auto-selects -mod=vendor when go.work and vendor/ are both present.
FROM golang:1.26 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/fakegithub ./test/fakegithub

## Stage 2: minimal distroless runtime image.
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/fakegithub /fakegithub
ENTRYPOINT ["/fakegithub"]
