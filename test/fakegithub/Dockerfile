FROM golang:1.26 AS builder
WORKDIR /src
# Copy workspace and root module files first for layer caching.
COPY go.work go.work.sum go.mod go.sum ./
# Copy all module go.mod/go.sum files so the workspace resolver can find them.
COPY cmd/agc/go.mod cmd/agc/go.sum cmd/agc/
COPY cmd/gmc/go.mod cmd/gmc/go.sum cmd/gmc/
COPY cmd/proxy/go.mod cmd/proxy/go.sum cmd/proxy/
COPY cmd/worker/go.mod cmd/worker/go.sum cmd/worker/
RUN go work sync 2>/dev/null || true
# Copy source
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/fakegithub ./test/fakegithub

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/fakegithub /fakegithub
ENTRYPOINT ["/fakegithub"]
