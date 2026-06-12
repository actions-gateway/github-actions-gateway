# Q97 — Multi-arch (linux/amd64 + linux/arm64) published images

**Goal:** every published first-party image (`gmc`, `agc`, `proxy`, `worker`) is a
multi-arch OCI image index covering `linux/amd64` and `linux/arm64`, with the
existing signing/SBOM supply-chain story intact across both architectures.

**Status: ✅ implemented** (this doc records the design choices; the live publish
path is first proven on the next `v*` tag — see §Verification).

## Why multi-arch (not document-the-limitation)

All three pinned base images already cover arm64 — verified against the exact
digests in the Dockerfiles with `docker buildx imagetools inspect`:

- `golang:1.26@sha256:68cb6d…` — index incl. `linux/amd64`, `linux/arm64/v8`
- `gcr.io/distroless/static:nonroot@sha256:963fa6…` — index incl. both
- `ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b…` — index incl. both

The runtime stages are `COPY`/`ENV`/`LABEL` only (no `RUN`), so arm64 image
assembly needs **no QEMU emulation** anywhere: builder stages run on the build
host's native platform (`--platform=$BUILDPLATFORM`) and Go cross-compiles via
`GOARCH=$TARGETARCH`. Cost is roughly 2× Go compile time on the publish leg only.

## Changes

### 1. Dockerfiles — `TARGETOS`/`TARGETARCH` plumbing (all five)

Each builder stage becomes `FROM --platform=$BUILDPLATFORM golang:… AS builder`
and the `go build` gets `GOOS=$TARGETOS GOARCH=$TARGETARCH` (BuildKit populates
both automatically; a plain single-platform `docker build` keeps working and
targets the host platform). This also fixes the worker image for local arm64
(Apple Silicon kind) builds, where the hardcoded `GOARCH=amd64` produced an
amd64 wrapper binary on top of an arm64 base image.

`test/fakegithub` is plumbed too (not published, but built by the e2e bake on
developer machines that may be arm64).

`docker-bake.hcl` intentionally stays single-platform (host arch): e2e/kind runs
only consume the host platform, and a multi-arch bake would double e2e CI build
time for nothing.

### 2. `publish.yml` — platforms, recursive signing, per-arch SBOMs

- `platforms: linux/amd64,linux/arm64` on the build-push step. With multiple
  platforms, `steps.build.outputs.digest` becomes the **image index** digest —
  exactly what chart values / GMC digest pinning should reference (the kubelet
  resolves the per-arch manifest from the index at pull time). The run-summary
  digest step is unchanged and now records the index digest.
- **Signing:** `cosign sign --recursive` — signs the index *and* each per-arch
  child manifest. `cosign verify` on the index (what `make verify-release` and
  the operator runbook do) is unchanged; verifying a per-arch digest (e.g. the
  `imageID` a kubelet actually pulled) now also succeeds.
- **SBOM:** one SBOM **per architecture**, attested onto that architecture's
  manifest digest. A single syft scan of an index silently picks one platform —
  an arm64 operator auditing the attestation would get an amd64 package list.
  The per-arch digests are resolved from the index with
  `docker buildx imagetools inspect --format`; syft scans each per-arch ref;
  `cosign attest` binds each SBOM to its arch digest. SBOM files are still
  uploaded as run artifacts (one per arch).
  - *Trade-off:* `cosign verify-attestation` against the **index** digest finds
    nothing — attestations live on the per-arch manifests. The operator docs
    show how to resolve the per-arch digest first. The alternative (also
    attaching both SBOMs to the index) was rejected: two same-type attestations
    on one digest is ambiguous for policy engines, and the per-arch binding is
    the precise one.
  - `anchore/sbom-action` is replaced by `anchore/sbom-action/download-syft` +
    an explicit loop, since the action has no per-platform mode.

### 3. PR CI (`security-scan.yml`) — unchanged

PR-time image builds/scans/SBOMs stay amd64-only (`load: true` is
single-platform by design). The cross-compile path itself is exercised on PRs
because the Dockerfile builder stages now always go through the
`TARGETOS`/`TARGETARCH` arguments. An arm64 e2e leg is explicitly out of scope
(amd64 runners cannot run arm64 images).

### 4. Reproducibility

Unchanged per binary: `-trimpath -ldflags=-buildid=` keeps each arch's binary
bit-for-bit reproducible; Go cross-compilation is deterministic w.r.t. build
host. The *index* digest additionally depends on both child manifests (and
BuildKit's provenance attestation, as before) — the reproducibility claim
remains about the binaries/layers, not registry manifest bytes.

## Verification

- PR CI: all five images still build (amd64 leg of the same plumbing).
- Local: `docker buildx build --platform linux/arm64 …` spot-check of one image
  confirms cross-compile works without QEMU for the Go stage.
- **The multi-arch publish + recursive sign + per-arch attest path runs for the
  first time on the next `v*` tag** — same caveat the release runbook already
  carries for the signing path. Release step 3 (`make verify-release`) plus the
  arm64 spot-checks added to the runbook prove it then.

## Docs touched

- `docs/development/building.md` — multi-arch publish, local bake = host arch.
- `docs/operations/release.md` — what a release produces (index digest),
  per-arch SBOM attestation note in verification.
- `docs/operations/security-operations.md` — verify per-arch signatures;
  resolve arch digest before `verify-attestation`.
- `docs/operations/install.md` — supported platforms prerequisite line.
- `docs/design/05-security.md` — supply-chain row: recursive signing, per-arch
  SBOM attestations.
