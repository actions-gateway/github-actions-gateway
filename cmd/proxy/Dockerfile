FROM golang:1.26 AS builder
WORKDIR /src
# Module manifests first so go mod download lives in its own layer and is
# reused across PRs that don't touch dependencies.
COPY go.mod go.sum ./
RUN go mod download
# Source — invalidates only this layer and the build below, not the download.
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/proxy .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/proxy /proxy
ENTRYPOINT ["/proxy"]
