# Agent reference: Building

All binaries are built into `.build/` at the repo root (gitignored). Use the root `Makefile`:

```bash
make build        # build all binaries → .build/agc, .build/gmc, .build/probe, .build/proxy
make build-agc    # build only the AGC controller
make build-gmc    # build only the GMC controller
make build-probe  # build only the probe
make build-proxy  # build only the egress proxy
```

`cmd/worker` is a workspace module but has no dedicated root-level build target — it is built into its container image only.

Individual module Makefiles (e.g. `cmd/agc/Makefile`) also output to `.build/` via a relative path (`../../.build/`), so both `make` invocations land in the same place.

## Container images

The four production images (`cmd/{agc,gmc,proxy,worker}/Dockerfile`) are built
together via [`docker-bake.hcl`](../../docker-bake.hcl) (`docker buildx bake`);
the e2e/CI image pipeline is described in
[docker-image-speed.md](../plan/docker-image-speed.md).

### Multi-arch (linux/amd64 + linux/arm64)

Published images are **multi-arch** (Q97): the release pipeline
([`publish.yml`](../../.github/workflows/publish.yml)) passes
`platforms: linux/amd64,linux/arm64`, producing an OCI image index whose digest
is what operators pin. All five Dockerfiles (the four production images plus
`test/fakegithub`) plumb the target platform the same way:

- The Go **builder stage** runs `FROM --platform=$BUILDPLATFORM` — always the
  build host's native platform — and **cross-compiles** with
  `GOOS=$TARGETOS GOARCH=$TARGETARCH`. BuildKit populates both args per target
  platform, so no QEMU emulation is needed for the build.
- The **runtime stages** contain no `RUN` (only `COPY`/`ENV`/`LABEL`), so
  assembling the foreign-arch image needs no emulation either. All pinned base
  digests (`golang`, `distroless/static`, `actions-runner`) are multi-arch
  index digests covering both platforms.

Local builds (`docker buildx bake`, plain `docker build`) stay
**single-platform, targeting the host arch** — on an Apple Silicon machine the
bake produces arm64 images that run natively in a local kind cluster. To
cross-build one image explicitly:

```bash
docker buildx build --platform linux/arm64 -f cmd/agc/Dockerfile .
```

Cross-compilation does not affect reproducibility: `-trimpath
-ldflags=-buildid=` keeps each architecture's binary bit-for-bit reproducible
regardless of the build host. (The *index* digest depends on both child
manifests plus BuildKit's provenance attestation — the reproducibility claim is
about the binaries and layers, not registry manifest bytes.)

### License attribution (`/licenses/`)

Each production image `COPY`s three license files into `/licenses/` — the
Red Hat/OpenShift container-certification convention, paired with the
`org.opencontainers.image.licenses="Apache-2.0"` label every image carries:

- `LICENSE` / `NOTICE` — the project's own Apache-2.0 license and notice.
- `THIRD-PARTY-NOTICES` — the aggregated license/notice texts of the third-party
  Go modules statically linked into the binary. This satisfies the
  reproduce-the-notice clauses those licenses impose on a redistribution, and a
  container image is a redistribution (Apache-2.0 §4(d), MIT/BSD).

`THIRD-PARTY-NOTICES` is a **generated, committed** file at the repo root. It is
assembled from the committed, version-pinned `vendor/` tree (offline — no network
or module cache) by [`scripts/gen-third-party-notices.sh`](../../scripts/gen-third-party-notices.sh):

```bash
make third-party-notices         # regenerate THIRD-PARTY-NOTICES from vendor/
make third-party-notices-check   # fail if it is stale (the CI drift gate)
```

**Regenerate it whenever the dependency set changes** (a module added, removed,
or bumped — i.e. any `go mod vendor` that touches `vendor/`) and commit the
result. The `license-notices` CI workflow runs `make third-party-notices-check`
on every `vendor/**` change and fails the PR if the committed file is stale, so
the image build always bundles current attribution. The operator-facing view of
what ships at `/licenses/` is in
[security-operations.md](../operations/security-operations.md#license-attribution-in-images).

The test-only `test/fakegithub` image is not published as a product artifact and
does not carry `/licenses/`.
