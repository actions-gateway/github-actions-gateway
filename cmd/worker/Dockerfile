# syntax=docker/dockerfile:1.7
## Stage 1: build the Go entrypoint wrapper.
## This produces a static binary that reads the job payload Secret
## and feeds it into Runner.Worker via anonymous pipes.
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
FROM golang:1.26@sha256:68cb6d68bed024785b69195b89af7ac7a444f27791435f98647edff595aa0479 AS builder
WORKDIR /src
# Build context is the repo root so the workspace vendor/ is available.
# `go build` auto-selects -mod=vendor when go.work and vendor/ are present.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o /wrapper ./cmd/worker

## Stage 2: runner image with the wrapper as ENTRYPOINT.
## Built on top of ghcr.io/actions/actions-runner (the ARC gha-runner-scale-set
## default) so tenants copy-pasting from ARC manifests see the same base. The
## image runs as USER runner (UID 1001) and ships Runner.Worker at
## /home/runner/bin/Runner.Worker. That directory is NOT on the image's default
## PATH, so prepend it here — the wrapper resolves the binary via
## exec.LookPath("Runner.Worker").
##
## Pinned to the multi-arch manifest-index digest for supply-chain integrity
## (M-19 in docs/plan/security.md). The version must match the runner version
## used during agent registration (DefaultWorkerImage in
## cmd/agc/internal/provisioner/provisioner.go and the runnerVersion stored in
## agent Secrets by cmd/agc/internal/agentpool) so GitHub does not reject
## POST /sessions with a version-too-old error.
##
## Runner-version bump procedure:
##   1. Pick the new ghcr.io/actions/actions-runner tag (X.Y.Z).
##   2. Re-resolve the digest:
##        docker buildx imagetools inspect ghcr.io/actions/actions-runner:X.Y.Z
##      Use the top-level "Digest:" line (the OCI image index), not a
##      per-platform manifest.
##   3. Update this FROM line AND DefaultWorkerImage in
##      cmd/agc/internal/provisioner/provisioner.go to the same X.Y.Z@sha256:….
##   4. Update the "2.327.1" string in cmd/agc/internal/agentpool tests so the
##      registered runnerVersion matches what GitHub expects.
FROM ghcr.io/actions/actions-runner:2.327.1@sha256:551dc313e6b6ef1ca7b9594d8090a7a6cc7aeb663f1079ba2fec07e9158f3259
ENV PATH=/home/runner/bin:$PATH
COPY --from=builder /wrapper /usr/local/bin/wrapper
ENTRYPOINT ["/usr/local/bin/wrapper"]
