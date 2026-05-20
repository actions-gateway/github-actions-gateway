## Stage 1: build the Go entrypoint wrapper.
## This produces a static binary that reads the job payload Secret
## and feeds it into Runner.Worker via named pipes.
FROM golang:1.24-alpine AS builder
WORKDIR /src
# Copy the workspace-level go files so the build can resolve shared modules.
COPY go.work go.work.sum ./
COPY go.mod go.sum ./
COPY broker/ broker/
COPY githubapp/ githubapp/
COPY cmd/worker/ cmd/worker/
RUN cd cmd/worker && go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o /wrapper ./cmd/worker

## Stage 2: runner image with the wrapper as ENTRYPOINT.
## The official actions/runner image includes Runner.Worker at
## /home/runner/externals/node20/bin/ and the runner binaries at /home/runner/.
##
## TODO(validation): Pin to an immutable digest in production.
## The version here must match the runner version used during agent registration
## (cmd/agc/internal/agentpool) so GitHub does not reject POST /sessions with
## a version-too-old error.
FROM ghcr.io/actions/runner:2.327.1
COPY --from=builder /wrapper /usr/local/bin/wrapper
ENTRYPOINT ["/usr/local/bin/wrapper"]
