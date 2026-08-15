# Agent reference: Building

All binaries are built into `.build/` at the repo root (gitignored).
Use the root `Makefile`:

```bash
make build        # build all binaries → .build/agc, .build/gmc, .build/probe, .build/proxy
make build-agc    # build only the AGC (Actions Gateway Controller)
make build-gmc    # build only the GMC (Gateway Manager Controller)
make build-probe  # build only the probe
make build-proxy  # build only the egress proxy
```

`cmd/worker` is a workspace module but has no dedicated root-level build target — it is built into its container image only.

Individual module Makefiles (e.g.
`cmd/agc/Makefile`) also output to `.build/` via a relative path (`../../.build/`), so both `make` invocations land in the same place.

## Container images

The four production images (`cmd/{agc,gmc,proxy,worker}/Dockerfile`) are built together via [`docker-bake.hcl`](../../docker-bake.hcl) (`docker buildx bake`).
Every image is a named stage of the single root [`Dockerfile`](../../Dockerfile), selected with `--target`; they share one `deps` stage that compiles the vendored dependency tree once.
The e2e/CI image pipeline is described in [e2e-ci-speed-round-2.md](../plan/e2e-ci-speed-round-2.md), with the earlier round in [docker-image-speed.md](../plan/docker-image-speed.md).

### Go version bumps (the `GOTOOLCHAIN=local` coupling)

The official `golang` base image sets `GOTOOLCHAIN=local`, so `go build` inside an image build will **not** auto-download a newer toolchain.
Bumping the `go` directive (e.g. for a stdlib CVE) is therefore a **three-part coupled change**:

1. **The `go` directive** — `go.work`, every `go.mod` (including `tools/`), and the three `go.work.gen` files; `make go-version-check` (part of `make check`) enforces they match.
2. **The `golang` builder digest in the root [`Dockerfile`](../../Dockerfile)** — one `FROM` line, on the shared `deps` stage every image builds from.
   The pinned digest must already ship ≥ the new patch, or every image build fails with `go: /src/go.work requires go >= X (running Y; GOTOOLCHAIN=local)`.
   Dependabot bumps these digests weekly (see [dependency-updates.md](dependency-updates.md)), but a CVE-driven bump can't wait for it: resolve the current digest with `docker buildx imagetools inspect golang:1.NN` (top-level `Digest:` line) and verify the patch with `docker run --rm golang:1.NN@sha256:<digest> go version`.
3. **`vendor/modules.txt`** — it records the `go` directive of the workspace-replaced modules, so the bump drifts the committed vendor tree.
   Run `make vendor-sync` and **commit** the result: CI's `vendor-check` diffs against git HEAD, so a dirty-but-correct working tree still fails.

A green `make check` does **not** cover parts 2–3 — the vendor/tidy/notices gates are CI-only (see [testing.md](testing.md#the-make-check-pre-review-gate)).
Local `make vulncheck` genuinely re-verifies a stdlib fix: outside the image, `GOTOOLCHAIN=auto` fetches the newer toolchain, and govulncheck reads that version — confirm you see `No vulnerabilities found.`

### Multi-arch (linux/amd64 + linux/arm64)

Published images are **multi-arch** (Q97): the release pipeline ([`publish.yml`](../../.github/workflows/publish.yml)) passes `platforms: linux/amd64,linux/arm64`, producing an OCI image index whose digest is what operators pin.
All five Dockerfiles (the four production images plus `test/fakegithub`) plumb the target platform the same way:

- The Go **builder stage** runs `FROM --platform=$BUILDPLATFORM` — always the build host's native platform — and **cross-compiles** with `GOOS=$TARGETOS GOARCH=$TARGETARCH`.
  BuildKit populates both args per target platform, so no QEMU emulation is needed for the build.
- The **runtime stages** contain no `RUN` (only `COPY`/`ENV`/`LABEL`/`USER`), so assembling the foreign-arch image needs no emulation either.
  All pinned base digests (`golang`, `distroless/static`, `actions-runner`) are multi-arch index digests covering both platforms.

Local builds (`docker buildx bake`, plain `docker build`) stay **single-platform, targeting the host arch** — on an Apple Silicon machine the bake produces arm64 images that run natively in a local kind cluster.
To cross-build one image explicitly:

```bash
docker buildx build --platform linux/arm64 --target agc .
```

Cross-compilation does not affect reproducibility: `-trimpath -ldflags=-buildid=` keeps each architecture's binary bit-for-bit reproducible regardless of the build host.
(The *index* digest depends on both child manifests plus BuildKit's provenance attestation — the reproducibility claim is about the binaries and layers, not registry manifest bytes.)

### Runner-version pin (lockstep)

The actions/runner version is pinned in **one** place — `RunnerVersion` (plus `WorkerImageDigest`) in [`cmd/agc/names/names.go`](../../cmd/agc/names/names.go).
That single constant drives, and must stay in lockstep with, three consumers:

- the AGC's `DefaultWorkerImage` (the worker pod's runner binary),
- the `GITHUB_RUNNER_VERSION` the GMC injects into the AGC Deployment — forwarded as `agent.version` on `CreateSession`, which **GitHub validates** at session creation (an empty or wrong value risks rejection), and
- the `FROM` tag on the root [`Dockerfile`](../../Dockerfile)'s `worker` stage.

Dependabot bumps only the Dockerfile, so the lockstep test [`cmd/agc/names/runner_version_test.go`](../../cmd/agc/names/runner_version_test.go) fails CI (in the unit tier) when the `FROM` tag/digest and the constants disagree.
To bump the version, follow the procedure in the Dockerfile header: update the `FROM` line **and** `RunnerVersion` + `WorkerImageDigest` together; `DefaultWorkerImage` and `GITHUB_RUNNER_VERSION` then follow automatically.

### License attribution (`/licenses/`)

Each production image `COPY`s three license files into `/licenses/` — the Red Hat/OpenShift container-certification convention, paired with the `org.opencontainers.image.licenses="Apache-2.0"` label every image carries:

- `LICENSE` / `NOTICE` — the project's own Apache-2.0 license and notice.
- `THIRD-PARTY-NOTICES` — the aggregated license/notice texts of the third-party Go modules statically linked into the binary.
  This satisfies the reproduce-the-notice clauses those licenses impose on a redistribution, and a container image is a redistribution (Apache-2.0 §4(d), MIT/BSD).

`THIRD-PARTY-NOTICES` is a **generated, committed** file at the repo root.
It is assembled from the committed, version-pinned `vendor/` tree (offline — no network or module cache) by [`scripts/release/gen-third-party-notices.sh`](../../scripts/release/gen-third-party-notices.sh):

```bash
make third-party-notices         # regenerate THIRD-PARTY-NOTICES from vendor/
make third-party-notices-check   # fail if it is stale (the CI drift gate)
```

**Regenerate it whenever the dependency set changes** (a module added, removed, or bumped — i.e. any `go mod vendor` that touches `vendor/`) and commit the result.
The `license-notices` CI workflow runs `make third-party-notices-check` on every `vendor/**` change and fails the PR if the committed file is stale, so the image build always bundles current attribution.
The operator-facing view of what ships at `/licenses/` is in [security-operations.md](../operations/security-operations.md#license-attribution-in-images).

The test-only `test/fakegithub` image is not published as a product artifact and does not carry `/licenses/`.

#### What it covers — and why build-time tooling does not

The obligation is triggered by **distribution, not by use**.
MIT ("in all copies or substantial portions"), BSD-3 ("redistributions in binary form must reproduce …"), and Apache-2.0 §4(d) all attach to conveying the work to someone else.
Code that only ever runs on a developer's machine or in CI is never conveyed, so it owes no attribution.

So the file is assembled from the repo-root `vendor/` tree **only** — the modules statically linked into the shipped binaries.
The other two vendored trees are deliberately excluded:

| Tree | In `THIRD-PARTY-NOTICES`? | Why |
|---|---|---|
| `vendor/` | yes | statically linked into the published images |
| `tools/vendor/` | no | pinned third-party build tools (`make tools`); never in an artifact |
| `devtools/vendor/` | no | first-party programs backing `make` gates; never shipped or signed |

Adding a dependency to `tools/` or `devtools/` therefore owes no notices regen.
Adding one to a workspace module does.

**The source tree is its own distribution, and it is already satisfied.** `vendor/` is committed and this repo is public, so the third-party *source* is redistributed too — but each vendored module keeps its own `LICENSE` beside its code, which is exactly what the reproduce-the-notice clauses ask for.
The roll-up file exists because a **binary** strips that: the image carries no `vendor/` tree, so the texts have to travel separately at `/licenses/`.

**Notices scope is not SBOM scope.** The SBOM answers "what touched this build" for vulnerability tracking, and may legitimately inventory build-time tooling.
`THIRD-PARTY-NOTICES` answers "what did we distribute" and is a legal artifact.
Keep the two scoped separately — widening one does not widen the other.

### Image hardening

A few build-time hardening conventions are shared by every Dockerfile and gated by the [`dockerfile-lint` CI workflow](testing.md#the-dockerfile-lint-gate) (`hadolint` at the strictest `style` threshold):

- **Explicit non-root `USER`.** Each final stage pins its runtime user even though the base already defaults to one — `USER 65532:65532` on the distroless images, `USER runner` on the actions-runner-based worker — so the non-root guarantee is local to the Dockerfile and survives a base-image/tag change.
  The worker keeps the user **by name** (not a numeric UID) on purpose; see [security §5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in) for why the GMC supplies the numeric `runAsUser: 1001` that lets kubelet verify it. hadolint 2.15.0 added `DL3066`, which wants the numeric form, so each by-name `USER` carries a `# hadolint ignore=DL3066` pragma stating that reason.
  The pragma is scoped to the instruction directly below it, so a new by-name `USER` elsewhere still fails the gate.
- **Digest-pinned BuildKit frontend.** The `# syntax=docker/dockerfile:1.7` directive carries an `@sha256:…` digest.
  Dependabot's docker ecosystem does **not** bump syntax directives, so re-pin it manually when bumping the tag: `docker buildx imagetools inspect docker/dockerfile:1.7`.
- **Digest-pinned bases**, multi-stage builds onto a minimal/distroless runtime, `CGO_ENABLED=0` static binaries, and reproducible-build flags — covered above and in [security §threat-model](../design/05-security.md).
