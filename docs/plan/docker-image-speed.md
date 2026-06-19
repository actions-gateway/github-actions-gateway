# Docker Image Speed Improvements

This document analyses where time is spent building and loading the five
Docker images in this repo (`gmc`, `agc`, `proxy`, `worker`, `fakegithub`) and
describes concrete improvements in order of estimated impact. §1–§9 target
the **build** phase; §12–§13 target the **load-into-kind** phase used by the
e2e suite. Each section covers motivation, implementation steps, files
affected, and estimated savings.

Items that were obsoleted by other decisions (§3 and §6 by workspace
vendoring; §10 and §11 by the in-cluster registry in §13) have been removed
from this document — see commit history for the original plans.

Items deliberately not pursued are noted with the rationale so the decision
is not revisited inadvertently.

## Table of Contents

- [Status](#status)
- [Background — where time goes today](#background--where-time-goes-today)
- [1. Root .dockerignore](#1-root-dockerignore)
- [2. Compile-cache mount on /root/.cache/go-build ✓](#2-compile-cache-mount-on-rootcachego-build-)
- [4. Drop the go work sync no-op ✓](#4-drop-the-go-work-sync-no-op-)
- [5. Build CI images in parallel ✓](#5-build-ci-images-in-parallel-)
- [7. Switch builder base to golang:1.26-alpine — **Not doing**](#7-switch-builder-base-to-golang126-alpine--not-doing)
- [8. Pin base images by digest ✓](#8-pin-base-images-by-digest-)
- [9. Skip image rebuilds when nothing relevant changed ✓](#9-skip-image-rebuilds-when-nothing-relevant-changed-)
- [Background — why kind load docker-image was slow](#background--why-kind-load-docker-image-was-slow)
- [12. Single-node CI cluster — **Not doing**](#12-single-node-ci-cluster--not-doing)
- [13. Replace kind load docker-image with an in-cluster registry ✓](#13-replace-kind-load-docker-image-with-an-in-cluster-registry-)
- [Final status](#final-status)

## Status

| # | Change | Status |
|---|---|---|
| 1  | Root `.dockerignore` | ✅ Done |
| 2  | Compile-cache mount | ✅ Done (lite — `/root/.cache/go-build` only; module-cache half mooted by vendoring) |
| 4  | Drop `go work sync` no-op | ✅ Done |
| 5  | Parallel CI builds (bake) | ✅ Done |
| 7  | Alpine builder base | 🚫 Not doing — musl/glibc differences introduce build complexity and a latent error risk that outweighs the 15–30 s savings. All builder stages use the standard Debian-based `golang` image. |
| 8  | Pin bases by digest | ✅ Done — `golang:1.26`, `gcr.io/distroless/static:nonroot`, and `golang:1.24` pinned in all five Dockerfiles. `ghcr.io/actions/runner` deferred until GHCR auth is available. |
| 9  | Path-based image skip in CI | ✅ Done — `changes` job gates the `e2e` job on e2e-relevant file changes; pushes to `main` always run. Note: bake still builds all four images (GHA layer cache makes cache-hits fast); the skip is at the job level, not per bake target, since all four images must be present in the ephemeral local registry for e2e to run. |
| 12 | Single-node CI cluster | 🚫 Not doing — the project has committed to a multi-node e2e suite to validate multi-tenancy under realistic scheduling conditions. Splitting into single-node and multi-node suites adds CI maintenance overhead that is not justified by the 30–60 s `kind create` saving. |
| 13 | In-cluster registry | ✅ Done |

Plus, as an extra-plan change: **Go workspace vendoring** (`go work vendor`
produces a single `vendor/` at the repo root, committed to git). Every
Dockerfile now does `COPY . .` + `go build` with `-mod=vendor` auto-selected,
no module-cache plumbing required. Vendor/ adds ~76 MB to the repo;
`.gitattributes` marks it `linguist-vendored` so it doesn't dominate GitHub UI
summaries.

---

## Background — where time goes today

A cold-cache run of `make e2e-images` builds four images sequentially:

| Phase | Typical duration |
|---|---|
| Send build context (`COPY . .`) ×4 | ~10–25 s |
| Pull `golang:1.26` base (~800 MB) on cold CI runner | ~30–60 s |
| `go mod download` (implicit on first `go build`) ×4 | ~60–120 s |
| `go build` (gmc, agc, fakegithub, proxy) | ~120–180 s |
| **Total (cold)** | **~4–6 min** |
| **Total (warm, current GHA cache hit)** | **~1–2 min** |

CI already enables `cache-from: type=gha` per image in
[.github/workflows/e2e-test.yml](../../.github/workflows/e2e-test.yml), so warm runs
do reuse layer blobs. The catch: BuildKit invalidates the `go build` layer the
moment any file inside `COPY . .` changes — which is every PR. With no
`--mount=type=cache` for `/go/pkg/mod` or `/root/.cache/go-build`, every
non-cached `go build` recompiles the world.

Local builds (`make e2e-images`) have no layer cache at all and rebuild from
scratch every time.

The improvements below target both paths: shrinking the build context so fewer
edits invalidate layers, and adding BuildKit cache mounts so compilation
artefacts survive cache misses.

---

## 1. Root `.dockerignore`

**Estimated savings: 5–15 s per build × 4 builds; eliminates many spurious
cache invalidations**

### Problem

Docker reads `.dockerignore` from the **context root**, not from the directory
next to the Dockerfile. The only `.dockerignore` in the repo is at
`cmd/gmc/.dockerignore`, but every Dockerfile except
proxy builds with `context: .`. That means none of those builds get a
dockerignore applied. (BuildKit also supports `<dockerfile>.dockerignore` next
to the Dockerfile, but the file would have to be named e.g.
`cmd/gmc/Dockerfile.dockerignore` — the current `.dockerignore` filename is not
matched.)

Result: the full repo ships into every build, including:

- `tools/` — 29 MB (vendored `controller-gen`, `kubebuilder`, `setup-envtest`)
- `.git/` — varies, often 50–200 MB
- `docs/` — 672 KB
- `testdata/`, `scripts/`, `broker/`, `internal/` — irrelevant to most images
- All `*_test.go` files — irrelevant to builds; ~hundreds of KB
- `.claude/`, `.build/`, IDE files — never needed

Beyond the wire transfer cost, every change to any of those files invalidates
the `COPY . .` layer and forces a full `go build` rebuild.

### Approach

Add a single root `.dockerignore` that excludes everything by default and
re-includes only Go source, module files, and the workspace files needed for
multi-module builds. Model after the existing
`cmd/gmc/.dockerignore`.

### Implementation steps

1. **Create `/.dockerignore`**:

   ```dockerignore
   # Ignore everything by default and re-include only what builds need.
   **

   # Re-include Go source (but not tests).
   !**/*.go
   **/*_test.go

   # Re-include Go module / workspace files.
   !go.mod
   !go.sum
   !go.work
   !go.work.sum
   !**/go.mod
   !**/go.sum

   # Re-include build-time inputs that aren't .go (e.g. embedded assets).
   # Add specific entries here if/when introduced; do not blanket-include
   # directories.
   ```

2. **Delete `cmd/gmc/.dockerignore`** — superseded by
   the root file, and ignored anyway when context is `.`.

3. **Add `cmd/proxy/.dockerignore`** mirroring the root file. The proxy build
   uses `context: cmd/proxy`, so the root `.dockerignore` does not apply to it.
   In particular, exclude `proxy_test.go` so test edits do not invalidate the
   build layer:

   ```dockerignore
   **
   !*.go
   *_test.go
   !go.mod
   !go.sum
   ```

### Files

- `.dockerignore` (new)
- `cmd/proxy/.dockerignore` (new)
- `cmd/gmc/.dockerignore` (delete)

---

## 2. Compile-cache mount on `/root/.cache/go-build` ✓

**Estimated savings: 30–60 s on rebuilds that invalidate the build layer**

### Problem

The `go build` step lives in its own layer; that layer's cache key depends on
every file in `COPY . .`. Any source edit invalidates the layer and forces
Go to recompile every package from scratch — including unchanged dependencies
— because the build cache lived inside the discarded layer.

### Approach

Wrap the `go build` step with a BuildKit cache mount targeting
`/root/.cache/go-build`. The mount is BuildKit-managed scratch space that is
NOT part of the layer output, so it survives across layer-cache misses.
Go's content-addressed build cache then reuses compiled `.a` files for any
package whose inputs haven't changed.

The module-cache half (`/go/pkg/mod`) from the original plan was dropped
because workspace vendoring (committed separately) eliminated the module
cache entirely — `go build` reads from `vendor/` without touching the
module cache at all.

### Implementation steps (shipped)

1. Pinned `# syntax=docker/dockerfile:1.7` at the top of each Dockerfile.
2. Added `--mount=type=cache,target=/root/.cache/go-build` to the `go build`
   step in [cmd/gmc/Dockerfile](../../cmd/gmc/Dockerfile),
   [cmd/agc/Dockerfile](../../cmd/agc/Dockerfile),
   [cmd/proxy/Dockerfile](../../cmd/proxy/Dockerfile),
   and [test/fakegithub/Dockerfile](../../test/fakegithub/Dockerfile).

### Notes

- The cache mount uses BuildKit-managed storage, separate from `cache-to=gha`.
  In CI it persists *within* a workflow run (so parallel bake targets share
  the cache), but does not survive across runs unless BuildKit state itself
  is cached.
- Inside the bake parallel run, the four targets share the mount with
  `sharing=shared` (the default), so the first build to compile each package
  populates a `.a` that the other three reuse.

### Files

- All four buildable Dockerfiles (`cmd/worker/Dockerfile` already uses
  alpine and isn't part of the e2e bake set, so it's untouched).

---

## 4. Drop the `go work sync` no-op ✓

**Estimated savings: ~1 s × 3 builds; layer-cache hygiene**

### Problem

[cmd/gmc/Dockerfile:11](../../cmd/gmc/Dockerfile),
[cmd/agc/Dockerfile:10](../../cmd/agc/Dockerfile), and
[test/fakegithub/Dockerfile:11](../../test/fakegithub/Dockerfile) all ran
`RUN go work sync 2>/dev/null || true` immediately before `COPY . .`. The next
layer overwrites the working directory, so any side-effect of `go work sync`
was thrown away. It was a no-op step that produced its own layer and added
~1 s.

### Approach

Removed the line. `go build` resolves the workspace correctly without an
explicit sync.

### Files

- `cmd/gmc/Dockerfile`
- `cmd/agc/Dockerfile`
- `test/fakegithub/Dockerfile`

---

## 5. Build CI images in parallel ✓

**Estimated savings: ~2–4 min of wall time on cold-cache CI runs (observed:
~2 min 15 s on the standard e2e job, ~2 min 40 s on multi-node)**

### Problem

The four CI images used to build sequentially inside one job via four
`docker/build-push-action@v6` steps. They have independent GHA cache scopes
(`scope=gmc`, `scope=agc`, etc.) and no inter-image dependencies, so they
could build concurrently and cut the build phase to roughly the slowest
single image.

### Approach (shipped)

`docker buildx bake` with one HCL file describing all four targets. Bake
runs them in parallel using a single BuildKit instance, so the compile-cache
mount from §2 is shared across siblings.

The implementation added a `GHA_CACHE` variable that toggles the
`type=gha` cache-from/cache-to flags so local invocations don't fail with
"ActionsRuntimeToken required" — see [docker-bake.hcl](../../docker-bake.hcl).

### Files

- [docker-bake.hcl](../../docker-bake.hcl) (new)
- [Makefile](../../Makefile) — `e2e-images` and `docker-build-*` targets call bake
- [.github/workflows/e2e-test.yml](../../.github/workflows/e2e-test.yml) and
  `.github/workflows/e2e-multi-node.yml`
  — four build-push-action steps collapsed into one bake step

---

## 7. Switch builder base to `golang:1.26-alpine` — **Not doing**

**Decision**: Alpine's musl libc introduces build-tool incompatibilities and
a latent error surface that outweighs the 15–30 s cold-pull savings. All
builder stages use the standard Debian-based `golang` image. The `cmd/worker`
builder was previously on `golang:1.24-alpine` and has been updated to the
standard `golang:1.24` as part of §8.

---

## 8. Pin base images by digest ✓

**Estimated savings: reproducibility / cache stability, not raw speed**

### Problem

`golang:1.26`, `gcr.io/distroless/static:nonroot`, and
`ghcr.io/actions/actions-runner:2.327.1` (worker base) are referenced by mutable tags.
A registry-side tag move silently busts the layer cache for every downstream
build.

### Approach (shipped)

Each base image is now pinned to its multi-arch manifest list digest
(`@sha256:...`) with a comment showing the inspect command to refresh it.
Dependabot keeps these up to date automatically.

| Image | Pinned |
|---|---|
| `golang:1.26` | ✅ — all four builder stages |
| `golang:1.24` | ✅ — worker builder stage (also switched from alpine) |
| `gcr.io/distroless/static:nonroot` | ✅ — all four runtime stages |
| `ghcr.io/actions/actions-runner:2.327.1` | ✅ — pinned 2026-06-01 to `@sha256:551dc313…` (M-19) |

### Files

- `cmd/gmc/Dockerfile`, `cmd/agc/Dockerfile`, `cmd/proxy/Dockerfile`,
  `test/fakegithub/Dockerfile`, `cmd/worker/Dockerfile`

---

## 9. Skip image rebuilds when nothing relevant changed ✓

**Estimated savings: full e2e suite time on PRs that touch only unit tests,
CI configs for other suites, scripts, etc.**

### Problem

CI ran the full e2e suite (up to 45 min) on every PR push, even when the only
changes were unit tests, CI workflow files for other suites, or other files
that do not affect any of the four built images.

### Approach (shipped)

A `changes` job runs `dorny/paths-filter` before `e2e` and emits a boolean
output `e2e` indicating whether any e2e-relevant file changed. The `e2e` job
has `needs: [changes]` and an `if:` condition:

```
if: needs.changes.outputs.e2e == 'true' || github.event_name == 'push'
```

Pushes to `main` always run. PRs that touch only CI configs for other suites,
`.claude/`, etc. skip both bake and the full test run entirely.

**Why not per-bake-target skipping?** The local registry (`127.0.0.1:5000`) is
ephemeral — created fresh each run. All four images must be present for e2e to
run, so skipping a specific bake target would leave the registry incomplete.
The GHA layer cache (`type=gha,mode=max`) already makes cache-hit builds fast
(~15 s across all four in parallel), so the marginal gain from per-target
skipping is small compared to skipping the entire 10–45 min test suite.

**Required-status-checks note**: if `e2e` is a required branch-protection
check, enable "Allow required status checks to pass when skipped" in the
repo's branch protection settings so that skipped runs count as passing.

### Files

- [.github/workflows/e2e-test.yml](../../.github/workflows/e2e-test.yml) — `changes` job + `if:` on `e2e` job

---

## Background — why `kind load docker-image` was slow

(Historical context for §12 and §13. The kind-load path itself has been
replaced by the in-cluster registry in §13; this section explains why.)

`kind load docker-image IMG --name X` did:

1. **`docker save IMG`** — re-serialise the entire image from the local
   daemon's storage into a tarball. No layer-level dedup across images; the
   distroless base was exported separately for every image.
2. **For each node in the cluster, sequentially**: pipe that tarball into
   `docker exec <node-container> ctr --namespace=k8s.io images import -`.
   Containerd inside the node container re-computed digests and wrote the
   layers into its own content store.

Multiplying by the CI cluster topology (2 nodes: 1 control-plane + 1 worker)
and image sizes (~77+61+17+11 = ~166 MB total): **4 × `docker save`** plus
**8 × `ctr import`**, all serial in one make target.

Three structural CI penalties on top of that:

1. **Slow ephemeral disk.** Every byte traversed the runner's disk multiple
   times: daemon storage → save tarball → pipe → node container filesystem →
   containerd content store.
2. **Single-threaded pipeline.** `docker save | docker exec … ctr import` is
   one pipe; gzip in the save tarball is single-threaded; `ctr import` runs
   digest verification on one CPU per call.
3. **Control-plane was loaded for nothing.** The control-plane node is tainted
   `NoSchedule`, but `kind load docker-image` still pushed every image into
   it, doubling the work in the 2-node config.

§13 (in-cluster registry) replaced all of this.

---

## 12. Single-node CI cluster — **Not doing**

**Decision**: The project has consolidated on a multi-node e2e suite (1
control-plane + 1 worker) to validate multi-tenancy under realistic scheduling
conditions — including the `E2E_GMC_ProxyPodScheduledOnWorker` assertion.
Shrinking to a single-node cluster would require either splitting the suite or
removing that coverage. Splitting into single-node and multi-node suites adds
CI maintenance overhead that is not justified by the 30–60 s `kind create`
saving.

---

## 13. Replace `kind load docker-image` with an in-cluster registry ✓

**Estimated savings: ~60–120 s on the CI image-load step; replaces the
entire `make e2e-load-images` step**

### Problem

`kind load docker-image` was a serial bytes-over-pipe operation (see Background
above): no layer dedup across images, no reuse of unchanged layers across runs,
full image serialisation on every push. Every CI run re-paid the full ~166 MB
transfer cost.

### Approach (shipped)

Adopted the "kind-with-registry" pattern documented at
<https://kind.sigs.k8s.io/docs/user/local-registry/>: a `registry:2` container
runs alongside the kind cluster on the kind docker network; each node's
containerd is configured to mirror `127.0.0.1:5000` → `kind-registry:5000`;
buildx pushes directly to the registry; pods pull on demand. (The host ref is
the literal IPv4 loopback, not `localhost`: the registry is published IPv4-only,
so a pusher that resolves `localhost` to IPv6 `[::1]` first fails intermittently.)

[scripts/kind-with-registry.sh](../../scripts/kind-with-registry.sh) handles the
whole setup idempotently. `make e2e-cluster` invokes it; the legacy `kind
load` flow was removed entirely (replaced rather than gated). Image tags now
include `127.0.0.1:5000/<name>:e2e-<sha>` so kubelet's `IfNotPresent` cache
can't serve a stale image across cluster reuse.

### Files

- [scripts/kind-with-registry.sh](../../scripts/kind-with-registry.sh) (new)
- [Makefile](../../Makefile) — `e2e-cluster` invokes the script;
  `e2e-load-images` removed; image tags include the registry prefix
- [.github/workflows/e2e-test.yml](../../.github/workflows/e2e-test.yml) and
  `.github/workflows/e2e-multi-node.yml`
  — drop the `kind load` step; setup-buildx-action uses `network=host` so
  buildx can push to the host registry
- [cmd/gmc/test/e2e/e2e_suite_test.go](../../cmd/gmc/test/e2e/e2e_suite_test.go) —
  dropped the redundant `LoadImageToKindClusterWithName` loop; flipped the
  fakegithub manifest's `imagePullPolicy: Never` → `IfNotPresent`

---

## Final status

All planned improvements are either shipped or explicitly closed.

| # | Status | Notes |
|---|---|---|
| 1 — root `.dockerignore` | ✅ Done | Highest value-per-minute |
| 4 — drop `go work sync` | ✅ Done | Trivial cleanup |
| 13 — in-cluster registry | ✅ Done | Replaced the `kind load` pipeline entirely |
| Workspace vendoring | ✅ Done | Subsumed §3, §6, and §2's module-cache half |
| 2 — compile-cache mount | ✅ Done | Lite version (build cache only) |
| 5 — parallel buildx bake | ✅ Done | Observed ~2:15 saved on the standard e2e job |
| 7 — alpine builder base | 🚫 Not doing | Complexity/error risk exceeds 15–30 s savings |
| 8 — pin bases by digest | ✅ Done | `golang:1.26`, `distroless`, `golang:1.24` pinned; runner image deferred |
| 9 — path-based e2e skip | ✅ Done | `changes` job gates full e2e suite on PR; pushes to main always run |
| 12 — single-node CI cluster | 🚫 Not doing | Committed to multi-node suite for realistic multi-tenancy validation |
