FROM golang:1.26 AS builder
WORKDIR /src
# Copy workspace and module manifests for dependency layer caching.
# cmd/worker and cmd/probe have no go.sum (no external deps).
COPY go.work go.work.sum go.mod go.sum ./
COPY cmd/agc/go.mod cmd/agc/go.sum cmd/agc/
COPY cmd/gmc/go.mod cmd/gmc/go.sum cmd/gmc/
COPY cmd/proxy/go.mod cmd/proxy/go.sum cmd/proxy/
COPY cmd/worker/go.mod cmd/worker/
COPY cmd/probe/go.mod cmd/probe/
# Predownload modules in their own layer so source changes don't re-trigger
# the download. Layer key depends only on the *.mod / *.sum files above.
RUN cd cmd/gmc && go mod download
# Copy source
COPY . .
RUN CGO_ENABLED=0 go build -C cmd/gmc -o /bin/manager ./cmd/main.go

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/manager /manager
ENTRYPOINT ["/manager"]
