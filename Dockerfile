# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# ^ BuildKit frontend pinned by digest. Dependabot's docker ecosystem does not
#   bump syntax directives, so re-pin manually when bumping the 1.7 tag:
#     docker buildx imagetools inspect docker/dockerfile:1.7
#
# ONE Dockerfile for every first-party image. Each image is a named final stage
# selected with `--target` (docker-bake.hcl for the e2e bake; the publish.yml
# and security-scan.yml matrices for release and scanning):
#
#   gmc  agc  proxy  worker  wrapper  build-runner  fakegithub
#
# Why one file rather than six (the layout before this change): all six images
# compile Go against the same workspace vendor/ tree, and BuildKit can only
# share a build step between targets when it is *the same stage in the same
# Dockerfile*. With six files the six builds each compiled the shared dependency
# closure from scratch, concurrently, on a 4-vCPU CI runner — the e2e bake's
# whole critical path (~205 s; the two largest legs alone were 170 s and 177 s).
# Collapsed here into a single `deps` stage they compile once and every binary
# builds from the result in seconds. See docs/plan/e2e-ci-speed-round-2.md.
#
# The stage graph:
#
#   deps   golang + workspace manifests + vendor/ → warm Go build cache
#     └─ src    + the first-party source
#          ├─ build-gmc / build-agc / build-proxy / build-wrapper / build-fakegithub
#          └─ final stages COPY just the binary onto a runtime base
#
# build-runner sits outside that graph: it is `worker` plus a Docker client
# COPY'd from a pinned docker:<N>-cli, so it compiles nothing of its own.

########################  deps — the shared compile cache  ####################
# golang:1.26 — pinned to the multi-arch manifest digest for cache stability.
# Update with: docker buildx imagetools inspect golang:1.26
# --platform=$BUILDPLATFORM: the builder always runs on the build host's native
# platform and CROSS-COMPILES for $TARGETARCH — no QEMU emulation of the Go
# toolchain on a multi-platform build.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:26326682769ca980f8f1d3b1f52be2dd1c1d25270e3de3fe0c97d6bb65df3556 AS deps
WORKDIR /src

# TARGETOS/TARGETARCH are populated by BuildKit from the requested target
# platform (the host platform for a plain single-platform build).
ARG TARGETOS TARGETARCH

# GOCACHE is a REAL DIRECTORY IN THE LAYER, not a --mount=type=cache. That is
# the whole point of this stage: a cache mount lives in BuildKit's own state,
# which `cache-to type=gha` does not export and which CI never has, because
# docker/setup-buildx-action boots a fresh builder on every run. A plain
# directory is part of the layer, so a layer cache carries it between runs and
# this stage is a HIT on any run that did not change vendor/, wherever such a
# cache is actually wired up. CI has none: the e2e bake's type=gha cache is
# inert from a `run:` step (docker-bake.hcl's GHA_CACHE comment, Q931), so this
# stage recompiles every run. Dependencies live in the workspace vendor/ at
# the repo root; `go build` auto-selects -mod=vendor when go.work and vendor/
# are both present.
ENV GOCACHE=/gocache \
    CGO_ENABLED=0 \
    GOOS=$TARGETOS \
    GOARCH=$TARGETARCH

# Copy ONLY the workspace manifests and the vendor tree — never first-party
# source. That is what keeps this (expensive) layer keyed on the dependency set
# alone: editing any .go file outside vendor/ leaves it untouched and the build
# below is skipped entirely. go.work names all ten modules, and each `use`
# directory must carry its go.mod for the workspace to load.
COPY go.work go.work.sum ./
COPY api/go.mod api/
COPY broker/go.mod broker/
COPY cmd/agc/go.mod cmd/agc/
COPY cmd/gmc/go.mod cmd/gmc/
COPY cmd/probe/go.mod cmd/probe/
COPY cmd/proxy/go.mod cmd/proxy/
COPY cmd/worker/go.mod cmd/worker/
COPY githubapp/go.mod githubapp/
COPY scaleset/go.mod scaleset/
COPY test/fakegithub/go.mod test/fakegithub/
COPY vendor/ vendor/

# Compile every vendored package so the per-binary builds below hit the cache
# instead of the compiler.
#
# -trimpath MUST match the flag the binary builds use. It is an input to every
# package's build-cache key, so warming without it populates entries the real
# builds can never hit — the warm-up appears to work and saves nothing (measured:
# gmc took 92 s CPU against a non-trimpath cache, 3.4 s against a matching one).
# For the same reason every build stage below passes -trimpath, including
# fakegithub, which is a test image but shares this cache.
RUN <<'EOF'
set -eu
# vendor/modules.txt lists one import path per line; module/marker lines start
# with '#' and the "## explicit" markers carry a second field.
awk '!/^#/ && NF==1' vendor/modules.txt > /tmp/vendored.txt
# Drop packages whose build constraints exclude every file on this platform
# (golang.org/x/sys/windows, .../plan9, …). `go build` fails fast on those and
# would abort the whole warm-up; `go list -e` reports them as errored instead.
xargs go list -e -f '{{if and (not .Error) .GoFiles}}{{.ImportPath}}{{end}}' \
    < /tmp/vendored.txt > /tmp/buildable.txt
xargs go build -trimpath < /tmp/buildable.txt
EOF

########################  src — deps plus first-party source  #################
# Split from `deps` so a source-only edit invalidates this cheap layer and not
# the compile cache above.
FROM deps AS src
COPY . .

########################  per-binary build stages  ############################
# VERSION is the release tag (or GIT_SHA fallback) wired from docker-bake.hcl /
# publish.yml; it is stamped into main.version via -ldflags -X so the binary
# emits actions_gateway_build_info{version=…} (Q318). Defaults to "dev" for a
# bare `docker build`, matching the un-stamped local-build placeholder.
#
# -trimpath strips absolute filesystem paths; -buildid= clears the build ID —
# together they make the binaries bit-for-bit reproducible (a reproducible-build
# input); the -X main.version stamp is a deterministic function of the release
# tag, so it preserves reproducibility. At publish time publish.yml emits a
# signed, Rekor-logged build-provenance attestation for each image (SLSA Build L2
# via GitHub Actions OIDC + Sigstore); reaching SLSA Build L3 would additionally
# require an isolated reusable-workflow builder, not yet adopted.

FROM src AS build-gmc
ARG VERSION="dev"
RUN go build -C cmd/gmc -trimpath -ldflags="-buildid= -X main.version=${VERSION}" -o /bin/manager ./cmd

FROM src AS build-agc
ARG VERSION="dev"
RUN go build -C cmd/agc -trimpath -ldflags="-buildid= -X main.version=${VERSION}" -o /bin/agc .

FROM src AS build-proxy
ARG VERSION="dev"
RUN go build -trimpath -ldflags="-buildid= -X main.version=${VERSION}" -o /bin/proxy ./cmd/proxy

# ONE wrapper build feeding TWO images: the full runner image (`worker`) and the
# scratch injection image (`wrapper`). These were separate Dockerfiles running
# the identical `go build ./cmd/worker`, which BuildKit could not deduplicate —
# the compile ran twice on every bake.
FROM src AS build-wrapper
RUN go build -trimpath -ldflags=-buildid= -o /wrapper ./cmd/worker

FROM src AS build-fakegithub
RUN go build -trimpath -o /bin/fakegithub ./test/fakegithub

########################  gmc  ################################################
# gcr.io/distroless/static:nonroot — pinned to the multi-arch manifest digest.
# Update with: docker buildx imagetools inspect gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478 AS gmc
# OpenContainers image labels carry provenance for SBOM scanners. REVISION and
# VERSION arrive as build args (wired from GIT_SHA in docker-bake.hcl); the
# defaults cover a bare `docker build` with no args. Declared in the final stage
# so changing them re-runs only the cheap label layer, not the build.
ARG REVISION="unknown"
ARG VERSION="dev"
LABEL org.opencontainers.image.source="https://github.com/actions-gateway/github-actions-gateway" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.title="gmc" \
      org.opencontainers.image.description="Gateway Manager Controller — provisions per-tenant Actions Gateway Controller instances from an ActionsGateway CR" \
      org.opencontainers.image.licenses="Apache-2.0"
# Bundle the license attribution this image redistributes: the project's own
# Apache-2.0 LICENSE + NOTICE, and THIRD-PARTY-NOTICES (the aggregated
# license/notice texts of the statically linked vendored deps). /licenses is the
# Red Hat/OpenShift container-certification convention and pairs with the
# org.opencontainers.image.licenses label above. Regenerate THIRD-PARTY-NOTICES
# with `make third-party-notices`. COPY needs no shell, so this works on distroless.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY --from=build-gmc /bin/manager /manager
# distroless :nonroot already runs as UID 65532; pin it explicitly so the
# non-root guarantee survives a base-image/tag change instead of living only in
# the FROM tag.
USER 65532:65532
ENTRYPOINT ["/manager"]

########################  agc  ################################################
FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478 AS agc
ARG REVISION="unknown"
ARG VERSION="dev"
LABEL org.opencontainers.image.source="https://github.com/actions-gateway/github-actions-gateway" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.title="agc" \
      org.opencontainers.image.description="Actions Gateway Controller — multiplexes virtual runner sessions and provisions ephemeral worker pods" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY --from=build-agc /bin/agc /agc
USER 65532:65532
ENTRYPOINT ["/agc"]

########################  proxy  ##############################################
FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478 AS proxy
ARG REVISION="unknown"
ARG VERSION="dev"
LABEL org.opencontainers.image.source="https://github.com/actions-gateway/github-actions-gateway" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.title="proxy" \
      org.opencontainers.image.description="Per-tenant egress proxy providing isolated egress IPs for GitHub traffic" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY --from=build-proxy /bin/proxy /proxy
USER 65532:65532
ENTRYPOINT ["/proxy"]

########################  worker  #############################################
## The runner image with the wrapper as ENTRYPOINT.
## Built on top of ghcr.io/actions/actions-runner (the ARC gha-runner-scale-set
## default) so tenants copy-pasting from ARC manifests see the same base. The
## image runs as USER runner (UID 1001) and ships Runner.Worker at
## /home/runner/bin/Runner.Worker. That directory is NOT on the image's default
## PATH, so prepend it here — the wrapper resolves the binary via
## exec.LookPath("Runner.Worker").
##
## Pinned to the multi-arch manifest-index digest for supply-chain integrity
## (M-19 in docs/plan/security.md). The version MUST match the pinned runner
## version in cmd/agc/names (RunnerVersion / WorkerImageDigest) — the single
## source of truth that drives both the AGC's DefaultWorkerImage and the
## GITHUB_RUNNER_VERSION the GMC injects (the agent.version GitHub validates at
## session creation). The lockstep test cmd/agc/names/runner_version_test.go
## fails CI if this FROM line and those constants disagree, so a Dependabot bump
## here that forgets the constants cannot drift silently.
##
## Runner-version bump procedure:
##   1. Pick the new ghcr.io/actions/actions-runner tag (X.Y.Z).
##   2. Re-resolve the digest:
##        docker buildx imagetools inspect ghcr.io/actions/actions-runner:X.Y.Z
##      Use the top-level "Digest:" line (the OCI image index), not a
##      per-platform manifest.
##   3. Update this FROM line AND RunnerVersion + WorkerImageDigest in
##      cmd/agc/names/names.go to the same X.Y.Z@sha256:… (provisioner's
##      DefaultWorkerImage and the GMC's GITHUB_RUNNER_VERSION follow
##      automatically).
##   4. Update any hardcoded prior runner-version string in
##      cmd/agc/internal/agentpool tests so the registered runnerVersion matches.
FROM ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29 AS worker
ENV PATH=/home/runner/bin:$PATH
ARG REVISION="unknown"
ARG VERSION="dev"
LABEL org.opencontainers.image.source="https://github.com/actions-gateway/github-actions-gateway" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.title="worker" \
      org.opencontainers.image.description="Ephemeral GitHub Actions runner worker — wraps Runner.Worker with a job-payload entrypoint" \
      org.opencontainers.image.licenses="Apache-2.0"
# Bundle the license attribution this image redistributes for the wrapper binary
# we add on top of the runner base. The upstream actions-runner base carries its
# own license files for its components; this covers only what we add.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY --from=build-wrapper /wrapper /usr/local/bin/wrapper
# The actions-runner base already ends as USER runner (UID 1001); pin it
# explicitly so the non-root guarantee is local to this Dockerfile and survives
# a base-image change rather than depending on the upstream's final USER.
# The name, not a numeric UID, is deliberate: the AGC gap-fills runAsUser 1001
# whenever it enforces runAsNonRoot, which is what lets kubelet verify non-root
# here and on any tenant image built from the same base (docs/design/05-security.md
# §5.3, Q115). DL3066 (new in hadolint 2.15.0) wants the number instead.
# hadolint ignore=DL3066
USER runner
ENTRYPOINT ["/usr/local/bin/wrapper"]

########################  wrapper  ############################################
## The ~2 MB scratch image holding just the cmd/worker wrapper binary (Q235).
## The AGC provisioner injects it into every worker pod at runtime — as a
## read-only OCI image volume (K8s >=1.33) or copied in by an initContainer
## (`wrapper install <dir>`) — so the runner container can be the unmodified
## upstream actions-runner (or any tenant workerImage) instead of a baked-in
## wrapper image. Shares build-wrapper with the `worker` image above, so the two
## ship byte-identical wrappers from a single compile.
FROM scratch AS wrapper
ARG REVISION="unknown"
ARG VERSION="dev"
LABEL org.opencontainers.image.source="https://github.com/actions-gateway/github-actions-gateway" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.title="wrapper" \
      org.opencontainers.image.description="GAG worker-wrapper binary, injected into worker pods (Q235)" \
      org.opencontainers.image.licenses="Apache-2.0"
# License attribution for the statically linked vendored deps (no OS layer to
# carry its own).
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
# The wrapper is at /wrapper so the image volume exposes it at <mount>/wrapper
# and the initContainer runs `/wrapper install <mount>`. No shell, no OS —
# nothing to pull but the binary.
COPY --from=build-wrapper /wrapper /wrapper
ENTRYPOINT ["/wrapper"]

########################  build-runner  #######################################
## The `worker` image with its Docker tooling replaced by a pinned, current set
## (Q740).
##
## READ THIS BEFORE ASSUMING WHAT IT IS FOR. ghcr.io/actions/actions-runner
## installs the docker static bundle into /usr/bin and copies a buildx plugin to
## /usr/local/lib/docker/cli-plugins — measured from the base image's own build
## history and from this repo's published worker SBOM, on every tag checked
## (2.328.0, 2.331.0, 2.335.1, latest). A Docker-in-Docker job on the stock runner
## therefore does not fail for want of a client, and the runner template library's
## DinD entries set no workerImage at all.
##
## What this image adds is narrower:
##   - docker compose, which the runner base ships in no form;
##   - a CLI and buildx pinned and bumped here, rather than whichever versions the
##     runner release vendored (base 29.4.3 / buildx 0.34.1 against 29.7.2 /
##     0.36.1 at this pin).
## ~58 MB compressed.
##
## THE PLUGIN DESTINATION IS LOAD-BEARING. docker/cli searches its plugin dirs in
## order and /usr/local/lib/docker/cli-plugins precedes /usr/local/libexec/...
## (defaultSystemPluginDirs, cli-plugins/manager/manager_unix.go). Writing there
## overwrites the base's plugin, leaving exactly one buildx; writing to libexec
## instead leaves the base's older one winning and the pinned buildx never
## executing. The CLI needs no such care: the base's PATH puts /usr/local/bin
## ahead of /usr/bin.
##
## COPY-only, deliberately: those binaries are STATICALLY LINKED (measured on the
## 29.7.2 x86_64 static release, `ELF 64-bit ... statically linked`), so they run
## on this Ubuntu-based base despite being built against Alpine, and the arm64 leg
## needs no QEMU. A `RUN apt-get install docker-ce-cli` would emulate instead, and
## would unpin the version. Pinned to the multi-arch index digest like every other
## base here; nothing bumps it automatically (see Q976), so re-pin by hand with:
##   docker buildx imagetools inspect docker:29-cli
## Re-check the source plugin paths when bumping the major: they are set by
## docker-library/docker's own Dockerfile, not by the CLI.
FROM docker:29-cli@sha256:000bb62ff495f986c9f5578eb67cc2cb98b91138eda81d7762d5371eb8a497fe AS docker-cli

## Everything else — ENTRYPOINT, PATH, USER runner, the pinned runner version and
## the lockstep test over it — is inherited from `worker`, so this image runs
## wherever that one does and the two can never disagree about the runner.
FROM worker AS build-runner
ARG REVISION="unknown"
ARG VERSION="dev"
## Re-stated, not inherited: LABEL values carry over from the base stage, so
## without this the image would advertise itself as `worker`.
LABEL org.opencontainers.image.source="https://github.com/actions-gateway/github-actions-gateway" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.title="build-runner" \
      org.opencontainers.image.description="GitHub Actions runner with a pinned Docker CLI, buildx and compose" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/ /usr/local/lib/docker/cli-plugins/
## Attribution for those three binaries. A `COPY --from` takes only the paths it
## names, so whatever docker:<N>-cli carried does not travel with them and this
## image is the redistributor. Hand-maintained: THIRD-PARTY-NOTICES is generated
## from vendor/ and covers only the Go modules linked into our own binaries.
COPY THIRD-PARTY-NOTICES-DOCKER /licenses/

########################  fakegithub  #########################################
# TEST-ONLY image: a fake GitHub API server used by the e2e suite, never
# published. It intentionally omits the OCI provenance labels and /licenses
# bundle that the production stages above carry — do NOT copy this as a template
# for a shipped image.
FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478 AS fakegithub
COPY --from=build-fakegithub /bin/fakegithub /fakegithub
USER 65532:65532
ENTRYPOINT ["/fakegithub"]
