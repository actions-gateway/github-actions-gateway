## Stage 1: build the Go entrypoint wrapper.
## This produces a static binary that reads the job payload Secret
## and feeds it into Runner.Worker via named pipes.
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
FROM golang:1.26@sha256:2d6c80227255c3112a4d08e67ba98e58efd3846daf15d9d7d4c389565d881b1a AS builder
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
## Built on top of ghcr.io/actions/actions-runner (the ARC gha-runner-scale-set
## default) so tenants copy-pasting from ARC manifests see the same base. The
## image runs as USER runner (UID 1001) and ships Runner.Worker at
## /home/runner/bin/Runner.Worker. That directory is NOT on the image's default
## PATH, so prepend it here — the wrapper resolves the binary via
## exec.LookPath("Runner.Worker").
##
## Pin this to a digest once GHCR auth is available:
##   docker buildx imagetools inspect ghcr.io/actions/actions-runner:2.327.1
## The version must match the runner version used during agent registration
## (cmd/agc/internal/agentpool) so GitHub does not reject POST /sessions with
## a version-too-old error.
FROM ghcr.io/actions/actions-runner:2.327.1
ENV PATH=/home/runner/bin:$PATH
COPY --from=builder /wrapper /usr/local/bin/wrapper
ENTRYPOINT ["/usr/local/bin/wrapper"]
